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
	"context"
	"errors"
	"fmt"
	"github.com/container-registry/helm-charts-oci-proxy/internal/blobs"
	"github.com/container-registry/helm-charts-oci-proxy/internal/blobs/handler/mem"
	"github.com/container-registry/helm-charts-oci-proxy/internal/manifest"
	"github.com/container-registry/helm-charts-oci-proxy/internal/registry"
	"github.com/dgraph-io/ristretto"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
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
		Short: "Serve Helm chart repositories as a read-only OCI registry",
		Long: `This sub-command serves Helm chart repositories as a read-only OCI registry on port :9000 (or $PORT).

The command blocks while the server answers pulls. Charts are converted on demand from their upstream
chart repositories and kept only in memory, so nothing survives a restart.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()

			l := log.New(os.Stdout, "proxy-", log.LstdFlags)

			cfg, err := loadServeConfig()
			if err != nil {
				l.Fatalln(err)
			}

			listener, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", cfg.port))
			if err != nil {
				l.Fatalln(err)
			}

			portI := listener.Addr().(*net.TCPAddr).Port

			// DIAG_ADDR serves pprof and expvar on a separate listener; keep it off the public port.
			indexCache, err := newIndexCache(cfg.indexCacheMiB, cfg.diagAddr != "")
			if err != nil {
				l.Fatalln(err)
			}

			if cfg.diagAddr != "" {
				publishCacheMetrics("index_cache", indexCache)
				diagListener, err := net.Listen("tcp", cfg.diagAddr)
				if err != nil {
					l.Fatalln(err)
				}
				diag := newDiagServer(cfg.diagAddr)
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
				Debug:                cfg.debug,
				CacheTTL:             cfg.manifestCacheTTL,
				IndexCacheTTL:        cfg.indexCacheTTL,
				IndexCacheMaxCost:    int64(cfg.indexCacheMiB) << 20,
				IndexErrorCacheTTL:   cfg.indexErrorCacheTTL,
				PrepareConcurrency:   cfg.prepareConcurrency,
				RewriteDependencies:  cfg.rewriteDeps,
				ProxyHost:            cfg.proxyHost,
				AllowPrivateNetworks: cfg.allowPrivateNetworks,
			}, indexCache, l)

			blobsHttpHandler := blobs.NewBlobs(manifests.Blobs(), l)
			s := &http.Server{
				ReadHeaderTimeout: 5 * time.Second, // prevent slowloris, quiet linter
				Handler: registry.New(
					manifests.Handle,
					blobsHttpHandler.Handle,
					manifests.HandleTags,
					manifests.HandleCatalog,
					registry.Debug(cfg.debug), registry.Logger(l)),
			}

			return runServer(ctx, s, func() error {
				if cfg.useTLS {
					l.Printf("listening HTTP over TLS serving on port %d", portI)
					return s.ServeTLS(listener, cfg.certFile, cfg.keyFile)
				}
				l.Printf("listening HTTP on port %d", portI)
				return s.Serve(listener)
			}, cfg.shutdownTimeout, l)
		},
	}
}

// runServer runs serve until ctx is cancelled, then lets in-flight requests
// finish for at most timeout. ctx is already cancelled at that point, so the
// drain needs its own deadline. Requests still running after it, such as ones
// waiting on a detached chart preparation, are cut off; that is a normal stop,
// not a failure, so a rollout does not end with a non-zero exit.
func runServer(ctx context.Context, s *http.Server, serve func() error, timeout time.Duration, l *log.Logger) error {
	errCh := make(chan error, 1)
	go func() { errCh <- serve() }()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	l.Println("shutting down...")
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	if err := s.Shutdown(shutdownCtx); err != nil {
		if !errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("shutdown: %w", err)
		}
		l.Printf("requests still running after %s, closing their connections", timeout)
		_ = s.Close()
	}
	if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

const maxIndexCacheMiB = 1 << 20

type serveConfig struct {
	port                 int
	debug                bool
	manifestCacheTTL     time.Duration
	indexCacheTTL        time.Duration
	indexCacheMiB        int
	indexErrorCacheTTL   time.Duration
	prepareConcurrency   int
	rewriteDeps          bool
	proxyHost            string
	allowPrivateNetworks bool
	useTLS               bool
	certFile             string
	keyFile              string
	shutdownTimeout      time.Duration
	diagAddr             string
}

// loadServeConfig reads the environment and reports every invalid value, so a
// typo fails the start instead of silently selecting a default.
func loadServeConfig() (serveConfig, error) {
	var r envReader
	c := serveConfig{
		port:                 r.int("PORT", 9000, 1, 65535),
		debug:                r.bool("DEBUG", false),
		manifestCacheTTL:     r.seconds("MANIFEST_CACHE_TTL", 60, 1),
		indexCacheTTL:        r.seconds("INDEX_CACHE_TTL", 4*3600, 1),
		indexCacheMiB:        r.int("INDEX_CACHE_SIZE_MIB", 128, 1, maxIndexCacheMiB),
		indexErrorCacheTTL:   r.seconds("INDEX_ERROR_CACHE_TTL", 30, 0),
		prepareConcurrency:   r.int("PREPARE_CONCURRENCY", manifest.DefaultPrepareConcurrency, 1, math.MaxInt),
		rewriteDeps:          r.bool("REWRITE_DEPENDENCIES", false),
		proxyHost:            r.str("PROXY_HOST", ""),
		allowPrivateNetworks: r.bool("ALLOW_PRIVATE_NETWORKS", false),
		useTLS:               r.bool("USE_TLS", false),
		certFile:             r.str("CERT_FILE", "certs/registry.pem"),
		keyFile:              r.str("KEY_FILE", "certs/registry-key.pem"),
		shutdownTimeout:      r.seconds("SHUTDOWN_TIMEOUT", 25, 1),
		diagAddr:             r.str("DIAG_ADDR", ""),
	}
	r.add(validateRewrite(c.rewriteDeps, c.proxyHost))
	return c, errors.Join(r.errs...)
}

// envReader treats a variable that is set but empty as unset, because charts
// and compose files render an unset value as "".
type envReader struct{ errs []error }

func (r *envReader) add(err error) {
	if err != nil {
		r.errs = append(r.errs, err)
	}
}

func (r *envReader) str(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func (r *envReader) int(key string, def, minimum, maximum int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		r.add(fmt.Errorf("%s: %w", key, err))
		return def
	}
	if n < minimum || n > maximum {
		r.add(fmt.Errorf("%s must be between %d and %d, got %d", key, minimum, maximum, n))
	}
	return n
}

// maxSeconds keeps n*time.Second from overflowing.
const maxSeconds = math.MaxInt64 / int64(time.Second)

func (r *envReader) seconds(key string, def, minimum int) time.Duration {
	return time.Duration(r.int(key, def, minimum, int(maxSeconds))) * time.Second
}

func (r *envReader) bool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		r.add(fmt.Errorf("%s: %w", key, err))
		return def
	}
	return b
}

// validateRewrite requires PROXY_HOST because rewritten charts are shared by
// all clients; the host must not come from a request.
func validateRewrite(rewriteDeps bool, proxyHost string) error {
	if rewriteDeps && proxyHost == "" {
		return errors.New("REWRITE_DEPENDENCIES=true requires PROXY_HOST")
	}
	if proxyHost == "" {
		return nil
	}
	// The value is pasted into oci://<PROXY_HOST>/..., so anything beyond
	// host[:port] produces a broken dependency URL.
	u, err := url.Parse("oci://" + proxyHost)
	if strings.ContainsAny(proxyHost, "/\\?#@ \t\r\n") || err != nil || u.Host != proxyHost || u.Hostname() == "" ||
		strings.HasSuffix(proxyHost, ":") || !validPort(u.Port()) {
		return fmt.Errorf("PROXY_HOST must be a bare host[:port] such as chartproxy.example.com, got %q", proxyHost)
	}
	return nil
}

// validPort reports whether p is empty or within 1..65535; url.Parse checks
// neither the range nor that a port follows the colon.
func validPort(p string) bool {
	if p == "" {
		return true
	}
	n, err := strconv.Atoi(p)
	return err == nil && n >= 1 && n <= 65535
}

// newIndexCache holds one parsed index per chart repository. NumCounters is 10x
// the expected key count, as ristretto recommends; 1e7 cost a fixed 48MiB.
// Entry costs come from manifest.IndexCost, which already includes overhead.
func newIndexCache(sizeMiB int, metrics bool) (*ristretto.Cache, error) {
	return ristretto.NewCache(&ristretto.Config{
		NumCounters:        1e4,
		MaxCost:            int64(sizeMiB) << 20,
		BufferItems:        64,
		Metrics:            metrics,
		IgnoreInternalCost: true,
	})
}
