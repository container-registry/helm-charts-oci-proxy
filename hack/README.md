# Load replay rig

Tools to replay recorded production traffic against a locally built proxy under a
memory limit, with the upstream chart repositories replaced by a local mirror. A replay
against the real upstreams from a laptop does not reproduce production load: upstream
latency plus the proxy's request serialization makes most requests time out, so the
proxy never holds the state it holds in production.

- `upstream-mirror fetch` reads an ingress-nginx access log export (VictoriaLogs JSONL,
  fields `_time` and `_msg`), resolves every requested chart version with the proxy's own
  index parser, and records the indexes and chart tarballs (status codes and bodies as-is).
  It also writes a CA and a server certificate for all mirrored hosts.
- `upstream-mirror serve` serves the recording over HTTPS, with optional added latency.
- `replay` sends the recorded registry requests to the proxy at their original relative
  timing (open loop) and samples cgroup memory and `DIAG_ADDR` expvars every 200ms. It
  saves a heap and goroutine profile each time the in-use heap passes the previous peak
  by 50MiB.

## Run

```shell
go build -o .bin/upstream-mirror ./hack/upstream-mirror
go build -o .bin/replay ./hack/replay
CGO_ENABLED=0 GOOS=linux go build -o .bin/upstream-mirror-linux ./hack/upstream-mirror
docker build -t chartproxy:local .

.bin/upstream-mirror fetch -log ingress.jsonl -from 2026-11-01T07:59:16Z -to 2026-11-01T08:08:14Z -dir mirror

# The mirror answers for the real hostnames inside a dedicated network.
docker network create cpbench
docker run -d --name upstream-mirror --network cpbench --user 0 --entrypoint /mirror \
  $(sed 's/^/--network-alias /' mirror/hosts.txt) \
  -v "$PWD/.bin/upstream-mirror-linux:/mirror:ro" -v "$PWD/mirror:/data:ro" \
  chartproxy:local serve -dir /data -addr :443 -latency 20ms

docker run -d --name chartproxy --network cpbench --memory=1g --memory-swap=1g --cpus=4 \
  -p 127.0.0.1:19000:9000 -p 127.0.0.1:19001:9001 -e DIAG_ADDR=0.0.0.0:9001 -e ALLOW_PRIVATE_NETWORKS=true \
  -e SSL_CERT_FILE=/ca.pem -v "$PWD/mirror/ca.pem:/ca.pem:ro" chartproxy:local

.bin/replay -log ingress.jsonl -from 2026-11-01T07:59:16Z -to 2026-11-01T08:08:14Z -target docker:chartproxy -out run
docker inspect chartproxy --format '{{.State.OOMKilled}}'
go tool pprof -top -sample_index=inuse_space run/heap-peak.pb.gz
```

`ALLOW_PRIVATE_NETWORKS=true` is needed because the mirror has a private address.
