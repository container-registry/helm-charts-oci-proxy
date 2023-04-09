package registry

import (
	"context"
	"fmt"
	"github.com/container-registry/helm-charts-oci-proxy/verify"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"io"
	"strings"
	"time"
)

const (
	MediaTypeManifestList = "application/vnd.docker.distribution.manifest.list.v2+json"
	MediaTypeManifest     = "application/vnd.docker.distribution.manifest.v2+json"
)

var defaultManifestMediaTypes = []string{
	MediaTypeManifest,
	MediaTypeManifestList,
	ocispec.MediaTypeImageManifest,
	ocispec.MediaTypeImageIndex,
	ocispec.MediaTypeArtifactManifest,
}

type InternalDst struct {
	repo           string
	blobPutHandler blobPutHandler
	manifests      *Manifests
}

func NewInternalDst(repo string, blobPutHandler blobPutHandler, manifests *Manifests) *InternalDst {
	return &InternalDst{repo: repo, blobPutHandler: blobPutHandler, manifests: manifests}
}

func (f *InternalDst) Tag(ctx context.Context, desc ocispec.Descriptor, reference string) error {

	h, err := v1.NewHash(desc.Digest.String())
	if err != nil {
		return err
	}

	m, ok := f.manifests.manifests[f.repo]
	if !ok {
		return fmt.Errorf("repository not found")
	}
	manifest, ok := m[h.String()]
	if !ok {
		return fmt.Errorf("target manifest not found")
	}
	f.manifests.manifests[f.repo][reference] = manifest
	return nil
}

func (f *InternalDst) Resolve(ctx context.Context, reference string) (ocispec.Descriptor, error) {
	// not implemented
	return ocispec.Descriptor{}, nil
}

func (f *InternalDst) Fetch(ctx context.Context, target ocispec.Descriptor) (io.ReadCloser, error) {
	// not implemented
	return nil, nil
}

func (f *InternalDst) Exists(ctx context.Context, target ocispec.Descriptor) (bool, error) {
	// always does not exist
	return false, nil
}

// Push no need lock
func (f *InternalDst) Push(ctx context.Context, expected ocispec.Descriptor, content io.Reader) error {
	h, err := v1.NewHash(expected.Digest.String())
	if err != nil {
		return err
	}

	vrc, err := verify.ReadCloser(io.NopCloser(content), expected.Size, h)
	if err != nil {
		return err
	}
	defer vrc.Close()

	if isManifestDescriptor(expected) {

		repo, ok := f.manifests.manifests[f.repo]
		if !ok {
			repo = map[string]Manifest{}
			f.manifests.manifests[f.repo] = repo
		}

		binary, err := io.ReadAll(vrc)
		if err != nil {
			return err
		}

		var refs []string

		for k, a := range expected.Annotations {
			if strings.HasPrefix(k, ProxyRefAnnotationPrefix) {
				refs = append(refs, a)
			}
		}

		repo[h.String()] = Manifest{
			ContentType: expected.MediaType,
			Blob:        binary,
			Refs:        refs,
			CreatedAt:   time.Now(),
		}

		return nil
	}
	//blob
	return f.blobPutHandler.Put(ctx, "", h, vrc)
}

func isManifestDescriptor(desc ocispec.Descriptor) bool {
	for _, mediaType := range defaultManifestMediaTypes {
		if desc.MediaType == mediaType {
			return true
		}
	}
	return false
}
