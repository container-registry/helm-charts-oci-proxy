// Copyright 2023 Google LLC All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"errors"
	"fmt"
	"github.com/container-registry/helm-charts-oci-proxy/internal/blobs"
	"github.com/container-registry/helm-charts-oci-proxy/internal/blobs/handler/mem"
	"github.com/container-registry/helm-charts-oci-proxy/internal/manifest"
	"github.com/container-registry/helm-charts-oci-proxy/internal/registry"
	"github.com/dgraph-io/ristretto"
	"k8s.io/utils/env"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"
)

func newCmdRegistry() *cobra.Command {
	cmd := &cobra.Command{
		Use: "registry",
	}
	cmd.AddCommand(newCmdServe())
	return cmd
}

func newCmdServe() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Serve an in-memory registry implementation",
		Long: `This sub-command serves an in-memory registry implementation on port :8080 (or $PORT)

The command blocks while the server accepts pushes and pulls.

Contents are only stored in memory, and when the process exits, pushed data is lost.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()

			l := log.New(os.Stdout, "proxy-", log.LstdFlags)

			port, err := env.GetInt("PORT", 9000)
			if err != nil {
				l.Fatalln(err)
			}

			debug, _ := env.GetBool("DEBUG", false)
			cacheTTL, _ := env.GetInt("MANIFEST_CACHE_TTL", 60)       // 1 minute
			indexCacheTTL, _ := env.GetInt("INDEX_CACHE_TTL", 3600*4) // 4 hours
			indexCacheMiB, err := env.GetInt("INDEX_CACHE_SIZE_MIB", 128)
			if err != nil {
				l.Fatalln(fmt.Errorf("INDEX_CACHE_SIZE_MIB: %w", err))
			}
			indexErrorCacheTTL, _ := env.GetInt("INDEX_ERROR_CACHE_TTL", 30) // 30 seconds
			prepareConcurrency, err := env.GetInt("PREPARE_CONCURRENCY", 4)
			if err != nil || prepareConcurrency < 1 {
				l.Fatalln(fmt.Errorf("PREPARE_CONCURRENCY must be a positive integer: %v", err))
			}

			rewriteDeps, _ := env.GetBool("REWRITE_DEPENDENCIES", false)
			proxyHost := env.GetString("PROXY_HOST", "")
			allowPrivateNetworks, _ := env.GetBool("ALLOW_PRIVATE_NETWORKS", false)

			useTLS, _ := env.GetBool("USE_TLS", false)
			certFile := env.GetString("CERT_FILE", "certs/registry.pem")
			keyfileFile := env.GetString("KEY_FILE", "certs/registry-key.pem")

			listener, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
			if err != nil {
				l.Fatalln(err)
			}

			portI := listener.Addr().(*net.TCPAddr).Port

			// DIAG_ADDR serves pprof and expvar on a separate listener; keep it off the public port.
			diagAddr := env.GetString("DIAG_ADDR", "")
			indexCache, err := newIndexCache(indexCacheMiB, diagAddr != "")
			if err != nil {
				l.Fatalln(err)
			}

			if diagAddr != "" {
				publishCacheMetrics("index_cache", indexCache)
				diagListener, err := net.Listen("tcp", diagAddr)
				if err != nil {
					l.Fatalln(err)
				}
				diag := newDiagServer(diagAddr)
				go func() {
					l.Printf("diagnostics listening on %s", diagListener.Addr())
					if err := diag.Serve(diagListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
						l.Printf("diagnostics server: %v", err)
					}
				}()
				defer diag.Close()
			}

			blobsHandler := mem.NewMemHandler()

			manifests := manifest.NewManifests(ctx, blobsHandler, manifest.Config{
				Debug:                debug,
				CacheTTL:             time.Duration(cacheTTL) * time.Second,
				IndexCacheTTL:        time.Duration(indexCacheTTL) * time.Second,
				IndexCacheMaxCost:    int64(indexCacheMiB) << 20,
				IndexErrorCacheTTl:   time.Duration(indexErrorCacheTTL) * time.Second,
				PrepareConcurrency:   prepareConcurrency,
				RewriteDependencies:  rewriteDeps,
				ProxyHost:            proxyHost,
				AllowPrivateNetworks: allowPrivateNetworks,
			}, indexCache, l)

			blobsHttpHandler := blobs.NewBlobs(manifests.Blobs(), l)
			//blobsHandler = file.NewHandler(dbLocation)
			s := &http.Server{
				ReadHeaderTimeout: 5 * time.Second, // prevent slowloris, quiet linter
				Handler: registry.New(
					manifests.Handle,
					blobsHttpHandler.Handle,
					manifests.HandleTags,
					manifests.HandleCatalog,
					registry.Debug(debug), registry.Logger(l)),
			}

			errCh := make(chan error)
			go func() {
				if useTLS {
					l.Printf("listening HTTP over TLS serving on port %d", portI)
					errCh <- s.ServeTLS(listener, certFile, keyfileFile)
				} else {
					l.Printf("listening HTTP on port %d", portI)
					errCh <- s.Serve(listener)
				}
			}()

			<-ctx.Done()
			l.Println("shutting down...")
			if err := s.Shutdown(ctx); err != nil {
				return err
			}
			if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		},
	}
}

const maxIndexCacheMiB = 1 << 20

// newIndexCache holds one parsed index per chart repository. NumCounters is 10x
// the expected key count, as ristretto recommends; 1e7 cost a fixed 48MiB.
// Entry costs come from manifest.IndexCost, which already includes overhead.
func newIndexCache(sizeMiB int, metrics bool) (*ristretto.Cache, error) {
	if sizeMiB < 1 || sizeMiB > maxIndexCacheMiB {
		return nil, fmt.Errorf("INDEX_CACHE_SIZE_MIB must be between 1 and %d, got %d", maxIndexCacheMiB, sizeMiB)
	}
	return ristretto.NewCache(&ristretto.Config{
		NumCounters:        1e4,
		MaxCost:            int64(sizeMiB) << 20,
		BufferItems:        64,
		Metrics:            metrics,
		IgnoreInternalCost: true,
	})
}
