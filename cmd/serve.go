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
	"github.com/container-registry/helm-charts-oci-proxy/registry/registry"
	"github.com/dgraph-io/badger/v3"
	"github.com/dgraph-io/badger/v3/options"
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
			cacheTTLMin, _ := env.GetInt("CACHE_TTL_MIN", 15)
			useTLS, _ := env.GetBool("USE_TLS", false)
			certFile := env.GetString("CERT_FILE", "certs/registry.pem")
			keyfileFile := env.GetString("KEY_FILE", "certs/registry-key.pem")
			dbLocation := env.GetString("DB_LOCATION", "/var/data")

			listener, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
			if err != nil {
				l.Fatalln(err)
			}

			portI := listener.Addr().(*net.TCPAddr).Port

			indexCache, err := ristretto.NewCache(&ristretto.Config{
				NumCounters: 1e7,       // number of keys to track frequency of (10M).
				MaxCost:     100000000, // maximum cost of cache (1GB).
				BufferItems: 64,        // number of keys per Get buffer.
			})
			if err != nil {
				l.Fatalln(err)
			}

			db, err := badger.Open(badger.DefaultOptions(dbLocation).
				WithCompression(options.None).
				WithBloomFalsePositive(0).
				WithMemTableSize(1024 * 1204 * 8), //8mb instead of 64
			)
			if err != nil {
				log.Fatal(err)
			}
			defer db.Close()

			s := &http.Server{
				ReadHeaderTimeout: 5 * time.Second, // prevent slowloris, quiet linter
				Handler: registry.New(ctx,
					registry.Debug(debug),
					registry.CacheTTLMin(cacheTTLMin),
					registry.Logger(l),
					registry.IndexCache(indexCache),
					registry.Badger(db),
				),
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
