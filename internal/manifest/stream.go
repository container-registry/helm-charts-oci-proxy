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
)

var (
	chartDrivePath = regexp.MustCompile(`^[a-zA-Z]:/`)
	utf8BOM        = []byte{0xEF, 0xBB, 0xBF}
)

// readChartMetadata reads a chart archive once and returns the metadata
// loader.LoadArchive would produce, rejecting every chart it rejects. Each
// entry is checked against helm's path and size rules (loader.LoadArchiveFiles),
// and every file loader.LoadFiles parses is handed to it. Templates and other
// files LoadFiles only stores are discarded as they stream past.
func readChartMetadata(r io.Reader) (*chart.Metadata, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	defer zr.Close()

	tr := tar.NewReader(zr)
	remaining := loader.MaxDecompressedChartSize
	var parsed []*loader.BufferedFile
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
		keep := parsedByLoader(n)
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
		if keep {
			parsed = append(parsed, &loader.BufferedFile{Name: n, Data: bytes.TrimPrefix(buf.Bytes(), utf8BOM)})
		}
	}
	if files == 0 {
		return nil, stderrors.New("no files in chart archive")
	}
	ch, err := loader.LoadFiles(parsed)
	if err != nil {
		return nil, err
	}
	return ch.Metadata, nil
}

// parsedByLoader reports whether loader.LoadFiles parses the file, so that a
// broken one makes helm reject the chart.
func parsedByLoader(name string) bool {
	switch name {
	case "Chart.yaml", "Chart.lock", "values.yaml", "requirements.yaml", "requirements.lock":
		return true
	}
	return strings.HasPrefix(name, "charts/")
}
