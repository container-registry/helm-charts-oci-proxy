package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/dgraph-io/ristretto"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/repo"
	"io"
	"log"
	"net/http"
	"sigs.k8s.io/yaml"
	"time"
)

type registry struct {
	log         *log.Logger
	Blobs       Blobs      `json:"blobs"`
	Manifests   *Manifests `json:"manifests"`
	debug       bool
	cacheTTLMin int
	indexCache  *ristretto.Cache
}

const (
	ProxyRefAnnotationPrefix = "com.container-registry.proxy-ref-"
)

func (r *registry) v2(resp http.ResponseWriter, req *http.Request) *regError {
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
	if isV2(req) {
		resp.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
		resp.WriteHeader(200)
		return nil
	}

	return &regError{
		Status:  http.StatusNotFound,
		Code:    "METHOD_UNKNOWN",
		Message: fmt.Sprintf("We don't understand your URL: %s", req.URL.Path),
	}
}

// api/version
func (r *registry) versionHandler(resp http.ResponseWriter) *regError {
	res := struct {
		Version string `json:"version"`
	}{
		Version: "v2.0",
	}
	resp.WriteHeader(200)
	if err := prettyEncode(res, resp); err != nil {
		return regErrInternal(err)
	}
	return nil
}

// api/v2.0/systeminfo
func (r *registry) harborInfoHandler(resp http.ResponseWriter) *regError {
	res := struct {
		HarborVersion string    `json:"harbor_version"`
		CurrentTime   time.Time `json:"current_time"`
	}{
		HarborVersion: "v2.7.0-864aca34",
		CurrentTime:   time.Now(),
	}
	resp.WriteHeader(200)
	if err := prettyEncode(res, resp); err != nil {
		return regErrInternal(err)
	}
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
	_ = prettyEncode(response, resp)
	return nil
}

func (r *registry) root(resp http.ResponseWriter, req *http.Request) {
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

func IndexCache(c *ristretto.Cache) Option {
	return func(r *registry) {
		r.indexCache = c
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

func (r *registry) getIndex(repoURLPath string) (*repo.IndexFile, error) {

	type cacheResp struct {
		c   *repo.IndexFile
		err error
	}

	if r.indexCache == nil {
		return r.downloadIndex(repoURLPath)
	}

	c, ok := r.indexCache.Get(repoURLPath)

	if !ok || c == nil {
		// nothing in the cache
		res := &cacheResp{}
		res.c, res.err = r.downloadIndex(repoURLPath)
		if res.err != nil {
			r.indexCache.SetWithTTL(repoURLPath, res, 10, time.Second*5)
		} else {
			r.indexCache.Set(repoURLPath, res, 10)
		}
		return res.c, res.err
	}

	res, ok := c.(*cacheResp)
	if !ok {
		return nil, fmt.Errorf("internal error")
	}
	return res.c, res.err
}

func (r *registry) downloadIndex(repoURLPath string) (*repo.IndexFile, error) {
	url := fmt.Sprintf("https://%s/index.yaml", repoURLPath)
	if r.debug {
		r.log.Printf("download index: %s\n", url)
	}
	data, err := r.getIndexBytes(url)
	if err != nil {
		return nil, err
	}
	i := repo.NewIndexFile()

	if len(data) == 0 {
		return i, repo.ErrEmptyIndexYaml
	}
	if err = yaml.UnmarshalStrict(data, i); err != nil {
		return nil, err
	}

	for _, cvs := range i.Entries {
		for idx := len(cvs) - 1; idx >= 0; idx-- {
			if cvs[idx] == nil {
				continue
			}
			if cvs[idx].APIVersion == "" {
				cvs[idx].APIVersion = chart.APIVersionV1
			}
			if err := cvs[idx].Validate(); err != nil {
				cvs = append(cvs[:idx], cvs[idx+1:]...)
			}
		}
	}
	i.SortEntries()
	if i.APIVersion == "" {
		return i, repo.ErrNoAPIVersion
	}
	return i, nil
}

func (r *registry) getIndexBytes(url string) ([]byte, error) {
	if r.indexCache == nil {
		return r.download(url)
	}
	type cacheResp struct {
		c   []byte
		err error
	}

	c, ok := r.indexCache.Get(url)

	if !ok || c == nil {
		// nothing in the cache
		res := &cacheResp{}
		res.c, res.err = r.download(url)
		if res.err != nil {
			r.indexCache.SetWithTTL(url, res, 10, time.Second*5)
		} else {
			r.indexCache.Set(url, res, 10)
		}
		return res.c, res.err
	}

	res, ok := c.(*cacheResp)
	if !ok {
		return nil, fmt.Errorf("internal error")
	}
	return res.c, res.err

}

func (r *registry) download(url string) ([]byte, error) {
	if r.debug {
		r.log.Printf("downloading : %s\n", url)
	}
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}
