package manifest

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

// newDownloadClient returns the HTTP client for upstream chart repository
// requests. Unless allowPrivate is set, connections to non-public addresses are
// refused so a crafted repo path or index.yaml cannot turn the proxy into an
// SSRF gateway (cloud metadata endpoints, cluster-internal services). The check
// runs at dial time, after DNS resolution, so it also covers redirects and DNS
// rebinding.
func newDownloadClient(allowPrivate bool) *http.Client {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	if !allowPrivate {
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return fmt.Errorf("refusing to dial unparsable address %q: %w", address, err)
			}
			if !isPublicAddr(ap.Addr()) {
				return fmt.Errorf("refusing to dial non-public address %s (set ALLOW_PRIVATE_NETWORKS=true to permit)", ap.Addr())
			}
			return nil
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = dialer.DialContext
	return &http.Client{
		Timeout:   60 * time.Second,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != "https" {
				return fmt.Errorf("refusing redirect to non-https URL %q", req.URL)
			}
			// A non-nil CheckRedirect replaces the client's default 10-hop limit.
			if len(via) >= 10 {
				return fmt.Errorf("stopped after 10 redirects")
			}
			return nil
		},
	}
}

func isPublicAddr(addr netip.Addr) bool {
	addr = addr.Unmap()
	// 0.0.0.0/8 routes to the local host on Linux.
	if addr.Is4() && addr.As4()[0] == 0 {
		return false
	}
	return !(addr.IsLoopback() ||
		addr.IsPrivate() ||
		addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() ||
		addr.IsInterfaceLocalMulticast() ||
		addr.IsMulticast() ||
		addr.IsUnspecified())
}
