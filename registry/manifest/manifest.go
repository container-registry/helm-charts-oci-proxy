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
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/sirupsen/logrus"
	"io"
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
	manifests   map[string]map[string]Manifest
	lock        sync.Mutex
	log         logrus.StdLogger
	cache       Cache
	blobHandler handler.BlobHandler
	config      Config
}

func NewManifests(blobHandler handler.BlobHandler, config Config, cache Cache, log logrus.StdLogger) *Manifests {
	return &Manifests{

		manifests:   map[string]map[string]Manifest{},
		blobHandler: blobHandler,
		log:         log,
		config:      config,
		cache:       cache,
	}
}

func (manifest *Manifests) Run(ctx context.Context) error {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if manifest.config.Debug {
				manifest.log.Println("cleanup cycle")
			}
			manifest.lock.Lock()
			for _, m := range manifest.manifests {
				for k, v := range m {
					if v.CreatedAt.Before(time.Now().Add(-manifest.config.CacheTTL)) {
						// delete
						delete(m, k)
						if delHandler, ok := manifest.blobHandler.(handler.BlobDeleteHandler); ok {
							for _, ref := range v.Refs {
								h, err := v1.NewHash(ref)
								if err != nil {
									manifest.log.Printf("hash err: %v", err)
									continue
								}
								if manifest.config.Debug {
									manifest.log.Printf("deleting blob %s", h.String())
								}
								if err = delHandler.Delete(ctx, "", h); err != nil {
									manifest.log.Println(err)
								}
							}
						}
					}
				}
			}
			manifest.lock.Unlock()
		case <-ctx.Done():
			return nil
		}
	}
}

// https://github.com/opencontainers/distribution-spec/blob/master/spec.md#pulling-an-image-manifest
// https://github.com/opencontainers/distribution-spec/blob/master/spec.md#pushing-an-image
func (manifest *Manifests) Handle(resp http.ResponseWriter, req *http.Request) error {
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
		manifest.lock.Lock()
		defer manifest.lock.Unlock()

		var prepared bool

		c, ok := manifest.manifests[repo]
		if !ok {
			err := manifest.prepareChart(req.Context(), repo, target)
			if err != nil {
				return err
			}
			prepared = true
			// re-find
			c = manifest.manifests[repo]
		}

		ma, ok := c[target]
		if !ok {
			if !prepared {
				err := manifest.prepareChart(req.Context(), repo, target)
				if err != nil {
					return err
				}
			}

			ma, ok = c[target]
			if !ok {
				// we failed
				return &errors.RegError{
					Status:  http.StatusNotFound,
					Code:    "NOT FOUND",
					Message: fmt.Sprintf("Chart prepare's result not found: %v, %v", repo, target),
				}
			}
		}
		rd := sha256.Sum256(ma.Blob)
		d := "sha256:" + hex.EncodeToString(rd[:])
		resp.Header().Set("Docker-Content-Digest", d)
		resp.Header().Set("Content-Type", ma.ContentType)
		resp.Header().Set("Content-Length", fmt.Sprint(len(ma.Blob)))
		resp.WriteHeader(http.StatusOK)
		_, err := io.Copy(resp, bytes.NewReader(ma.Blob))
		if err != nil {
			return errors.RegErrInternal(err)
		}
		return nil

	case http.MethodHead:
		manifest.lock.Lock()
		defer manifest.lock.Unlock()
		if _, ok := manifest.manifests[repo]; !ok {

			err := manifest.prepareChart(req.Context(), repo, target)
			if err != nil {
				return err
			}
		}
		ma, ok := manifest.manifests[repo][target]
		if !ok {
			err := manifest.prepareChart(req.Context(), repo, target)
			if err != nil {
				return err
			}
			ma, ok = manifest.manifests[repo][target]
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

func (manifest *Manifests) HandleTags(resp http.ResponseWriter, req *http.Request) error {
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
	fullRepo := strings.Join(repoParts, "/")

	if req.Method != "GET" {
		return &errors.RegError{
			Status:  http.StatusBadRequest,
			Code:    "METHOD_UNKNOWN",
			Message: "We don't understand your method + url",
		}
	}
	manifest.lock.Lock()
	defer manifest.lock.Unlock()

	c, ok := manifest.manifests[fullRepo]
	if !ok {
		err := manifest.prepareChart(req.Context(), fullRepo, "")
		if err != nil {
			return err
		}
		c, _ = manifest.manifests[fullRepo]
	}

	repoPath := strings.Join(repoParts[:len(repoParts)-1], "/")
	var tags []string

	index, _ := manifest.GetIndex(repoPath)

	if index != nil {
		if versions, ok := index.Entries[repoParts[len(repoParts)-1]]; ok {
			for _, v := range versions {
				tags = append(tags, strings.TrimLeft(v.Version, "v"))
			}
		}
	} else {
		for tag := range c {
			if !strings.Contains(tag, "sha256:") {
				tags = append(tags, tag)
			}
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
		Name: fullRepo,
		Tags: tags,
	}

	msg, _ := json.Marshal(tagsToList)
	resp.Header().Set("Content-Length", fmt.Sprint(len(msg)))
	resp.WriteHeader(http.StatusOK)
	_, err := io.Copy(resp, bytes.NewReader(msg))
	if err != nil {
		return errors.RegErrInternal(err)
	}
	return nil
}

func (manifest *Manifests) Read(repo string, name string) (Manifest, error) {

	mRepo, ok := manifest.manifests[repo]
	if !ok {
		return Manifest{}, fmt.Errorf("repository not found")
	}
	ma, ok := mRepo[name]
	if !ok {
		return Manifest{}, fmt.Errorf("manifest not found")
	}
	return ma, nil
}

func (manifest *Manifests) Write(repo string, name string, n Manifest) error {

	mRepo, ok := manifest.manifests[repo]
	if !ok {
		mRepo = map[string]Manifest{}
		manifest.manifests[repo] = mRepo
	}
	mRepo[name] = n
	return nil
}

func (manifest *Manifests) HandleCatalog(resp http.ResponseWriter, req *http.Request) error {
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
		index, _ := manifest.GetIndex(repo)
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
		manifest.lock.Lock()
		defer manifest.lock.Unlock()

		// TODO: implement pagination
		for key := range manifest.manifests {
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
	_, err := io.Copy(resp, bytes.NewReader([]byte(msg)))
	if err != nil {
		return errors.RegErrInternal(err)
	}
	return nil
}
