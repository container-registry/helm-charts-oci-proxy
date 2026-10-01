# --platform=$BUILDPLATFORM: run the compiler natively and cross-compile via
# GOOS/GOARCH; building the arm64 half under QEMU takes ~10x longer.
FROM --platform=$BUILDPLATFORM cgr.dev/chainguard/go:latest@sha256:ab5ed13913bd2d03b535ce3c3a1782bdcfef821f185697341b28e82aa5061f51 AS build
ARG VERSION=dev
ARG TARGETOS TARGETARCH
ENV CGO_ENABLED=0
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X github.com/container-registry/helm-charts-oci-proxy/internal/version.Version=${VERSION}" -o /proxy .

FROM cgr.dev/chainguard/wolfi-base:latest@sha256:d59fd2d1d21e913b12a8d56064e9aaf61f818289bd18b17132a0c4fde2358cea
ARG VERSION=dev
LABEL org.opencontainers.image.title="helm-charts-oci-proxy" \
      org.opencontainers.image.source="https://github.com/container-registry/helm-charts-oci-proxy" \
      org.opencontainers.image.description="Transparently proxies traditional Helm Chart Repositories as OCI artifacts" \
      org.opencontainers.image.licenses="AGPL-3.0-only" \
      org.opencontainers.image.version="${VERSION}"
ENV PORT=9000
ENV USE_TLS=false
COPY --from=build /proxy /proxy
USER 65534
EXPOSE 9000
CMD ["/proxy", "registry", "serve"]
