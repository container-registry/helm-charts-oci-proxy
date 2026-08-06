# Release Process

This repository uses [release-please](https://github.com/googleapis/release-please) with two
independently versioned packages:

| Package | Path | Tags | Artifacts |
|---|---|---|---|
| Application | `.` (repo root, excl. `chart/`, `.github/`, `docs/`) | `vX.Y.Z` | Container image, binaries, GitHub release |
| Helm chart | `chart/` | `chart-vX.Y.Z` | OCI chart + Artifact Hub metadata |

All artifacts are published to Harbor at `8gears.container-registry.com` under project
`library` with the name `helm-charts-oci-proxy` — the image and the OCI chart share that name.

## Commit conventions

Squash merging is mandatory; the PR title becomes the commit message release-please parses, so
it must be a [conventional commit](https://www.conventionalcommits.org/) (enforced by the
`PR Title` workflow).

| Type | Version bump | Changelog section |
|---|---|---|
| `feat` | minor | Features |
| `fix` | patch | Bug Fixes |
| `perf` | patch | Performance Improvements |
| `revert` / `refactor` / `docs` | patch | own sections |
| `feat!` or `BREAKING CHANGE:` footer | major | — |
| `test` / `chore` / `ci` / `build` | none | hidden |

Scope PRs to one package. A commit touching both Go code and `chart/` bumps **both**
packages — split such changes into separate PRs.

## Release flow

1. Merging conventional commits to `main` makes release-please open (or refresh) one release
   PR per package: `chore: release helm-charts-oci-proxy X.Y.Z` (app) and
   `chore(chart): release chart X.Y.Z`.
2. Merging the **app** release PR tags `vX.Y.Z` and triggers, inside `release-please.yml`:
   - `publish-image` — multi-arch (amd64/arm64) image `library/helm-charts-oci-proxy:vX.Y.Z`
     + `latest`, cosign-signed, SPDX SBOM attested;
   - `publish-binaries` — cross-compiled tarballs + `checksums.txt` uploaded to the GitHub
     release;
   - `update-appversion` — opens a `fix(chart): update appVersion to vX.Y.Z` PR (runs after
     the image push, so the chart never references an unpushed tag);
   - `update-release-notes` — appends image digest and `cosign verify` instructions to the
     release body.
3. Merging the appVersion PR refreshes the chart release PR.
4. Merging the **chart** release PR tags `chart-vX.Y.Z` and triggers `publish-chart`:
   `helm push` to `oci://8gears.container-registry.com/library/helm-charts-oci-proxy`, then an
   `oras push` of `artifacthub-repo.yml` to the `:artifacthub.io` reference so
   [Artifact Hub](https://artifacthub.io) indexes the new version.

The publish jobs are downstream jobs of `release-please.yml` (not `on: push tags`) because
tags created with `GITHUB_TOKEN` do not trigger other workflows.

### Invariants

- `appVersion` in `chart/Chart.yaml` is v-prefixed (`"v1.0.0"`) and must equal a pushed image
  tag — the chart's default image reference is `<repository>:<appVersion>`.
- Never re-publish an existing chart version: Artifact Hub cannot re-index an OCI version.
  release-please's monotonic bumps guarantee this as long as nobody pushes charts by hand.

## Registry authentication (keyless)

No registry passwords. Workflows mint a GitHub OIDC token with audience
`https://8gears.container-registry.com` and use it as the registry password (username `jwt`)
for `docker login`, `helm registry login`, and `oras login`. Harbor maps the token to a
federated robot account via a claim rule on `repository == container-registry/helm-charts-oci-proxy`
— see [harbor-workload-identity-federation](https://github.com/container-registry/harbor-workload-identity-federation).

## PR builds

- `ci.yml` — vet, tests, build (Go paths).
- `chart-ci.yml` — `helm lint --strict`, `helm template`, `helm unittest` (chart paths).
- `pr-image.yml` — pushes a preview image `pr-<N>` to the dev project
  (`vars.PR_REGISTRY_PROJECT`) and maintains a sticky PR comment with the image reference and
  digest. Skipped for forks and dependabot (they cannot mint the OIDC token).

## Repository / infrastructure prerequisites

1. **Harbor**: federated robot with claim rule `repository == container-registry/helm-charts-oci-proxy`,
   audience `https://8gears.container-registry.com`, push+pull on `library` and on the PR
   preview project. The PR preview project (e.g. `library-dev`) must exist.
2. **GitHub repo settings**: squash merge only (default message: PR title); Settings →
   Actions → General → "Allow GitHub Actions to create and approve pull requests" enabled.
3. **GitHub variables**: `REGISTRY_ADDRESS` (optional, default `8gears.container-registry.com`),
   `REGISTRY_PROJECT` (optional, default `library`), `PR_REGISTRY_PROJECT` (required for PR
   preview images). The legacy `REGISTRY_ADDR`/`REGISTRY_USERNAME`/`REGISTRY_PASSWORD` are unused.
4. If branch protection ever requires status checks, release-please's PRs need a PAT
   (`GITHUB_TOKEN`-created PRs don't trigger CI) — pass it as `token` to the release-please
   action and to `peter-evans/create-pull-request` in `update-appversion.yml`.

## First-release runbook (v1.0.0)

The config carries `"release-as": "1.0.0"` to jump the app from v0.1.8 to v1.0.0.

1. Merge the workflow-introduction PR → release-please opens two release PRs.
2. Merge the **app** release PR first. Hold the chart release PR (its `appVersion` still
   points at the old image).
3. Merge the auto-opened `fix(chart): update appVersion to v1.0.0` PR.
4. Merge the refreshed chart release PR → chart publishes, Artifact Hub picks it up
   (indexing can lag ~30 min).
5. Open a cleanup PR removing `"release-as": "1.0.0"` from `release-please-config.json`
   (type `chore:` — no release). **Forgetting this pins every future app release to 1.0.0.**
