package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

// upstreamPolicyError marks a fetch the proxy refuses as well, so the chart is
// not-applicable rather than an upstream or proxy failure.
type upstreamPolicyError struct{ msg string }

func (e *upstreamPolicyError) Error() string { return e.msg }

func isPolicyError(err error) bool {
	var pe *upstreamPolicyError
	return errors.As(err, &pe)
}

// guardUpstream applies the proxy's download policy (internal/manifest/httpclient.go)
// to direct upstream fetches, whose URLs come from third-party indexes:
// https-only redirects and, unless allowPrivate, no dial to non-public addresses.
func guardUpstream(base *http.Client, allowPrivate bool) *http.Client {
	tr, ok := base.Transport.(*http.Transport)
	if ok {
		tr = tr.Clone()
	} else {
		tr = http.DefaultTransport.(*http.Transport).Clone()
	}
	if !allowPrivate {
		d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return &upstreamPolicyError{fmt.Sprintf("refusing to dial unparsable address %q", address)}
			}
			if !isPublicAddr(ap.Addr()) {
				return &upstreamPolicyError{fmt.Sprintf("refusing to dial non-public address %s", ap.Addr())}
			}
			return nil
		}}
		tr.DialContext = d.DialContext
	}
	c := *base
	c.Transport = tr
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" {
			return &upstreamPolicyError{fmt.Sprintf("refusing redirect to non-https URL %q", req.URL)}
		}
		if len(via) >= 10 {
			return fmt.Errorf("stopped after 10 redirects")
		}
		return nil
	}
	return &c
}

// isPublicAddr mirrors the proxy's check of the same name.
func isPublicAddr(addr netip.Addr) bool {
	addr = addr.Unmap()
	if addr.Is4() && addr.As4()[0] == 0 {
		return false
	}
	return !(addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() ||
		addr.IsInterfaceLocalMulticast() || addr.IsMulticast() || addr.IsUnspecified())
}
