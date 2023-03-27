# helm-charts-oci-proxy
The Helm Charts OCI Proxy, will proxy and transform Helm Chart into OCI images on the fly. Address any Helm Chart as OCI image. 


#### Build

```shell
./do.sh build
```

#### Run
```shell
./do.sh run
```

#### Test

Test without specified version (should pull the latest)
```shell
helm pull --repository-cache=/tmp2 oci://registry:9000/charts.jetstack.io/cert-manager-istio-csr
```

With specific version
```shell
helm pull --repository-cache=/tmp2 oci://registry:9000/charts.jetstack.io/cert-manager-istio-csr --version 0.2.1
```

#### Environment variables

* `PORT` - specifies port, default `9000`
* `DEBUG` - enabled debug if it's `TRUE`
* `CACHE_TTL_MIN` - how long store manifest and related blobs, default value 15
* `USE_TLS` - enabled HTTP over TLS


###
* Add tests
* Add helm index cache layer
