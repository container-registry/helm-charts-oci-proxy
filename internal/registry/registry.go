package registry

import (
	"encoding/json"
	"fmt"
	"github.com/container-registry/helm-charts-oci-proxy/internal/errors"
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
	blobs Handler
	//
	manifests Handler
	tags      Handler
	catalog   Handler

	debug bool
}

func (r *Registry) v2(resp http.ResponseWriter, req *http.Request) error {
	/// debug //
	if req.URL.Path == "/" || req.URL.Path == "" {
		return r.homeHandler(resp, req)
	}
	if req.URL.Path == "/api/version" {
		return r.versionHandler(resp)
	}
	if req.URL.Path == "/api/systeminfo" || req.URL.Path == "/api/v2.0/systeminfo" {
		return r.harborInfoHandler(resp)
	}
	// Routes by the trailing segments only, so a prefix before /v2/, as Harbor's
	// proxy cache sends, is routed the same way.
	elem := strings.Split(req.URL.Path, "/")
	n := len(elem)
	b := strings.Split(strings.TrimSuffix(req.URL.Path, "/"), "/")
	nb := len(b)
	switch {
	case nb >= 4 && (b[nb-2] == "blobs" || b[nb-3] == "blobs" && b[nb-2] == "uploads"):
		return r.blobs(resp, req)
	case n >= 5 && elem[n-2] == "manifests":
		return r.manifests(resp, req)
	case n >= 5 && elem[n-2] == "tags":
		return r.tags(resp, req)
	case n >= 3 && elem[n-1] == "_catalog":
		return r.catalog(resp, req)
	case strings.HasSuffix("/"+strings.Trim(req.URL.Path, "/"), "/v2"):
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
func (r *Registry) versionHandler(resp http.ResponseWriter) error {
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
		return errors.RegErrInternal(err)
	}
	return nil
}

func (r *Registry) homeHandler(w http.ResponseWriter, req *http.Request) error {
	http.Redirect(w, req, "https://container-registry.com/helm-charts-oci-proxy/", 302)
	return nil
}

func (r *Registry) root(resp http.ResponseWriter, req *http.Request) {
	if err := r.v2(resp, req); err != nil {
		if regErr, ok := err.(*errors.RegError); ok {
			r.log.Printf("%s %s %d %s %s", req.Method, req.URL, regErr.Status, regErr.Code, regErr.Message)
			_ = regErr.Write(resp)
		} else {
			http.Error(resp, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	if r.debug {
		r.log.Printf("%s - %s", req.Method, req.URL)
	}
}

// New returns a handler which implements the docker registry protocol.
// It should be registered at the site root.
func New(manifests Handler, blobs Handler, tags Handler, catalog Handler, opts ...Option) http.Handler {
	r := &Registry{
		manifests: manifests,
		blobs:     blobs,
		tags:      tags,
		catalog:   catalog,
	}
	for _, o := range opts {
		o(r)
	}
	if r.log == nil {
		r.log = log.Default()
	}
	return http.HandlerFunc(r.root)
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

func prettyEncode(data interface{}, out io.Writer) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "    ")
	if err := enc.Encode(data); err != nil {
		return err
	}
	return nil
}
