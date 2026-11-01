package manifest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	stderrors "errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"sigs.k8s.io/yaml"
)

var (
	chartDrivePath = regexp.MustCompile(`^[a-zA-Z]:/`)
	utf8BOM        = []byte{0xEF, 0xBB, 0xBF}
)

// readChartMetadata reads a chart archive once and returns the metadata that
// loader.LoadArchive would produce, holding only Chart.yaml and
// requirements.yaml in memory. Every entry is checked against helm's path and
// size rules (loader.LoadArchiveFiles); content that does not feed
// chart.Metadata (loader.LoadFiles) is discarded as it streams past.
func readChartMetadata(r io.Reader) (*chart.Metadata, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	defer zr.Close()

	tr := tar.NewReader(zr)
	remaining := loader.MaxDecompressedChartSize
	var chartYAMLs, requirements [][]byte
	files := 0
	for {
		hd, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if hd.FileInfo().IsDir() {
			continue
		}
		switch hd.Typeflag {
		case tar.TypeXGlobalHeader, tar.TypeXHeader:
			continue
		}

		delimiter := "/"
		if strings.ContainsRune(hd.Name, '\\') {
			delimiter = "\\"
		}
		parts := strings.Split(hd.Name, delimiter)
		n := strings.ReplaceAll(strings.Join(parts[1:], delimiter), delimiter, "/")
		if path.IsAbs(n) {
			return nil, stderrors.New("chart illegally contains absolute paths")
		}
		n = path.Clean(n)
		if n == "." {
			return nil, fmt.Errorf("chart illegally contains content outside the base directory: %q", hd.Name)
		}
		if strings.HasPrefix(n, "..") {
			return nil, stderrors.New("chart illegally references parent directory")
		}
		if chartDrivePath.MatchString(n) {
			return nil, stderrors.New("chart contains illegally named files")
		}
		if parts[0] == "Chart.yaml" {
			return nil, stderrors.New("chart yaml not in base directory")
		}
		if hd.Size > remaining {
			return nil, fmt.Errorf("decompressed chart is larger than the maximum file size %d", loader.MaxDecompressedChartSize)
		}
		if hd.Size > loader.MaxDecompressedFileSize {
			return nil, fmt.Errorf("decompressed chart file %q is larger than the maximum file size %d", hd.Name, loader.MaxDecompressedFileSize)
		}

		var buf bytes.Buffer
		var w io.Writer = io.Discard
		keep := n == "Chart.yaml" || n == "requirements.yaml"
		if keep {
			w = &buf
		}
		written, err := io.Copy(w, io.LimitReader(tr, remaining))
		if err != nil {
			return nil, err
		}
		remaining -= written
		if written < hd.Size || remaining <= 0 {
			return nil, fmt.Errorf("decompressed chart is larger than the maximum file size %d", loader.MaxDecompressedChartSize)
		}
		files++
		if !keep {
			continue
		}
		data := bytes.TrimPrefix(buf.Bytes(), utf8BOM)
		if n == "Chart.yaml" {
			chartYAMLs = append(chartYAMLs, data)
		} else {
			requirements = append(requirements, data)
		}
	}
	if files == 0 {
		return nil, stderrors.New("no files in chart archive")
	}

	// Same order as loader.LoadFiles: every Chart.yaml first, then
	// requirements.yaml unmarshalled on top of it.
	var md *chart.Metadata
	for _, data := range chartYAMLs {
		if md == nil {
			md = new(chart.Metadata)
		}
		if err := yaml.Unmarshal(data, md); err != nil {
			return nil, fmt.Errorf("cannot load Chart.yaml: %w", err)
		}
		if md.APIVersion == "" {
			md.APIVersion = chart.APIVersionV1
		}
	}
	for _, data := range requirements {
		if md == nil {
			md = new(chart.Metadata)
		}
		if err := yaml.Unmarshal(data, md); err != nil {
			return nil, fmt.Errorf("cannot load requirements.yaml: %w", err)
		}
	}
	if md == nil {
		return nil, stderrors.New("Chart.yaml file is missing")
	}
	if err := md.Validate(); err != nil {
		return nil, err
	}
	return md, nil
}
