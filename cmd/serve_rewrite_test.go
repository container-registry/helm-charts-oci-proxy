package cmd

import "testing"

func TestRewriteRequiresProxyHost(t *testing.T) {
	if err := validateRewrite(true, ""); err == nil {
		t.Error("REWRITE_DEPENDENCIES without PROXY_HOST accepted")
	}
	for _, c := range []struct {
		rewrite bool
		host    string
	}{{true, "proxy.example.com"}, {false, ""}, {false, "proxy.example.com"}} {
		if err := validateRewrite(c.rewrite, c.host); err != nil {
			t.Errorf("validateRewrite(%v, %q): %v", c.rewrite, c.host, err)
		}
	}
}

func TestProxyHostMustBeBareHost(t *testing.T) {
	for _, h := range []string{"proxy.example.com", "proxy.example.com:8443", "proxy.example.com:1", "proxy.example.com:65535", "10.0.0.1:9000", "[::1]:9000", "[::1]"} {
		if err := validateRewrite(true, h); err != nil {
			t.Errorf("PROXY_HOST=%q rejected: %v", h, err)
		}
	}
	for _, h := range []string{
		"https://proxy.example.com", "proxy.example.com/", "proxy.example.com/charts", " proxy.example.com",
		"proxy.example.com ", "proxy.example.com\n", "user@proxy.example.com", "proxy.example.com?x=1",
		"proxy.example.com:port", ":9000", "proxy.example.com#x",
		"proxy.example.com:", "[::1]:", "proxy.example.com:0", "proxy.example.com:65536", "proxy.example.com:99999999999999999999",
	} {
		if err := validateRewrite(true, h); err == nil {
			t.Errorf("PROXY_HOST=%q accepted", h)
		}
	}
}
