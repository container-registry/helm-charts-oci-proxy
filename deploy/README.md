# Production deployment (GitOps, OCI only)

Everything the cluster consumes is an OCI artifact in Harbor. Nothing is pulled from Git,
and no credentials live in the cluster: Harbor's `library` project is public, so both the
deploy artifact and the chart are pulled anonymously. Both are signed by CI and verified
by Flux before anything is applied.

```
deploy/prod/**  ──CI (publish-deploy.yml)──▶  oci://8gears.container-registry.com/library/helm-charts-oci-proxy-deploy:prod
                                                        │  OCIRepository (5m, cosign keyless)
                                                        ▼
                                              Flux Kustomization "chartproxy-prod"
                                                        │ applies
                                                        ▼
                                    Namespace + OCIRepository(chart) + HelmRelease
                                                        │  semver >=1.3.1 <2.0.0 (5m, cosign keyless)
                                                        ▼
                                    oci://8gears.container-registry.com/library/helm-charts-oci-proxy
```

Two independent update paths, both unattended:

| Change | What moves | Latency |
|---|---|---|
| New chart release (`chart-vX.Y.Z`) | The chart `OCIRepository` re-resolves the semver range, `HelmRelease` upgrades | ≤ 5 min after the chart is pushed |
| Change to `deploy/prod/**` on `main` | CI republishes the artifact, the deploy `OCIRepository` picks up the new digest | ≤ 5 min after the merge |

An app release reaches prod through the chart: `update-appversion.yml` bumps `appVersion`,
the chart release publishes it, and the `HelmRelease` picks the new chart up. The image tag
is never pinned here — see `docs/RELEASES.md`.

## Layout

- `prod/` — the desired state of the `chartproxy` namespace. This directory *is* the OCI
  artifact; the Flux `Kustomization` path is `./`.
- `bootstrap/` — the two objects in `flux-system` that point the cluster's Flux at the
  artifact. Applied once by hand; everything after that flows through the artifact.

## What the target cluster must provide

The manifests do not name a cluster. Any cluster with the following works:

| | |
|---|---|
| Flux | Installed and managed by the cluster, serving `source.toolkit.fluxcd.io/v1`, `kustomize.toolkit.fluxcd.io/v1` and `helm.toolkit.fluxcd.io/v2` (Flux 2.6 or newer) |
| Ingress | `ingress-nginx` with class `nginx` |
| TLS | cert-manager `ClusterIssuer` `letsencrypt-prod` |
| DNS | `external-dns` with the ingress source; the record is held back until cutover (below) |
| Storage | none; blobs live in memory and `persistence.enabled` stays `false` |
| Egress | Sigstore's public Fulcio, Rekor and TUF endpoints, for keyless verification |

## Signatures

Both artifacts are signed keyless by GitHub Actions and verified in-cluster through
`spec.verify.matchOIDCIdentity`, pinned to the exact workflow and ref:

| Artifact | Certificate identity |
|---|---|
| Deploy artifact | `…/.github/workflows/publish-deploy.yml@refs/heads/main` |
| Chart | `…/.github/workflows/publish-chart.yml@refs/heads/main` (reusable workflow; the certificate carries its ref, not the caller's) |

Out of band:

```bash
cosign verify --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity https://github.com/container-registry/helm-charts-oci-proxy/.github/workflows/publish-deploy.yml@refs/heads/main \
  8gears.container-registry.com/library/helm-charts-oci-proxy-deploy:prod
```

A locally pushed artifact (`task deploy:publish`) fails verification unless `spec.verify` is
removed from `bootstrap/ocirepository.yaml` for the duration of the test. An already
bootstrapped cluster keeps serving the last accepted artifact while verification fails.

## Bootstrap

Bootstrap after CI has published once, so a signed artifact exists.

```bash
export KUBECONFIG=<target cluster>           # required; the tasks never default it
task deploy:bootstrap
task deploy:status
task deploy:verify INGRESS_IP=<public ingress IP>   # probes by IP, bypassing public DNS
```

## DNS cutover

`prod/helmrelease.yaml` carries `external-dns.alpha.kubernetes.io/controller: none` on the
Ingress. external-dns skips any Ingress whose controller annotation is not `dns-controller`,
so the deployment can be brought up and verified while `chartproxy.container-registry.com`
still resolves to the current deployment.

Removing that annotation and merging to `main` is the cutover: external-dns upserts the
record within its sync interval. Issue the certificate first (cert-manager uses DNS01, so it
does not need the record to point here) and check `kubectl -n chartproxy get certificate`.

Rolling back is the reverse commit, plus the same wait for external-dns.

## Emergency controls

```bash
kubectl -n chartproxy patch helmrelease chartproxy --type merge -p '{"spec":{"suspend":true}}'
kubectl -n flux-system patch kustomization chartproxy-prod --type merge -p '{"spec":{"suspend":true}}'
```

Pinning to a known-good chart version is a one-line change to `spec.ref.semver` in
`prod/chart-ocirepository.yaml`. Every published deploy artifact also carries an immutable
`sha-<commit>` tag, so `ref.tag` in `bootstrap/ocirepository.yaml` can be pointed at a
specific build.
