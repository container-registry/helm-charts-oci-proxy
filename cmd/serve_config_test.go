package cmd

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/container-registry/helm-charts-oci-proxy/internal/manifest"
)

var serveEnvKeys = []string{
	"PORT", "DEBUG", "MANIFEST_CACHE_TTL", "INDEX_CACHE_TTL", "INDEX_CACHE_SIZE_MIB", "INDEX_ERROR_CACHE_TTL",
	"PREPARE_CONCURRENCY", "REWRITE_DEPENDENCIES", "PROXY_HOST", "ALLOW_PRIVATE_NETWORKS", "USE_TLS",
	"CERT_FILE", "KEY_FILE", "SHUTDOWN_TIMEOUT", "DIAG_ADDR",
}

// clearServeEnv unsets the serve variables for one test; t.Setenv restores them.
func clearServeEnv(t *testing.T) {
	for _, k := range serveEnvKeys {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

func TestLoadServeConfigDefaults(t *testing.T) {
	clearServeEnv(t)
	c, err := loadServeConfig()
	if err != nil {
		t.Fatal(err)
	}
	if c.port != 9000 || c.manifestCacheTTL != time.Minute || c.indexCacheTTL != 4*time.Hour ||
		c.indexErrorCacheTTL != 30*time.Second || c.shutdownTimeout != 25*time.Second ||
		c.prepareConcurrency != manifest.DefaultPrepareConcurrency || c.indexCacheMiB != 128 {
		t.Errorf("unexpected defaults: %+v", c)
	}
}

// A value that does not parse must stop the start, not fall back to the default.
func TestLoadServeConfigRejectsInvalidValues(t *testing.T) {
	clearServeEnv(t)
	for _, key := range []string{
		"PORT", "MANIFEST_CACHE_TTL", "INDEX_CACHE_TTL", "INDEX_CACHE_SIZE_MIB", "INDEX_ERROR_CACHE_TTL",
		"PREPARE_CONCURRENCY", "SHUTDOWN_TIMEOUT", "DEBUG", "REWRITE_DEPENDENCIES", "ALLOW_PRIVATE_NETWORKS", "USE_TLS",
	} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, "notavalue")
			if _, err := loadServeConfig(); err == nil || !strings.Contains(err.Error(), key) {
				t.Errorf("%s=notavalue: err = %v", key, err)
			}
		})
	}
	for key, v := range map[string]string{"PREPARE_CONCURRENCY": "0", "SHUTDOWN_TIMEOUT": "0", "INDEX_CACHE_SIZE_MIB": "0", "MANIFEST_CACHE_TTL": "-1"} {
		t.Run(key+"="+v, func(t *testing.T) {
			t.Setenv(key, v)
			if _, err := loadServeConfig(); err == nil {
				t.Errorf("%s=%s accepted", key, v)
			}
		})
	}
}

func TestLoadServeConfigRejectsRewriteWithoutProxyHost(t *testing.T) {
	clearServeEnv(t)
	t.Setenv("REWRITE_DEPENDENCIES", "true")
	if _, err := loadServeConfig(); err == nil {
		t.Fatal("REWRITE_DEPENDENCIES=true without PROXY_HOST accepted")
	}
	t.Setenv("PROXY_HOST", "proxy.example.com")
	c, err := loadServeConfig()
	if err != nil || !c.rewriteDeps || c.proxyHost != "proxy.example.com" {
		t.Fatalf("config %+v, err %v", c, err)
	}
}
