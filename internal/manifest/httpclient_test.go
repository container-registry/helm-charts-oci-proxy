package manifest

import (
	"net/netip"
	"strings"
	"testing"
)

func TestIsPublicAddr(t *testing.T) {
	tests := []struct {
		addr   string
		public bool
	}{
		{"127.0.0.1", false},
		{"10.1.2.3", false},
		{"172.16.0.1", false},
		{"192.168.1.1", false},
		{"169.254.169.254", false}, // cloud metadata
		{"0.0.0.0", false},
		{"0.1.2.3", false},
		{"::1", false},
		{"fe80::1", false},
		{"fd00::1", false},
		{"::ffff:127.0.0.1", false}, // 4-in-6 mapped loopback
		{"::ffff:10.0.0.1", false},
		{"1.1.1.1", true},
		{"142.250.180.14", true},
		{"2606:4700:4700::1111", true},
	}
	for _, tt := range tests {
		if got := isPublicAddr(netip.MustParseAddr(tt.addr)); got != tt.public {
			t.Errorf("isPublicAddr(%s) = %v, want %v", tt.addr, got, tt.public)
		}
	}
}

func TestDownloadClientBlocksPrivateDial(t *testing.T) {
	client := newDownloadClient(false)
	_, err := client.Get("https://127.0.0.1:1/index.yaml")
	if err == nil || !strings.Contains(err.Error(), "non-public address") {
		t.Fatalf("expected non-public address refusal, got: %v", err)
	}
}

func TestDownloadRejectsNonHTTPS(t *testing.T) {
	m := &Manifests{config: Config{}, httpClient: newDownloadClient(false)}
	if _, err := m.download("http://169.254.169.254/latest/meta-data/"); err == nil || !strings.Contains(err.Error(), "non-https") {
		t.Fatalf("expected non-https refusal, got: %v", err)
	}
}
