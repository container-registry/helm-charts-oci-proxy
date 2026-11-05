# --platform=$BUILDPLATFORM: run the compiler natively and cross-compile via
# GOOS/GOARCH; building the arm64 half under QEMU takes ~10x longer.
FROM --platform=$BUILDPLATFORM cgr.dev/chainguard/go:latest@sha256:55c9a5f809aca447f5427cf895ddb923ec4bce1f64accd3308840cdc7f03cbca AS build
ARG VERSION=dev
ARG TARGETOS TARGETARCH
ENV CGO_ENABLED=0
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X github.com/container-registry/helm-charts-oci-proxy/internal/version.Version=${VERSION}" -o /proxy .

FROM cgr.dev/chainguard/wolfi-base:latest@sha256:771146c2dfc880902a185f060c3db5571473c46b267edd98c3119c83068dfb61
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
