package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"
)

const pageSize = 60

// Package is the subset of an Artifact Hub search result the checks need.
type Package struct {
	PackageID  string `json:"package_id"`
	Name       string `json:"name"`
	Version    string `json:"version"`
	Deprecated bool   `json:"deprecated"`
	Repository struct {
		Name             string `json:"name"`
		URL              string `json:"url"`
		OrganizationName string `json:"organization_name"`
	} `json:"repository"`
}

// Discovery is cached to disk so reruns do not query Artifact Hub again.
type Discovery struct {
	FetchedAt time.Time `json:"fetched_at"`
	Query     string    `json:"query"`
	Total     int       `json:"total"`
	Packages  []Package `json:"packages"`
}

func loadOrDiscover(path string, refresh bool, c *http.Client, base string, deprecated bool) (*Discovery, bool, error) {
	if !refresh {
		raw, err := os.ReadFile(path)
		if err == nil {
			var d Discovery
			if err := json.Unmarshal(raw, &d); err != nil {
				return nil, false, fmt.Errorf("%s: %w", path, err)
			}
			if d.Query == discoveryQuery(deprecated) {
				return &d, true, nil
			}
			log.Printf("%s holds query %q, want %q; querying Artifact Hub again", path, d.Query, discoveryQuery(deprecated))
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, false, err
		}
	}
	d, err := discover(c, base, deprecated)
	if err != nil {
		return nil, false, err
	}
	raw, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return nil, false, err
	}
	return d, false, os.WriteFile(path, raw, 0o644)
}

// discover pages through the search the Artifact Hub UI runs for
// ?kind=0&cncf=true. Like the UI, it leaves out deprecated packages unless asked.
func discoveryQuery(deprecated bool) string {
	if deprecated {
		return "kind=0&cncf=true&deprecated=true"
	}
	return "kind=0&cncf=true"
}

func discover(c *http.Client, base string, deprecated bool) (*Discovery, error) {
	q := discoveryQuery(deprecated)
	d := &Discovery{FetchedAt: time.Now().UTC(), Query: q}
	seen := map[string]bool{}
	for offset := 0; ; offset += pageSize {
		u := fmt.Sprintf("%s/api/v1/packages/search?%s&limit=%d&offset=%d", base, q, pageSize, offset)
		page, total, err := searchPage(c, u)
		if err != nil {
			return nil, err
		}
		d.Total = total
		for _, p := range page {
			if !seen[p.PackageID] {
				seen[p.PackageID] = true
				d.Packages = append(d.Packages, p)
			}
		}
		if len(page) == 0 || offset+pageSize >= total {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if len(d.Packages) != d.Total {
		return nil, fmt.Errorf("artifact hub reported %d packages, pagination returned %d distinct", d.Total, len(d.Packages))
	}
	return d, nil
}

func searchPage(c *http.Client, u string) ([]Package, int, error) {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, 0, fmt.Errorf("%s: HTTP %d: %s", u, resp.StatusCode, b)
	}
	total, err := strconv.Atoi(resp.Header.Get("Pagination-Total-Count"))
	if err != nil {
		return nil, 0, fmt.Errorf("%s: Pagination-Total-Count: %w", u, err)
	}
	var page struct {
		Packages []Package `json:"packages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		return nil, 0, fmt.Errorf("%s: %w", u, err)
	}
	return page.Packages, total, nil
}
