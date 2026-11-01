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
