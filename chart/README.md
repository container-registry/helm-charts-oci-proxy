# Helm Chart OCI Proxy

Helm Chart OCI Proxy transparently proxies and transforms traditional Chart Repository styled Helm Charts into OCI artifacts. It lets you address any public Chart Repository Helm Chart as an OCI image, e.g. `helm pull oci://chartproxy.container-registry.com/charts.jetstack.io/cert-manager`. This is particularly useful since Harbor 2.8 removed support for the older Chart Repository format.

Source and documentation: [github.com/container-registry/helm-charts-oci-proxy](https://github.com/container-registry/helm-charts-oci-proxy)

## Install

The chart is published as an OCI artifact, no repository setup required:

```bash
helm install ocip oci://8gears.container-registry.com/library/helm-charts-oci-proxy
```

To pin a specific chart version, add `--version <chart version>`:

```bash
helm install ocip oci://8gears.container-registry.com/library/helm-charts-oci-proxy --version <chart version>
```

## Values

### Image

| Key | Default | Description |
|-----|---------|-------------|
| `image.repository` | `8gears.container-registry.com/library/helm-charts-oci-proxy` | Image repository |
| `image.tag` | `""` | Image tag, defaults to the chart `appVersion` |
| `image.pullPolicy` | `Always` | Image pull policy |
| `imagePullSecrets` | `[]` | Pull secrets for private registries |

### Deployment

| Key | Default | Description |
|-----|---------|-------------|
| `replicaCount` | `1` | Number of replicas (ignored when autoscaling is enabled) |
| `deploymentStrategy.type` | `RollingUpdate` | Deployment strategy |
| `resources.requests` | `cpu: 100m, memory: 128Mi` | Resource requests |
| `nodeSelector` / `tolerations` / `affinity` | `{}` / `[]` / `{}` | Scheduling controls |
| `podAnnotations` / `podLabels` / `customLabels` | `{}` | Extra metadata |
| `podSecurityContext` / `securityContext` | `{}` | Security contexts |
| `serviceAccount.create` | `false` | Create a dedicated ServiceAccount |
| `serviceAccount.annotations` | `{}` | Annotations for the ServiceAccount |
| `serviceAccount.name` | `""` | ServiceAccount name override |
| `nameOverride` | `""` | Override the chart name used in resource names |
| `fullnameOverride` | `""` | Override the fully qualified resource name |

### Service and ingress

| Key | Default | Description |
|-----|---------|-------------|
| `service.type` | `ClusterIP` | Service type |
| `service.port` | `9000` | Service port |
| `ingress.enabled` | `false` | Enable ingress |
| `ingress.className` | `nginx` | Ingress class |
| `ingress.annotations` | cert-manager/nginx annotations | Ingress annotations |
| `ingress.hosts` | `chartproxy.container-registry.com` | Hostnames and paths |
| `ingress.tls` | ACME TLS secret | TLS configuration |

### Application environment (`app.env_vars`)

| Key | Default | Description |
|-----|---------|-------------|
| `app.env_vars.DEBUG` | `false` | Enable debug logging |
| `app.env_vars.USE_TLS` | `false` | Serve HTTPS instead of HTTP (probes switch scheme accordingly) |
| `app.env_vars.MANIFEST_CACHE_TTL` | `60` | Manifest cache duration in seconds |
| `app.env_vars.INDEX_CACHE_TTL` | `14400` | Chart index cache duration in seconds (4h) |
| `app.env_vars.INDEX_ERROR_CACHE_TTL` | `30` | Retry delay for failed index fetches in seconds |
| `app.env_vars.REWRITE_DEPENDENCIES` | `false` | Rewrite chart dependency URLs to point through the proxy |
| `app.env_vars.PROXY_HOST` | `""` | Override proxy host for rewritten URLs (defaults to request Host header) |

### Persistence

| Key | Default | Description |
|-----|---------|-------------|
| `persistence.enabled` | `false` | Persist data in a PVC instead of an emptyDir |
| `persistence.existingClaim` | `""` | Use an existing PVC instead of creating one |
| `persistence.size` | `5Gi` | Requested volume size |
| `persistence.accessMode` | `ReadWriteOnce` | PVC access mode |
| `persistence.annotations` | `{}` | PVC annotations |

### Extra volumes

| Key | Default | Description |
|-----|---------|-------------|
| `extraVolumes` | `[]` | Additional pod volumes (e.g. a custom CA bundle ConfigMap) |
| `extraVolumeMounts` | `[]` | Additional container volume mounts |

### Autoscaling

| Key | Default | Description |
|-----|---------|-------------|
| `autoscaling.enabled` | `false` | Enable HorizontalPodAutoscaler (requires Kubernetes v1.23+) |
| `autoscaling.minReplicas` | `1` | Minimum replicas |
| `autoscaling.maxReplicas` | `100` | Maximum replicas |
| `autoscaling.targetCPUUtilizationPercentage` | `80` | CPU utilization target |

## License

AGPL-3.0-only. See the [LICENSE](https://github.com/container-registry/helm-charts-oci-proxy/blob/main/LICENSE) file.
