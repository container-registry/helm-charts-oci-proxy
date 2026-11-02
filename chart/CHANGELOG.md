# Changelog

## [2.0.4](https://github.com/container-registry/helm-charts-oci-proxy/compare/chart-v2.0.3...chart-v2.0.4) (2026-11-02)


### Bug Fixes

* **chart:** update appVersion to v1.2.2 ([#111](https://github.com/container-registry/helm-charts-oci-proxy/issues/111)) ([e68f8f2](https://github.com/container-registry/helm-charts-oci-proxy/commit/e68f8f26409912d399c4c6264f494d5b7d2504ee))
* **chart:** update appVersion to v2.0.0 ([#115](https://github.com/container-registry/helm-charts-oci-proxy/issues/115)) ([158bd90](https://github.com/container-registry/helm-charts-oci-proxy/commit/158bd90e7b95065708273229efe2a8c6cec07cf4))

## [2.0.3](https://github.com/container-registry/helm-charts-oci-proxy/compare/chart-v2.0.2...chart-v2.0.3) (2026-11-01)


### Bug Fixes

* **chart:** update appVersion to v1.2.1 ([#105](https://github.com/container-registry/helm-charts-oci-proxy/issues/105)) ([09eeed1](https://github.com/container-registry/helm-charts-oci-proxy/commit/09eeed1de890572f2117ad1f602da20f7f0c104e))

## [2.0.2](https://github.com/container-registry/helm-charts-oci-proxy/compare/chart-v2.0.1...chart-v2.0.2) (2026-11-01)


### Bug Fixes

* **chart:** update appVersion to v1.2.0 ([#101](https://github.com/container-registry/helm-charts-oci-proxy/issues/101)) ([40cbae0](https://github.com/container-registry/helm-charts-oci-proxy/commit/40cbae043e5c2966596467921c72a74e1fb34bfa))

## [2.0.1](https://github.com/container-registry/helm-charts-oci-proxy/compare/chart-v2.0.0...chart-v2.0.1) (2026-09-25)


### Bug Fixes

* **chart:** update appVersion to v1.1.1 ([#86](https://github.com/container-registry/helm-charts-oci-proxy/issues/86)) ([21420c1](https://github.com/container-registry/helm-charts-oci-proxy/commit/21420c1630b7b9d6fddff93683d4ec54113053b5))

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
