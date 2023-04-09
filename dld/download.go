package dld

import (
	"fmt"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/repo"
	"io"
	"net/http"
	"sigs.k8s.io/yaml"
)

func DownloadIndex(repoURLPath string) (*repo.IndexFile, error) {
	url := fmt.Sprintf("https://%s/index.yaml", repoURLPath)

	data, err := DownloadBytes(url)
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

func DownloadBytes(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}
