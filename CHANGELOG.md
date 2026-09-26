# Changelog

## [1.1.2](https://github.com/container-registry/helm-charts-oci-proxy/compare/v1.1.1...v1.1.2) (2026-09-26)


### Documentation

* **deploy:** render the deployment diagram with SVGBob via Kroki ([#90](https://github.com/container-registry/helm-charts-oci-proxy/issues/90)) ([27da143](https://github.com/container-registry/helm-charts-oci-proxy/commit/27da1434f071886cf155062b0ff88a9049bbb641))

## [1.1.1](https://github.com/container-registry/helm-charts-oci-proxy/compare/v1.1.0...v1.1.1) (2026-09-25)


### Bug Fixes

* serve chart versions with build metadata by their OCI tag ([#85](https://github.com/container-registry/helm-charts-oci-proxy/issues/85)) ([e650a9f](https://github.com/container-registry/helm-charts-oci-proxy/commit/e650a9fefaa9674845ea2a9bbf78f3e2c3d96166))


### Documentation

* **readme:** standalone run, image tags, Harbor endpoint rules ([#82](https://github.com/container-registry/helm-charts-oci-proxy/issues/82)) ([ea2f065](https://github.com/container-registry/helm-charts-oci-proxy/commit/ea2f065f8d61f987774009f37b24e7ded42a87fc))

## [1.1.0](https://github.com/container-registry/helm-charts-oci-proxy/compare/v1.0.0...v1.1.0) (2026-09-22)


### Features

* **deploy:** OCI-based Flux deployment for prod ([#75](https://github.com/container-registry/helm-charts-oci-proxy/issues/75)) ([c0ac797](https://github.com/container-registry/helm-charts-oci-proxy/commit/c0ac797ea2b2d7a07b562c791751f9c2be6e9c58))


### Bug Fixes

* address open security alerts (oras-go CVEs, SSRF hardening) ([#55](https://github.com/container-registry/helm-charts-oci-proxy/issues/55)) ([6708f4b](https://github.com/container-registry/helm-charts-oci-proxy/commit/6708f4b55243dfc798895c78ae818844a46b66a1))
* **deps:** bump google.golang.org/grpc from 1.83.0 to 1.83.2 ([#70](https://github.com/container-registry/helm-charts-oci-proxy/issues/70)) ([99c3905](https://github.com/container-registry/helm-charts-oci-proxy/commit/99c3905948938962fce3c4313c510190d9496832))
* **deps:** bump the go-dependencies group across 1 directory with 3 updates ([#64](https://github.com/container-registry/helm-charts-oci-proxy/issues/64)) ([22fe848](https://github.com/container-registry/helm-charts-oci-proxy/commit/22fe84852b6384ec5980bae205fc569aa2d5e981))
* **manifest:** list tags from index for prerelease-only charts ([#73](https://github.com/container-registry/helm-charts-oci-proxy/issues/73)) ([8217cd0](https://github.com/container-registry/helm-charts-oci-proxy/commit/8217cd0aa13559a64870eef9affbb642decd7fe8))

## [1.0.0](https://github.com/container-registry/helm-charts-oci-proxy/compare/v0.1.8...v1.0.0) (2026-08-06)


### Features

* release workflow (release-please, keyless Harbor push, Artifact Hub) ([#53](https://github.com/container-registry/helm-charts-oci-proxy/issues/53)) ([2f8570c](https://github.com/container-registry/helm-charts-oci-proxy/commit/2f8570ccff4d16b7ce2c8f15f6989242782e500b))
