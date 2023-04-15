package registry

import (
	"bytes"
	"context"
	"fmt"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	helmregistry "helm.sh/helm/v3/pkg/registry"
	"net/http"
	"net/url"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/memory"
	"path/filepath"
	"strings"
)

func (r *registry) PrepareChart(ctx context.Context, repo string, reference string) *regError {

	elem := strings.Split(repo, "/")

	if len(elem) < 2 {
		return regErrInternal(fmt.Errorf("invalid repo length"))
	}

	path := strings.Join(elem[:len(elem)-1], "/")
	chart := elem[len(elem)-1]

	index, err := r.getIndex(path)
	if err != nil {
		return &regError{
			Status:  http.StatusNotFound,
			Code:    "NAME_UNKNOWN",
			Message: fmt.Sprintf("index file fetch error: %s", path),
		}
	}

	chartVer, err := index.Get(chart, reference)
	if err != nil {
		return &regError{
			Status:  http.StatusNotFound,
			Code:    "NOT FOUND",
			Message: fmt.Sprintf("Chart: %s version: %s not found: %v", chart, reference, err),
		}
	}
	if len(chartVer.URLs) == 0 {
		return &regError{
			Status:  http.StatusNotFound,
			Code:    "NOT FOUND",
			Message: fmt.Sprintf("Chart has no URLs"),
		}
	}
	reference = strings.TrimPrefix(chartVer.Version, "v")

	var downloadUrl string

	u, err := url.Parse(chartVer.URLs[0])
	if err != nil {
		regErrInternal(err)
	}
	if u.IsAbs() {
		downloadUrl = u.String()
	} else {
		downloadUrl = fmt.Sprintf("https://%s/%s", path, chartVer.URLs[0])
	}

	manifestData, err := r.download(downloadUrl)
	if err != nil {
		return regErrInternal(err)
	}

	packOpts := oras.PackOptions{}
	memStore := memory.New()

	configData := []byte("{}")

	desc := ocispec.Descriptor{
		MediaType: helmregistry.ConfigMediaType,
		Digest:    digest.FromBytes(configData),
		Size:      int64(len(configData)),
		Annotations: map[string]string{
			ocispec.AnnotationTitle: "$config",
		},
	}
	err = memStore.Push(ctx, desc, bytes.NewReader(configData))
	if err != nil {
		return regErrInternal(err)
	}

	desc.Annotations = packOpts.ConfigAnnotations
	packOpts.ConfigDescriptor = &desc
	packOpts.PackImageManifest = true
	name := filepath.Clean(filepath.Base(downloadUrl))

	manifestFile := ocispec.Descriptor{
		MediaType: helmregistry.ChartLayerMediaType,
		Digest:    digest.FromBytes(manifestData),
		Size:      int64(len(manifestData)),
		Annotations: map[string]string{
			ocispec.AnnotationTitle: name,
		},
	}

	err = memStore.Push(ctx, manifestFile, bytes.NewReader(manifestData))
	copyOptions := oras.DefaultCopyOptions
	copyOptions.Concurrency = 1

	root, err := oras.Pack(ctx, memStore, "", []ocispec.Descriptor{manifestFile}, packOpts)
	if err != nil {
		return regErrInternal(err)
	}
	if err = memStore.Tag(ctx, root, root.Digest.String()); err != nil {
		return regErrInternal(err)
	}

	var refs []string

	copyOptions.PreCopy = func(ctx context.Context, desc ocispec.Descriptor) error {
		if desc.MediaType == ocispec.MediaTypeImageManifest {
			// oci manifest
			for k, ref := range refs {
				desc.Annotations[fmt.Sprintf("%s%d", ProxyRefAnnotationPrefix, k)] = ref
			}
		} else {
			refs = append(refs, desc.Digest.String())
		}
		return nil
	}

	dst := NewInternalDst(fmt.Sprintf("%s/%s", path, chartVer.Name), r.Blobs.BlobHandler.(blobPutHandler), r.Manifests)
	// push
	if reference == "" {
		err = oras.CopyGraph(ctx, memStore, dst, root, copyOptions.CopyGraphOptions)
	} else {
		_, err = oras.Copy(ctx, memStore, root.Digest.String(), dst, reference, copyOptions)
	}
	if err != nil {
		return regErrInternal(err)
	}
	return nil
}
