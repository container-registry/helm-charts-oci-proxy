package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/container-registry/helm-charts-oci-proxy/registry/blobs"
	"github.com/container-registry/helm-charts-oci-proxy/registry/blobs/handler"
	"github.com/container-registry/helm-charts-oci-proxy/registry/errors"
	"github.com/container-registry/helm-charts-oci-proxy/registry/helper"
	"github.com/container-registry/helm-charts-oci-proxy/registry/manifest"
	"github.com/dgraph-io/ristretto"
	"io"
	"log"
	"net/http"
	"time"
)

type Registry struct {
	log *log.Logger

	// to operate blobs directly from registry
	blobsHandler handler.BlobHandler
	blobs        *blobs.Blobs `json:"blobs"`
	//
	manifests  *manifest.Manifests `json:"manifests"`
	debug      bool
	cacheTTL   int
	indexCache *ristretto.Cache
}

func (r *Registry) v2(resp http.ResponseWriter, req *http.Request) *errors.RegError {
	/// debug //
	if req.URL.Path == "/" || req.URL.Path == "" {
		return r.debugHandler(resp)
	}
	if req.URL.Path == "/api/version" {
		return r.versionHandler(resp)
	}
	if req.URL.Path == "/api/systeminfo" || req.URL.Path == "/api/v2.0/systeminfo" {
		return r.harborInfoHandler(resp)
	}
	if helper.IsBlob(req) {
		return r.blobs.Handle(resp, req)
	}
	if helper.IsManifest(req) {
		return r.manifests.Handle(resp, req)
	}
	if helper.IsTags(req) {
		return r.manifests.HandleTags(resp, req)
	}
	if helper.IsCatalog(req) {
		return r.manifests.HandleCatalog(resp, req)
	}
	if helper.IsV2(req) {
		resp.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
		resp.WriteHeader(200)
		return nil
	}

	return &errors.RegError{
		Status:  http.StatusNotFound,
		Code:    "METHOD_UNKNOWN",
		Message: fmt.Sprintf("We don't understand your URL: %s", req.URL.Path),
	}
}

// api/version
func (r *Registry) versionHandler(resp http.ResponseWriter) *errors.RegError {
	res := struct {
		Version string `json:"version"`
	}{
		Version: "v2.0",
	}
	resp.WriteHeader(200)
	if err := prettyEncode(res, resp); err != nil {
		return errors.RegErrInternal(err)
	}
	return nil
}

// api/v2.0/systeminfo
func (r *Registry) harborInfoHandler(resp http.ResponseWriter) *errors.RegError {
	res := struct {
		HarborVersion string    `json:"harbor_version"`
		CurrentTime   time.Time `json:"current_time"`
	}{
		HarborVersion: "v2.7.0-864aca34",
		CurrentTime:   time.Now(),
	}
	resp.WriteHeader(200)
	if err := prettyEncode(res, resp); err != nil {
		return errors.RegErrInternal(err)
	}
	return nil
}

func (r *Registry) debugHandler(resp http.ResponseWriter) *errors.RegError {
	if !r.debug {
		return nil
	}
	//
	//r.Manifests.lock.Lock()
	//r.Blobs.lock.Lock()
	//
	//defer r.Blobs.lock.Unlock()
	//defer r.Manifests.lock.Unlock()
	//
	//response := struct {
	//	Manifests map[string]map[string]registry2.Manifest `json:"manifests"`
	//	Blobs     map[string][]byte                        `json:"blobs"`
	//}{
	//	Manifests: r.Manifests.manifests,
	//	Blobs:     r.Blobs.BlobHandler.Debug(),
	//}
	//_ = prettyEncode(response, resp)
	return nil
}

func (r *Registry) root(resp http.ResponseWriter, req *http.Request) {
	if rErr := r.v2(resp, req); rErr != nil {
		r.log.Printf("%s %s %d %s %s", req.Method, req.URL, rErr.Status, rErr.Code, rErr.Message)
		_ = rErr.Write(resp)
		return
	}
	if r.debug {
		r.log.Printf("%s - %s", req.Method, req.URL)
	}
}

// New returns a handler which implements the docker registry protocol.
// It should be registered at the site root.
func New(ctx context.Context, opts ...Option) http.Handler {
	r := &Registry{
		log: log.Default(), //default logger
	}
	for _, o := range opts {
		o(r)
	}

	r.blobs = blobs.NewBlobs(r.blobsHandler, r.log)
	r.manifests = manifest.NewManifests(ctx, r.debug, r.indexCache, r.cacheTTL, r.blobsHandler, r.log)

	return http.HandlerFunc(r.root)
}

// Option describes the available options
// for creating the registry.
type Option func(r *Registry)

// Logger overrides the logger used to record requests to the registry.
func Logger(l *log.Logger) Option {
	return func(r *Registry) {
		r.log = l
	}
}

func IndexCache(c *ristretto.Cache) Option {
	return func(r *Registry) {
		r.indexCache = c
	}
}

func BlobsHandler(bh handler.BlobHandler) Option {
	return func(r *Registry) {
		r.blobsHandler = bh
	}
}

func Debug(v bool) Option {
	return func(r *Registry) {
		r.debug = v
	}
}

func CacheTTL(v int) Option {
	return func(r *Registry) {
		r.cacheTTL = v
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
