package manifest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/container-registry/helm-charts-oci-proxy/registry/blobs/handler"
	"github.com/container-registry/helm-charts-oci-proxy/registry/errors"
	"github.com/dgraph-io/ristretto"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Catalog struct {
	Repos []string `json:"repositories"`
}

type listTags struct {
	Name string   `json:"name"`
	Tags []string `json:"tags"`
}

type Manifest struct {
	ContentType string    `json:"contentType"`
	Blob        []byte    `json:"blob"`
	Refs        []string  `json:"refs"` // referenced blobs digests
	CreatedAt   time.Time `json:"createdAt"`
}

type Manifests struct {
	// maps repo -> Manifest tag/digest -> Manifest
	manifests map[string]map[string]Manifest
	lock      sync.Mutex
	log       *log.Logger

	debug bool

	indexCache  *ristretto.Cache
	blobHandler handler.BlobHandler
	cacheTTLMin int
}

func NewManifests(ctx context.Context, debug bool, indexCache *ristretto.Cache, cacheTTLMin int, blobHandler handler.BlobHandler, l *log.Logger) *Manifests {
	ma := &Manifests{
		debug:       debug,
		manifests:   map[string]map[string]Manifest{},
		indexCache:  indexCache,
		blobHandler: blobHandler,
		log:         l,
		cacheTTLMin: cacheTTLMin,
	}

	go func() {
		ticker := time.NewTicker(time.Minute)
		if ma.debug {
			ma.log.Println("cleanup cycle")
		}
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				ma.lock.Lock()
				for _, m := range ma.manifests {
					for k, v := range m {
						if v.CreatedAt.Before(time.Now().Add(-time.Minute * time.Duration(ma.cacheTTLMin))) {
							// delete
							delete(m, k)
							if delHandler, ok := ma.blobHandler.(handler.BlobDeleteHandler); ok {
								for _, ref := range v.Refs {
									h, err := v1.NewHash(ref)
									if err != nil {
										continue
									}
									if ma.debug {
										l.Printf("deleting blob %s", h.String())
									}
									_ = delHandler.Delete(ctx, "", h)
								}
							}
						}
					}
				}
				ma.lock.Unlock()
			case <-ctx.Done():
				return
			}
		}
	}()

	return ma
}

// https://github.com/opencontainers/distribution-spec/blob/master/spec.md#pulling-an-image-manifest
// https://github.com/opencontainers/distribution-spec/blob/master/spec.md#pushing-an-image
func (m *Manifests) Handle(resp http.ResponseWriter, req *http.Request) *errors.RegError {
	elem := strings.Split(req.URL.Path, "/")

	if len(elem) < 3 {
		return &errors.RegError{
			Status:  http.StatusBadRequest,
			Code:    "INVALID PARAMS",
			Message: "No chart name specified",
		}
	}

	elem = elem[1:]
	target := elem[len(elem)-1]

	var repoParts []string
	for i := len(elem) - 3; i > 0; i-- {
		if elem[i] == "v2" {
			//enough
			break
		}
		repoParts = append(repoParts, elem[i])
	}
	sort.SliceStable(repoParts, func(i, j int) bool {
		//reverse
		return i > j
	})
	repo := strings.Join(repoParts, "/")

	switch req.Method {
	case http.MethodGet:
		m.lock.Lock()
		defer m.lock.Unlock()

		c, ok := m.manifests[repo]
		if !ok {
			err := m.prepareChart(req.Context(), repo, target)
			if err != nil {
				return err
			}
		}

		ma, ok := c[target]
		if !ok {
			err := m.prepareChart(req.Context(), repo, target)
			if err != nil {
				return err
			}
			ma, ok = c[target]
			if !ok {
				// we failed
				return &errors.RegError{
					Status:  http.StatusNotFound,
					Code:    "NOT FOUND",
					Message: "Chart prepare error",
				}
			}
		}
		rd := sha256.Sum256(ma.Blob)
		d := "sha256:" + hex.EncodeToString(rd[:])
		resp.Header().Set("Docker-Content-Digest", d)
		resp.Header().Set("Content-Type", ma.ContentType)
		resp.Header().Set("Content-Length", fmt.Sprint(len(ma.Blob)))
		resp.WriteHeader(http.StatusOK)
		io.Copy(resp, bytes.NewReader(ma.Blob))
		return nil

	case http.MethodHead:
		m.lock.Lock()
		defer m.lock.Unlock()
		if _, ok := m.manifests[repo]; !ok {

			err := m.prepareChart(req.Context(), repo, target)
			if err != nil {
				return err
			}
		}
		ma, ok := m.manifests[repo][target]
		if !ok {
			err := m.prepareChart(req.Context(), repo, target)
			if err != nil {
				return err
			}
			ma, ok = m.manifests[repo][target]
			if !ok {
				// we failed
				return &errors.RegError{
					Status:  http.StatusNotFound,
					Code:    "NOT FOUND",
					Message: "Chart prepare error",
				}
			}
		}
		rd := sha256.Sum256(ma.Blob)
		d := "sha256:" + hex.EncodeToString(rd[:])
		resp.Header().Set("Docker-Content-Digest", d)
		resp.Header().Set("Content-Type", ma.ContentType)
		resp.Header().Set("Content-Length", fmt.Sprint(len(ma.Blob)))
		resp.WriteHeader(http.StatusOK)
		return nil

	default:
		return &errors.RegError{
			Status:  http.StatusBadRequest,
			Code:    "METHOD_UNKNOWN",
			Message: "We don't understand your method + url",
		}
	}
}

