


# Helm Chart OCI Proxy

Transparently proxy and transform [Chart Repository styled](https://helm.sh/docs/topics/chart_repository/) Helm Charts as OCI artifacts. Now you can address any public Chart Repository styled Helm Chart as an OCI styled artifact.

<p align="center"><img src="docs/diagram.png" alt="Diagram showing the workflow of the Helm Chart OCI Proxy" width=66%></p>

## What is it good for?

This proxy was primary designed after Harbor 2.8 removed support for [Chart Repository](https://helm.sh/docs/topics/chart_repository/) in favor of OCI. The proxy can be used without Harbor implementing other use cases.

* Store all 3rd party public Helm Charts in your OCI compliant registry. While you can switch the storage and distribution of your Helm Charts easily, it is close to impossible to do so for all sorts of 3rd party Helm Charts.
* Simplify your workflow and tooling by only using the OCI Helm Chart and not a mix of both
* Use it in combination with [Skopeo](https://github.com/containers/skopeo) to copy Helm Charts into your OCI registry of choice.

## Usage

Use our free hosted version via [chartproxy.container-registry.com](https://chartproxy.container-registry.com) or [host it yourself](#user-content-installation).

### Example

Here is an example of how you can use the service.
The following helm command will fetch `cert-manager` as an OCI Helm Chart, located on charts.jetstack.io.

```bash  
helm pull oci://chartproxy.container-registry.com/charts.jetstack.io/cert-manager --version 1.11.2
```  

If you do not specify a version, the system will retrieve the latest version.

```bash  
helm pull oci://stage-proxy.container-registry.com/charts.bitnami.com/bitnami/airflow #will use latest
```  


#### Use with Harbor

You can use the Helm Chart OCI Proxy with the Harbor Container Registry.
Each source needs to be added as an own endpoint.

To proxy, for example `charts.jetstack.io` you would set the Endpoint URL to `https://chartproxy.container-registry.com/charts.jetstack.io`.

You also would set the provider to _Docker Registry_.

<p align="center"><img src="docs/harbor_registry_endpoint.png" alt="Screenshot of adding Helm Chart OCI Proxy to Harbor" width=36%></p>

After adding the Endpoint, you can proceed with creating the replication rule.

<p align="center"><img src="docs/harbor_replication_rule.png" alt="Screenshot on how to create a replication rule for Helm Chart OCI Proxy to Harbor" width=36%></p>


## Installation

[![Artifact Hub](https://img.shields.io/endpoint?url=https://artifacthub.io/badge/repository/helm-charts-oci-proxy)](https://artifacthub.io/packages/search?repo=helm-charts-oci-proxy)

Install and operate the service yourself, we currently provide a handy Helm Chart, so you can get started quickly.

Our Helm Charts are only available as OCI artifacts. Unlike with traditional Charts where you
need to add a Repo first. With OCI, you can install the Chart with one command.

```bash
helm install chartproxy --version 1.0.0 --create-namespace --namespace chartproxy oci://8gears.container-registry.com/library/helm-charts-oci-proxy 
```

Use `helm pull` to only pull the chart to your local disc, without installing.

```bash
helm pull oci://8gears.container-registry.com/library/helm-charts-oci-proxy --version 1.0.0
```

### Installation outside Kubernetes

We also provide the container image that you use with your container runtime of choice.

```bash
docker pull 8gears.container-registry.com/library/helm-charts-oci-proxy
```


## Development

Build the binary

```shell  
./do.sh build
```  

### Run Locally
```shell  
./do.sh run
```  

### Run Tests

Tests without specifying a version will pull the latest version.

```shell  
helm pull --repository-cache=/tmp2 oci://registry:9000/charts.jetstack.io/cert-manager-istio-csr  
helm pull oci://registry:9000/charts.jetstack.io/cert-manager-istio-csr  
helm pull oci://registry:9000/charts.bitnami.com/bitnami/airflow  
helm pull oci://registry:9000/charts.bitnami.com/bitnami/airflow --version 14.0.11  
```  

With specific version

```shell  
helm pull --repository-cache=/tmp2 oci://registry:9000/charts.jetstack.io/cert-manager-istio-csr --version 0.2.1
```  

### Environment Variables

There are not many options in configure the application except the following.

* `PORT` - specifies port, default `9000`
* `DEBUG` - enabled debug if it's `TRUE`
* `CACHE_TTL` - for how many seconds we have to store manifest and related blobs, default value is 60
* `USE_TLS` - enabled HTTP over TLS


### TODO

* CI/CD Pipeline with GitHub Action
* Add tests
* Add helm index cache layer
