package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/container-registry/helm-charts-oci-proxy/registry/blobs"
	"github.com/container-registry/helm-charts-oci-proxy/registry/blobs/handler/mem"
	rerrros "github.com/container-registry/helm-charts-oci-proxy/registry/errors"
	"github.com/container-registry/helm-charts-oci-proxy/registry/helper"
	"github.com/container-registry/helm-charts-oci-proxy/registry/manifest"
	"github.com/sirupsen/logrus"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

type Registry struct {
	log logrus.StdLogger

	// to operate blobs directly from registry
	blobs *blobs.Blobs
	//
	manifests *manifest.Manifests

	cache manifest.Cache

	manifestCacheTTL int
	//
	indexCacheTTL      int
	indexErrorCacheTTL int
	debug              bool

	pathPrefix string
}

func (r *Registry) path(req *http.Request) string {
	return strings.TrimPrefix(req.URL.Path, r.pathPrefix)
}

func (r *Registry) v2(resp http.ResponseWriter, req *http.Request) error {
	/// debug //
	path := r.path(req)

	if path == "/" || path == "" {
		return r.homeHandler(resp, req)
	}
	if path == "/api/version" {
		return r.versionHandler(resp)
	}
	if path == "/api/systeminfo" || path == "/api/v2.0/systeminfo" {
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
	return &rerrros.RegError{
		Status:  http.StatusNotFound,
		Code:    "METHOD_UNKNOWN",
		Message: fmt.Sprintf("We don't understand your URL: %s", path),
	}
}

// api/version
func (r *Registry) versionHandler(resp http.ResponseWriter) error {
	res := struct {
		Version string `json:"version"`
	}{
		Version: "v2.0",
	}
	resp.WriteHeader(200)
	if err := prettyEncode(res, resp); err != nil {
		return rerrros.RegErrInternal(err)
	}
	return nil
}

// api/v2.0/systeminfo
func (r *Registry) harborInfoHandler(resp http.ResponseWriter) error {
	res := struct {
		HarborVersion string    `json:"harbor_version"`
		CurrentTime   time.Time `json:"current_time"`
	}{
		HarborVersion: "v2.7.0-864aca34",
		CurrentTime:   time.Now(),
	}
	resp.WriteHeader(200)
	if err := prettyEncode(res, resp); err != nil {
		return rerrros.RegErrInternal(err)
	}
	return nil
}

func (r *Registry) homeHandler(w http.ResponseWriter, req *http.Request) error {
	http.Redirect(w, req, "https://container-registry.com/helm-charts-oci-proxy/", 302)
	return nil
}

func (r *Registry) Handle(resp http.ResponseWriter, req *http.Request) {
	if r.debug {
		r.log.Printf("%s - %s", req.Method, req.URL)
	}
	if err := r.v2(resp, req); err != nil {
		var regErr *rerrros.RegError
		if errors.As(err, &regErr) {
			r.log.Printf("%s %s %d %s %s", req.Method, req.URL, regErr.Status, regErr.Code, regErr.Message)
			_ = regErr.Write(resp)
			return
		}
		http.Error(resp, err.Error(), http.StatusInternalServerError)
		return
	}
}

// New returns an instance of Registry
// It should be registered at the site root.
func New(opts ...Option) *Registry {

	r := &Registry{
		manifestCacheTTL:   60, // default values
		indexCacheTTL:      3600 * 4,
		indexErrorCacheTTL: 30,
	}

	for _, o := range opts {
		o(r)
	}
	if r.log == nil {
		r.log = log.Default()
	}

	if r.cache == nil {
		r.log.Fatalln("no cache initialised")
	}

	blobsHandler := mem.NewMemHandler()

	r.manifests = manifest.NewManifests(blobsHandler, manifest.Config{
		Debug:              r.debug,
		CacheTTL:           time.Duration(r.manifestCacheTTL) * time.Second,
		IndexCacheTTL:      time.Duration(r.indexCacheTTL) * time.Second,
		IndexErrorCacheTTl: time.Duration(r.indexErrorCacheTTL) * time.Second,
	}, r.cache, r.log)

	r.blobs = blobs.NewBlobs(blobsHandler, r.log)
	return r
}

func (r *Registry) Run(ctx context.Context) error {
	return r.manifests.Run(ctx)
}

// Option describes the available options
// for creating the registry.
type Option func(r *Registry)

// Logger overrides the logger used to record requests to the registry.
func Logger(l logrus.StdLogger) Option {
	return func(r *Registry) {
		r.log = l
	}
}

func Debug(v bool) Option {
	return func(r *Registry) {
		r.debug = v
	}
}

func ManifestCacheTTL(v int) Option {
	return func(r *Registry) {
		r.manifestCacheTTL = v
	}
}

func IndexErrorCacheTTL(v int) Option {
	return func(r *Registry) {
		r.indexErrorCacheTTL = v
	}
}

func Cache(c manifest.Cache) Option {
	return func(r *Registry) {
		r.cache = c
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
