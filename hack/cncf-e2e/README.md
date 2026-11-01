# CNCF charts end-to-end check

`cncf-e2e` takes every Helm package of a CNCF project listed on Artifact Hub
(`https://artifacthub.io/packages/search?kind=0&cncf=true`), pulls its chart through a running proxy and compares
each answer with what the chart repository serves directly. It hits external networks, so it is not wired into CI.

Per chart (latest version, or every version in the index with `-all-versions`):

1. Upstream reference: `index.yaml` parsed with helm's loader, tarball downloaded from its URL, sha256 computed,
   archive loaded with helm's chart loader.
2. Manifest by tag and by digest: media types, `Docker-Content-Digest`, a single layer whose digest and size equal
   the upstream tarball.
3. Config blob: digest and size match the manifest, name and version match `Chart.yaml`.
4. Layer blob: streamed bytes hash to the upstream digest.
5. `helm pull` and `helm show chart` through the proxy (helm 3; helm 4 prints OCI pull output differently).

Results are classified as:

| Class | Meaning |
|---|---|
| `ok` | every check passed (a note says when the index digest is stale or the first attempt failed) |
| `proxy-bug` | upstream is fine and unchanged, the proxy check failed twice; evidence is recorded |
| `upstream-problem` | index or tarball unreachable, 4xx/5xx, missing entry or invalid archive |
| `not-applicable` | OCI-only repository or OCI/non-https download URL, which the proxy does not convert |

A failed proxy check is repeated after `-retry-delay` (default 10s), then the upstream is checked again; only a
failure that repeats against an unchanged upstream counts as `proxy-bug`.

## Run

```shell
go build -o .bin/cncf-e2e ./hack/cncf-e2e
docker build -t chartproxy:local .
docker run -d --name chartproxy-e2e --memory=1g --memory-swap=1g \
  -p 127.0.0.1:19100:9000 -p 127.0.0.1:19101:9001 -e DIAG_ADDR=0.0.0.0:9001 chartproxy:local

.bin/cncf-e2e -proxy http://127.0.0.1:19100 -diag http://127.0.0.1:19101 -target docker:chartproxy-e2e \
  -helm /path/to/helm3 -out cncf-e2e-out -label "$(git rev-parse --short HEAD)"
```

`task app:e2e-cncf HELM=/path/to/helm3` runs the tool against a proxy already listening on `PROXY`
(default `http://127.0.0.1:19100`).

Output in `-out`: `discovery.json` (Artifact Hub result, reused on later runs unless `-refresh`), `results.json`
(one row per chart with digests and evidence) and `summary.md` (counts per class, every non-ok row, memory, wall
time).

Memory: with `-target docker:<container>` the tool samples the cgroup working set every 500ms and reads
`memory.peak` and `OOMKilled` at the end; with `-diag` it records Go heap and the proxy's expvar counters. Restart
the container before a run so the peak belongs to that run.

Useful flags: `-only <regexp>` on `<repo URL>/<chart>`, `-concurrency` (8), `-per-host` (2), `-deprecated`,
`-timeout` (3m per request or helm command).
