package manifest

import (
	"context"
	"github.com/container-registry/helm-charts-oci-proxy/internal/blobs/handler"
	"github.com/container-registry/helm-charts-oci-proxy/pkg/verify"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"helm.sh/helm/v3/pkg/repo"
	"io"
	"strings"
	"time"
)

// getDeterministicTimestamp returns a deterministic timestamp based on chart metadata.
// This ensures that the same chart version always produces the same timestamp,
// making OCI artifacts reproducible.
func (f *InternalDst) getDeterministicTimestamp() time.Time {
	// If we have chart version information, use the shared function
	if f.chartVer != nil {
		return getDeterministicCreatedTimestamp(f.chartVer)
	}

	// Fallback to current time if no chart version available (backward compatibility)
	return time.Now()
}

const (
	ProxyRefAnnotationPrefix = "com.container-registry.proxy-ref-"
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
	blobPutHandler handler.BlobPutHandler
	manifests      *Manifests
	chartVer       *repo.ChartVersion // Added to store chart version for deterministic timestamps
}

func NewInternalDst(repo string, blobPutHandler handler.BlobPutHandler, manifests *Manifests) *InternalDst {
	return &InternalDst{repo: repo, blobPutHandler: blobPutHandler, manifests: manifests, chartVer: nil}
}

func NewInternalDstWithChartVer(repo string, blobPutHandler handler.BlobPutHandler, manifests *Manifests, chartVer *repo.ChartVersion) *InternalDst {
	return &InternalDst{repo: repo, blobPutHandler: blobPutHandler, manifests: manifests, chartVer: chartVer}
}

func (f *InternalDst) Tag(ctx context.Context, desc ocispec.Descriptor, reference string) error {

	h, err := v1.NewHash(desc.Digest.String())
	if err != nil {
		return err
	}

	hm, err := f.manifests.Read(f.repo, h.String())
	if err != nil {
		return err
	}
	return f.manifests.Write(f.repo, reference, hm)
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

		return f.manifests.Write(f.repo, h.String(), Manifest{
			ContentType: expected.MediaType,
			Blob:        binary,
			Refs:        refs,
			CreatedAt:   f.getDeterministicTimestamp(),
		})
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
