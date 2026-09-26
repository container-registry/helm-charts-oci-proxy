# Production deployment: GitOps from signed OCI artifacts

`deploy/` is the production instance behind `chartproxy.container-registry.com`, and a
worked example of running a service from nothing but OCI artifacts:

- Git is the source of truth, but the cluster never talks to Git. CI packages the
  manifests as an OCI artifact; Flux pulls that artifact.
- The cluster holds no credentials. Registry, chart and deploy artifact are public and
  pulled anonymously. CI pushes with a short-lived GitHub OIDC token, no stored secrets.
- Nothing unsigned is applied. Both artifacts are signed keyless by GitHub Actions and Flux
  verifies the signing identity down to the workflow file and branch.
- Releases roll out without a person in the loop, and a major version bump does not.

<!--
```SVGBob
     "GitHub Actions"                                       "GitHub Actions"
     "publish-deploy.yml"                                   "publish-chart.yml"
     "push to main touching deploy/prod/**"                 "chart release chart-vX.Y.Z"
                    │                                                      │
                    │ "package + oras push"                                │ "helm push"
                    │ "cosign sign, keyless"                               │ "cosign sign, keyless"
                    ▼                                                      ▼
┌────────────────────────────────────────┐             ┌────────────────────────────────────────┐
│ "Harbor, public project library"       │             │ "Harbor, public project library"       │
│ "helm-charts-oci-proxy-deploy"         │             │ "helm-charts-oci-proxy"                │
│ ":prod (mutable)  :sha-<commit>"       │             │ "chart X.Y.Z, appVersion pins image"   │
└───────────────────┬────────────────────┘             └───────────────────┬────────────────────┘
                    │ "anonymous pull, every 5m"                           │ "anonymous pull, every 5m"
                    │ "cosign: publish-deploy.yml@main"                    │ "cosign: publish-chart.yml@main"
                    ▼                                                      │ "semver >=2.0.1 <3.0.0"
┌──────────────────────────────────────────────────────────────────────────┼───────────────────────────────────────┐
│ "Kubernetes cluster, Flux"                                               │                                       │
│                                                                          ▼                                       │
│   ┌────────────────────────────────┐                     ┌────────────────────────────────┐                      │
│   │ "OCIRepository"                │                     │ "OCIRepository"                │                      │
│   │ "chartproxy-prod"              │                     │ "helm-charts-oci-proxy"        │                      │
│   └───────────────┬────────────────┘                     └───────────────┬────────────────┘                      │
│                   │ "sourceRef"                                          │ "chartRef"                            │
│                   ▼                                                      ▼                                       │
│   ┌────────────────────────────────┐       "applies"     ┌────────────────────────────────┐                      │
│   │ "Kustomization"                ├────────────────────▶│ "HelmRelease chartproxy"       │                      │
│   │ "chartproxy-prod"              │                     │ "+ Namespace, OCIRepository"   │                      │
│   └────────────────────────────────┘                     └───────────────┬────────────────┘                      │
│                                                                          │ "helm upgrade"                        │
│                                                                          ▼                                       │
│                                                          ┌────────────────────────────────┐                      │
│                                                          │ "Deployment, 1 replica"        │                      │
│                                                          │ "Service, Ingress, Certificate"│                      │
│                                                          └────────────────────────────────┘                      │
└──────────────────────────────────────────────────────────────────────────────────────────────────────────────────┘
```
-->
![Diagram](https://kroki.io/svgbob/svg/eNrtV81u00AQvucpRnuC1klbEJeoVKAiaFUJpCAh4LZ2pvHSXa-1u45qThVnDj1EUQ88AmdOeZo8CWs7CjSJE8du4gixWsVy5PnZnfm-mQGwi7xh5ixy4aVnmAw0gWJrVqyR_RtGLmfab3Yx5DJuxYIXUTgV83yqTCo11ad9MBIEZYF9Rp7Pgh5k2g9CJbsHe3vzFkiqBxRypBohfWv2P7Y-tT5PFM-s8eAblFpWMFchCal3RXsI-yAV1ZCchRRQCMRHLrLP85V7UrNeAMmPA1cYc9Qrg5cvuNjOcFTyVoajxnjwfTy4qWnfzhy7Tlca6a2fUeVK5UCa6R7Y1P2CngHOXEVVTHLycD3RxjR5MiDppvRY035_HU8ASXIzPl-ULMr59PN2AkB4JCJDXY6PAdrap81jTwrBzMnSM2UATRHpAA3DD6i05REIWaCBCYsaMrU0qHD7P6sF727G8zpdyacCGsggFjJKKIZzB7CPKoZngqyiglzBVbTThnmif5FwNCksOKX6TO6BCSixqFHY88DJ8yetw9YRHD-1j0NSLzFtZ4_q5bqLyEUVoEENHo-0QUter3l0TUpEsXgZLl3Dq2XeH9NbSazbHCdqM33vAmzw352edzC0YDdSLawd-YAtJzrrQEosWdFL6hNZw4Hl9a-QA4PNV4W7HCdqM70EfhkRy0h52MFLsiaDp6FYJbfEeIX-dbfRT2zLxBlOGv4dQv-FpXsp2FeaTIULIPyjkhPDX1lDbGHa-Xu2u4fTLfDEPrylArWd79CBOdbaCE_c_Ns8ARX6vHRajsKeol0kO90hlD7lLuC7SohepVOCwMA4cATKvjGPrlNYqxh_j6rPEqSeBz2FWjtwisqwS-uCQbJJ47WB_AEm5_972a3_BjODq2g=)

## Layout

| Path | Content |
|---|---|
| `prod/` | Desired state of the `chartproxy` namespace: `Namespace`, the chart `OCIRepository`, the `HelmRelease`. This directory is the deploy artifact; the Flux `Kustomization` path is `./`. |
| `bootstrap/` | The `OCIRepository` and `Kustomization` in `flux-system` that point the cluster's Flux at the deploy artifact. Applied once by hand, the only manual step. |
| `../taskfiles/deploy.yml` | The `task deploy:*` commands CI and operators use: render, publish, sign, bootstrap, status, verify. |

## What runs

The `HelmRelease` installs the chart from this repository with these values:

| Setting | Value | Reason |
|---|---|---|
| `replicaCount` | 1, `Recreate` strategy | Blobs live in a per-pod in-memory store. A second pod, including a rolling-update surge pod, answers 404 for blobs whose manifest the other pod served. |
| `persistence.enabled` | `false` | Blobs never leave memory; no StorageClass is needed. |
| `ingress` | class `nginx`, host `chartproxy.container-registry.com`, TLS secret `chartproxy-tls` | Issued by cert-manager through the `cert-manager.io/cluster-issuer: letsencrypt-prod` annotation. |
| Ingress annotations | `proxy-body-size: 0`, buffering off, SSL redirect | Chart tarballs stream through unbuffered. |
| `resources` | requests 100m / 256Mi, limit 1Gi | Manifest and blob cache sizing. |
| `app.env_vars` | `MANIFEST_CACHE_TTL=60`, `INDEX_CACHE_TTL=14400`, `INDEX_ERROR_CACHE_TTL=30`, `REWRITE_DEPENDENCIES=false`, `USE_TLS=false`, `DEBUG=false` | Application defaults made explicit. TLS terminates at the ingress. |

The image tag is never set here. The chart's `appVersion` pins it, so an application release
reaches production through the appVersion bump and the following chart release
(`docs/RELEASES.md`).

## How changes reach the cluster

| Change | Path | Latency |
|---|---|---|
| Chart release `chart-vX.Y.Z` inside the semver range | The chart `OCIRepository` re-resolves the range, the `HelmRelease` upgrades | up to 5 min after the chart is published |
| Merge touching `deploy/prod/**`, `taskfiles/deploy.yml` or the publish workflow | `publish-deploy.yml` republishes and signs the artifact, the deploy `OCIRepository` picks up the new digest, the `Kustomization` applies it | up to 5 min after the merge |

Both paths are unattended. A chart major bump falls outside the range on purpose and needs a
deliberate edit of `spec.ref.semver` in `prod/chart-ocirepository.yaml`.

Each `publish-deploy.yml` run is recorded as a GitHub deployment to the `production`
environment. A successful deployment means the artifact is published and signed; the
cluster converges within the intervals above.

## CI

| Workflow | Trigger | Does |
|---|---|---|
| `deploy-ci.yml` | pull request and push touching `deploy/**`, the deploy tasks or the CI actions | `task deploy:build` renders both overlays, `task deploy:template` renders the released chart with the production values, so values the chart rejects fail here and not in the cluster |
| `publish-deploy.yml` | push to `main` touching `deploy/prod/**`, the deploy tasks or the workflow itself; `workflow_dispatch` on `main` only | `task deploy:release`: packages `prod/` as a Flux OCI artifact tagged `prod` and `sha-<short commit>`, signs it keyless by digest. Runs are serialized and never cancelled, so the `prod` tag never points at a partial upload |

Registry login is keyless: `.github/actions/harbor-login` exchanges the job's GitHub OIDC
token for a Harbor session, so the workflows carry `id-token: write` and no registry secret.

## Trust chain

Both artifacts are signed keyless by GitHub Actions and verified in-cluster through
`spec.verify.matchOIDCIdentity`, pinned to the workflow and ref:

| Artifact | Certificate identity |
|---|---|
| Deploy artifact | `…/.github/workflows/publish-deploy.yml@refs/heads/main` |
| Chart | `…/.github/workflows/publish-chart.yml@refs/heads/main` (reusable workflow; the certificate carries its ref, not the caller's) |

The `prod` tag is mutable, so the signature is what makes it trustworthy: a push from
anywhere other than that workflow on `main` is rejected by Flux and the cluster keeps serving
the last accepted artifact. The same check out of band:

```bash
cosign verify --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity https://github.com/container-registry/helm-charts-oci-proxy/.github/workflows/publish-deploy.yml@refs/heads/main \
  8gears.container-registry.com/library/helm-charts-oci-proxy-deploy:prod
```

A locally pushed artifact (`task deploy:publish`) fails verification unless `spec.verify` is
removed from `bootstrap/ocirepository.yaml` for the test.

## What the target cluster provides

The manifests do not name a cluster. Any cluster with the following works:

| | |
|---|---|
| Flux | `source.toolkit.fluxcd.io/v1`, `kustomize.toolkit.fluxcd.io/v1`, `helm.toolkit.fluxcd.io/v2` (Flux 2.6 or newer) |
| Ingress | `ingress-nginx` with class `nginx` |
| TLS | cert-manager `ClusterIssuer` `letsencrypt-prod` |
| DNS | `external-dns` with the ingress source. It publishes `chartproxy.container-registry.com` from the Ingress and owns the record through its TXT registry |
| Storage | none |
| Egress | Sigstore's public Fulcio, Rekor and TUF endpoints for keyless verification |

## Bootstrap

Bootstrap after CI has published once, so a signed artifact exists.

```bash
export KUBECONFIG=<target cluster>           # required; the tasks never default it
task deploy:bootstrap
task deploy:status
task deploy:verify INGRESS_IP=<public ingress IP>   # probes by IP, bypassing public DNS
```

## Operations

Suspend reconciliation:

```bash
kubectl -n chartproxy patch helmrelease chartproxy --type merge -p '{"spec":{"suspend":true}}'
kubectl -n flux-system patch kustomization chartproxy-prod --type merge -p '{"spec":{"suspend":true}}'
```

Pin a chart version: set `spec.ref.semver` in `prod/chart-ocirepository.yaml` to the exact
version. Pin a deploy artifact: set `ref.tag` in `bootstrap/ocirepository.yaml` to one of the
immutable `sha-<short commit>` tags.

Roll back: revert the commit and let the same paths carry it through, or pin as above.

## Reusing this for another service

Everything cluster- and registry-specific sits in a few places:

| What | Where |
|---|---|
| Registry and project | `REGISTRY_ADDRESS` / `REGISTRY_PROJECT` repository variables read by the workflows, defaults in `taskfiles/deploy.yml`, the `url` fields in both `OCIRepository` objects |
| Signing identity | the `subject` regex in `bootstrap/ocirepository.yaml` and `prod/chart-ocirepository.yaml`, one per publishing workflow |
| Hostname and issuer | `ingress.hosts`, `ingress.tls` and the `cluster-issuer` annotation in `prod/helmrelease.yaml` |
| Rollout policy | `spec.ref.semver` in `prod/chart-ocirepository.yaml`, the Flux `interval` fields |

The Harbor `library` project is public, which is what allows anonymous pulls. A private
project needs an image pull secret referenced from both `OCIRepository` objects, and the
cluster then holds a credential.
