# Changelog

## [2.0.0](https://github.com/container-registry/helm-charts-oci-proxy/compare/chart-v1.3.1...chart-v2.0.0) (2026-09-25)


### ⚠ BREAKING CHANGES

* **chart:** ingress.annotations defaults to {} instead of the cert-manager and ingress-class annotations. Affected are releases with ingress.enabled true that relied on the default cert-manager.io/cluster-issuer and kubernetes.io/tls-acme annotations, including subchart installs. New installs get no Certificate until the annotations are set. Upgrades keep and renew their existing Certificate, but lose it once the Ingress is recreated. The ingress class is unchanged. Migration: set ingress.annotations explicitly, e.g. cert-manager.io/cluster-issuer: letsencrypt-prod and kubernetes.io/tls-acme: "true".

### Bug Fixes

* **chart:** drop opinionated ingress annotation defaults ([160e796](https://github.com/container-registry/helm-charts-oci-proxy/commit/160e796d2648baa9fc876fab0b9b65a3bfbf125f)), closes [#22](https://github.com/container-registry/helm-charts-oci-proxy/issues/22)
* **chart:** update appVersion to v1.1.0 ([#76](https://github.com/container-registry/helm-charts-oci-proxy/issues/76)) ([27ab621](https://github.com/container-registry/helm-charts-oci-proxy/commit/27ab6210a5a6a4bdeb873afafa4ef2f2269686e0))

## [1.3.1](https://github.com/container-registry/helm-charts-oci-proxy/compare/chart-v1.3.0...chart-v1.3.1) (2026-08-11)


### Bug Fixes

* **chart:** update appVersion to v1.0.0 ([#59](https://github.com/container-registry/helm-charts-oci-proxy/issues/59)) ([df641d2](https://github.com/container-registry/helm-charts-oci-proxy/commit/df641d29726c810e8121c813afbba3ac28d441fb))

## [1.3.0](https://github.com/container-registry/helm-charts-oci-proxy/compare/chart-v1.2.3...chart-v1.3.0) (2026-08-06)


### Features

* release workflow (release-please, keyless Harbor push, Artifact Hub) ([#53](https://github.com/container-registry/helm-charts-oci-proxy/issues/53)) ([2f8570c](https://github.com/container-registry/helm-charts-oci-proxy/commit/2f8570ccff4d16b7ce2c8f15f6989242782e500b))
