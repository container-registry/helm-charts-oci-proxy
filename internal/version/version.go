// Package version holds the build-time version information.
package version

// Version is overridden at build time via
// -ldflags "-X github.com/container-registry/helm-charts-oci-proxy/internal/version.Version=...".
var Version = "dev"
