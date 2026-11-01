package cmd

import (
	"os"
	"strconv"
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
	for key, v := range map[string]string{
		"PREPARE_CONCURRENCY": "0", "SHUTDOWN_TIMEOUT": "0", "MANIFEST_CACHE_TTL": "0", "INDEX_CACHE_TTL": "0", "INDEX_ERROR_CACHE_TTL": "-1",
		"PORT": "0", "PORT ": "65536", "INDEX_CACHE_SIZE_MIB": "0", "INDEX_CACHE_SIZE_MIB ": strconv.Itoa(maxIndexCacheMiB + 1),
	} {
		key := strings.TrimSpace(key)
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

// Charts render an unset value as "", which must select the default.
func TestLoadServeConfigTreatsEmptyAsUnset(t *testing.T) {
	clearServeEnv(t)
	for _, k := range serveEnvKeys {
		t.Setenv(k, "")
	}
	c, err := loadServeConfig()
	if err != nil {
		t.Fatal(err)
	}
	if c.port != 9000 || c.debug || c.manifestCacheTTL != time.Minute || c.indexCacheMiB != 128 ||
		c.certFile != "certs/registry.pem" || c.keyFile != "certs/registry-key.pem" || c.shutdownTimeout != 25*time.Second {
		t.Errorf("empty variables did not select the defaults: %+v", c)
	}
}

func TestLoadServeConfigReportsAllErrors(t *testing.T) {
	clearServeEnv(t)
	t.Setenv("PORT", "notaport")
	t.Setenv("INDEX_CACHE_SIZE_MIB", strconv.Itoa(maxIndexCacheMiB+1))
	_, err := loadServeConfig()
	if err == nil || !strings.Contains(err.Error(), "PORT") || !strings.Contains(err.Error(), "INDEX_CACHE_SIZE_MIB") {
		t.Fatalf("err = %v, want both PORT and INDEX_CACHE_SIZE_MIB", err)
	}
}

func TestLoadServeConfigAcceptsZeroIndexErrorCacheTTL(t *testing.T) {
	clearServeEnv(t)
	t.Setenv("INDEX_ERROR_CACHE_TTL", "0")
	c, err := loadServeConfig()
	if err != nil || c.indexErrorCacheTTL != 0 {
		t.Fatalf("indexErrorCacheTTL %v, err %v", c.indexErrorCacheTTL, err)
	}
}

// n*time.Second must not wrap to a negative duration that passes the range check.
func TestLoadServeConfigRejectsOverflowingSeconds(t *testing.T) {
	big := strconv.FormatInt(maxSeconds+1, 10)
	for _, key := range []string{"SHUTDOWN_TIMEOUT", "MANIFEST_CACHE_TTL", "INDEX_CACHE_TTL", "INDEX_ERROR_CACHE_TTL"} {
		t.Run(key, func(t *testing.T) {
			clearServeEnv(t)
			t.Setenv(key, big)
			if c, err := loadServeConfig(); err == nil {
				t.Errorf("%s=%s accepted: %+v", key, big, c)
			}
		})
	}
	clearServeEnv(t)
	t.Setenv("SHUTDOWN_TIMEOUT", strconv.FormatInt(maxSeconds, 10))
	if c, err := loadServeConfig(); err != nil || c.shutdownTimeout <= 0 {
		t.Fatalf("largest valid value: %v, %v", c.shutdownTimeout, err)
	}
}
