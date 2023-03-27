// Copyright 2018 Google LLC All Rights Reserved.
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

// Package registry implements a docker V2 registry and the OCI distribution specification.
//
// It is designed to be used anywhere a low dependency container registry is needed, with an
// initial focus on tests.
//
// Its goal is to be standards compliant and its strictness will increase over time.
//
// This is currently a low flightmiles system. It's likely quite safe to use in tests; If you're using it
// in production, please let us know how and send us CL's for integration tests.
package registry

import (
	"context"
	"encoding/json"
	"fmt"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"io"
	"log"
	"net/http"
	"time"
)

type registry struct {
	log         *log.Logger
	Blobs       Blobs      `json:"blobs"`
	Manifests   *Manifests `json:"manifests"`
	debug       bool
	cacheTTLMin int
}

const (
	ProxyRefAnnotationPrefix = "com.container-registry.proxy-ref-"
)

func (r *registry) v2(resp http.ResponseWriter, req *http.Request) *regError {
	/// debug //
	if req.URL.Path == "/" || req.URL.Path == "" {
		return r.debugHandler(resp)
	}
	if isBlob(req) {
		return r.Blobs.handle(resp, req)
	}
	if isManifest(req) {
		return r.Manifests.handle(resp, req)
	}
	if isTags(req) {
		return r.Manifests.handleTags(resp, req)
	}
	if isCatalog(req) {
		return r.Manifests.handleCatalog(resp, req)
	}
	resp.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	if req.URL.Path != "/v2/" && req.URL.Path != "/v2" {
		return &regError{
			Status:  http.StatusNotFound,
			Code:    "METHOD_UNKNOWN",
			Message: fmt.Sprintf("We don't understand your URL: %s", req.URL.Path),
		}
	}
	resp.WriteHeader(200)
	return nil
}

func (r *registry) debugHandler(resp http.ResponseWriter) *regError {
	if !r.debug {
		return nil
	}

	r.Manifests.lock.Lock()
	r.Blobs.lock.Lock()

	defer r.Blobs.lock.Unlock()
	defer r.Manifests.lock.Unlock()

	response := struct {
		Manifests map[string]map[string]Manifest `json:"manifests"`
		Blobs     map[string][]byte              `json:"blobs"`
	}{
		Manifests: r.Manifests.manifests,
		Blobs:     r.Blobs.BlobHandler.Debug(),
	}
	if err := prettyEncode(response, resp); err != nil {
		return &regError{
			Status:  http.StatusBadRequest,
			Code:    "INTERNAL ERROR",
			Message: err.Error(),
		}
	}
	return nil
}

func (r *registry) root(resp http.ResponseWriter, req *http.Request) {
	if rErr := r.v2(resp, req); rErr != nil {
		r.log.Printf("%s %s %d %s %s", req.Method, req.URL, rErr.Status, rErr.Code, rErr.Message)
		_ = rErr.Write(resp)
		return
	}
	if r.debug {
		r.log.Printf("%s %s", req.Method, req.URL)
	}
}

// New returns a handler which implements the docker registry protocol.
// It should be registered at the site root.
func New(ctx context.Context, opts ...Option) http.Handler {
	r := &registry{
		log: log.Default(), //default logger
	}
	for _, o := range opts {
		o(r)
	}
	r.Blobs = Blobs{
		BlobHandler: &memHandler{m: map[string][]byte{}},
		registry:    r,
		log:         r.log,
	}

	r.Manifests = &Manifests{
		manifests: map[string]map[string]Manifest{},
		registry:  r,
		log:       r.log,
	}

	go func() {
		ticker := time.NewTicker(time.Minute * 2)
		if r.debug {
			r.log.Println("cleanup cycle")
		}
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				r.Manifests.lock.Lock()
				for _, m := range r.Manifests.manifests {
					for k, v := range m {
						if v.CreatedAt.Before(time.Now().Add(-time.Minute * time.Duration(r.cacheTTLMin))) {
							// delete
							delete(m, k)
							if delHandler, ok := r.Blobs.BlobHandler.(blobDeleteHandler); ok {
								for _, ref := range v.Refs {
									h, err := v1.NewHash(ref)
									if err != nil {
										continue
									}
									_ = delHandler.Delete(ctx, "", h)
								}
							}
						}
					}
				}
				r.Manifests.lock.Unlock()
			case <-ctx.Done():
				return
			}
		}
	}()

	return http.HandlerFunc(r.root)
}

// Option describes the available options
// for creating the registry.
type Option func(r *registry)

// Logger overrides the logger used to record requests to the registry.
func Logger(l *log.Logger) Option {
	return func(r *registry) {
		r.log = l
	}
}

func Debug(v bool) Option {
	return func(r *registry) {
		r.debug = v
	}
}

func CacheTTLMin(v int) Option {
	return func(r *registry) {
		r.cacheTTLMin = v
	}
}

func prettyEncode(data interface{}, out io.Writer) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "    ")
	if err := enc.Encode(data); err != nil {
		return err
	}
	return nil
}
