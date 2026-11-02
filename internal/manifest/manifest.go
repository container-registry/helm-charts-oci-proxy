package manifest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/container-registry/helm-charts-oci-proxy/internal/errors"
	"github.com/container-registry/helm-charts-oci-proxy/internal/helper"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"
)

type Catalog struct {
	Repos []string `json:"repositories"`
}

type listTags struct {
	Name string   `json:"name"`
	Tags []string `json:"tags"`
}

type Manifests struct {
	// repo -> chart version or manifest digest -> record; both keys of a
	// prepared chart version share one record.
	records     map[string]map[string]*record
	failures    int          // keys in records that hold a failure
	nextExpire  time.Time    // earliest time insert may scan for expired records
	mu          sync.RWMutex // guards records, failures and nextExpire; never held during network I/O
	prepares    singleflight.Group
	indexes     singleflight.Group
	slots       chan struct{}
	log         logrus.StdLogger
	cache       Cache
	config      Config
	httpClient  *http.Client
	layerClient *http.Client
}

func NewManifests(ctx context.Context, config Config, cache Cache, log logrus.StdLogger) *Manifests {
	concurrency := config.PrepareConcurrency
	if concurrency <= 0 {
		concurrency = DefaultPrepareConcurrency
	}
	ma := &Manifests{
		records:    map[string]map[string]*record{},
		log:        log,
		config:     config,
		cache:      cache,
		httpClient: newDownloadClient(config.AllowPrivateNetworks),
		slots:      make(chan struct{}, concurrency),
	}
	ma.layerClient = newLayerClient(ma.httpClient)

	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				ma.cleanup()
			case <-ctx.Done():
				return
			}
		}
	}()

	return ma
}

// https://github.com/opencontainers/distribution-spec/blob/master/spec.md#pulling-an-image-manifest
// https://github.com/opencontainers/distribution-spec/blob/master/spec.md#pushing-an-image
func (m *Manifests) Handle(resp http.ResponseWriter, req *http.Request) error {
	repo, _, ref, ok := helper.SplitPath(req.URL.Path)
	if !ok {
		return &errors.RegError{
			Status:  http.StatusBadRequest,
			Code:    "INVALID PARAMS",
			Message: "No chart name specified",
		}
	}
	// A chart is <index path>/<chart>; main answered every other shape with
	// this error from prepareChart.
	if !strings.Contains(repo, "/") {
		return errors.RegErrInternal(fmt.Errorf("invalid repo length"))
	}
	// Cached manifests are keyed by the index version, so the tag form is
	// normalised once here and never looked up again.
	target := ManifestTarget(ref)

	switch req.Method {
	case http.MethodGet, http.MethodHead:
		r, err := m.manifest(req.Context(), repo, target)
		if err != nil {
			return err
		}
		resp.Header().Set("Docker-Content-Digest", r.digest)
		resp.Header().Set("Content-Type", ocispec.MediaTypeImageManifest)
		resp.Header().Set("Content-Length", fmt.Sprint(len(r.manifest)))
		resp.WriteHeader(http.StatusOK)
		if req.Method == http.MethodHead {
			return nil
		}
		if _, err := resp.Write(r.manifest); err != nil {
			return errors.RegErrInternal(err)
		}
		return nil

	default:
		return &errors.RegError{
			Status:  http.StatusBadRequest,
			Code:    "METHOD_UNKNOWN",
			Message: "We don't understand your method + url",
		}
	}
}

func (m *Manifests) HandleTags(resp http.ResponseWriter, req *http.Request) error {
	fullRepo, _, _, ok := helper.SplitPath(req.URL.Path)
	// /v2/tags/list has no repository, and a chart needs a repository path.
	if !ok || !strings.Contains(fullRepo, "/") {
		return &errors.RegError{
			Status:  http.StatusBadRequest,
			Code:    "INVALID PARAMS",
			Message: "No chart name specified",
		}
	}

	if req.Method != "GET" {
		return &errors.RegError{
			Status:  http.StatusBadRequest,
			Code:    "METHOD_UNKNOWN",
			Message: "We don't understand your method + url",
		}
	}
	i := strings.LastIndexByte(fullRepo, '/')
	repoPath, chartName := fullRepo[:i], fullRepo[i+1:]

	var tags []string
	var fromIndex bool

	// The tag list is derived from the repository index alone. Downloading a chart
	// is not required here and must not make this endpoint fail, otherwise a single
	// unresolvable chart aborts a registry-wide scan (e.g. Harbor replication).
	if index, _ := m.GetIndex(repoPath); index != nil {
		if versions, ok := index.Entries[chartName]; ok {
			fromIndex = true
			for _, v := range versions {
				// Same resolution as prepareChart: versions without a convertible
				// tarball would only ever answer 404.
				if u, err := ChartURL(repoPath, v); err != nil || u.Scheme == "oci" {
					continue
				}
				tags = append(tags, helper.VersionToTag(strings.TrimLeft(v.Version, "v")))
			}
		}
	}

	if !fromIndex {
		names := m.names(fullRepo)
		if len(names) == 0 {
			if err := m.prepare(req.Context(), fullRepo, ""); err != nil {
				return err
			}
			names = m.names(fullRepo)
		}
		for _, tag := range names {
			if !strings.Contains(tag, "sha256:") {
				tags = append(tags, helper.VersionToTag(tag))
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

func (m *Manifests) HandleCatalog(resp http.ResponseWriter, req *http.Request) error {
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
		m.mu.RLock()
		defer m.mu.RUnlock()

		// TODO: implement pagination
		for key, entries := range m.records {
			if !hasManifest(entries) {
				continue
			}
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
