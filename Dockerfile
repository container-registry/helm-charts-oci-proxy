FROM cgr.dev/chainguard/go:latest@sha256:ae87411b2d4508b67727c73fc6236c21ca9233fc4e6bade03406b852c244eb8d AS build
ARG VERSION=dev
ENV CGO_ENABLED=0
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -trimpath -ldflags "-s -w -X github.com/container-registry/helm-charts-oci-proxy/internal/version.Version=${VERSION}" -o /proxy .

FROM cgr.dev/chainguard/wolfi-base:latest@sha256:ca263a0360cca48e8fe3f86c8af61c6d5b85e484809fe187440a4206a50efc06
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