func (m *Manifests) HandleTags(resp http.ResponseWriter, req *http.Request) *errors.RegError {
	elem := strings.Split(req.URL.Path, "/")
	if len(elem) < 4 {
		return &errors.RegError{
			Status:  http.StatusBadRequest,
			Code:    "INVALID PARAMS",
			Message: "No chart name specified",
		}
	}
	var repoParts []string
	for i := len(elem) - 3; i > 0; i-- {
		if elem[i] == "v2" {
			//stop
			break
		}
		repoParts = append(repoParts, elem[i])
	}
	sort.SliceStable(repoParts, func(i, j int) bool {
		//reverse
		return i > j
	})
	repo := strings.Join(repoParts, "/")

	if req.Method == "GET" {
		m.lock.Lock()
		defer m.lock.Unlock()

		c, ok := m.manifests[repo]
		if !ok {
			err := m.prepareChart(req.Context(), repo, "")
			if err != nil {
				return err
			}
			c, _ = m.manifests[repo]
		}

		var tags []string
		for tag := range c {
			if !strings.Contains(tag, "sha256:") {
				tags = append(tags, tag)
			}
		}
		sort.Strings(tags)

		// https://github.com/opencontainers/distribution-spec/blob/b505e9cc53ec499edbd9c1be32298388921bb705/detail.md#tags-paginated
		// Offset using last query parameter.
		if last := req.URL.Query().Get("last"); last != "" {
			for i, t := range tags {
				if t > last {
					tags = tags[i:]
					break
				}
			}
		}

		// Limit using n query parameter.
		if ns := req.URL.Query().Get("n"); ns != "" {
			if n, err := strconv.Atoi(ns); err != nil {
				return &errors.RegError{
					Status:  http.StatusBadRequest,
					Code:    "BAD_REQUEST",
					Message: fmt.Sprintf("parsing n: %v", err),
				}
			} else if n < len(tags) {
				tags = tags[:n]
			}
		}

		tagsToList := listTags{
			Name: repo,
			Tags: tags,
		}

		msg, _ := json.Marshal(tagsToList)
		resp.Header().Set("Content-Length", fmt.Sprint(len(msg)))
		resp.WriteHeader(http.StatusOK)
		io.Copy(resp, bytes.NewReader(msg))
		return nil
	}

	return &errors.RegError{
		Status:  http.StatusBadRequest,
		Code:    "METHOD_UNKNOWN",
		Message: "We don't understand your method + url",
	}
}

func (m *Manifests) Read(repo string, name string) (Manifest, error) {

	mRepo, ok := m.manifests[repo]
	if !ok {
		return Manifest{}, fmt.Errorf("repository not found")
	}
	ma, ok := mRepo[name]
	if !ok {
		return Manifest{}, fmt.Errorf("manifest not found")
	}
	return ma, nil
}

func (m *Manifests) Write(repo string, name string, n Manifest) error {

	mRepo, ok := m.manifests[repo]
	if !ok {
		mRepo = map[string]Manifest{}
		m.manifests[repo] = mRepo
	}
	mRepo[name] = n
	return nil
}

func (m *Manifests) HandleCatalog(resp http.ResponseWriter, req *http.Request) *errors.RegError {
	query := req.URL.Query()
	nStr := query.Get("n")
	n := 10000
	if nStr != "" {
		var err error
		n, err = strconv.Atoi(nStr)
		if err != nil {
			return errors.RegErrInternal(err)
		}
	}

	elems := strings.Split(req.URL.Path, "/")
	elems = elems[1:]

	if req.Method != "GET" {
		return &errors.RegError{
			Status:  http.StatusBadRequest,
			Code:    "METHOD_UNKNOWN",
			Message: "We don't understand your method + url",
		}
	}

	var repos []string
	countRepos := 0

	if len(elems) > 2 {
		// we have repo
		repo := strings.Join(elems[0:len(elems)-2], "/")
		index, _ := m.GetIndex(repo)
		if index != nil {
			// show index's content instead of local
			for r := range index.Entries {
				if countRepos >= n {
					break
				}
				countRepos++
				repos = append(repos, fmt.Sprintf("%s/%s", repo, r))
			}
		}

	} else {
		m.lock.Lock()
		defer m.lock.Unlock()

		// TODO: implement pagination
		for key := range m.manifests {
			if countRepos >= n {
				break
			}
			countRepos++
			repos = append(repos, key)
		}
	}

	sort.Strings(repos)
	repositoriesToList := Catalog{
		Repos: repos,
	}

	msg, _ := json.Marshal(repositoriesToList)
	resp.Header().Set("Content-Length", fmt.Sprint(len(msg)))
	resp.WriteHeader(http.StatusOK)
	io.Copy(resp, bytes.NewReader([]byte(msg)))
	return nil
}
