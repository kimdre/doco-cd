package restapi

import (
	"net/http"
	"net/netip"
	"testing"
)

func TestResolveRequestIPUsesRemoteAddrWhenUntrusted(t *testing.T) {
	got := ResolveRequestIP("172.18.0.24:58534", "", nil, nil)
	if got != "172.18.0.24:58534" {
		t.Fatalf("expected remote address to be used, got %q", got)
	}
}

func TestResolveRequestIPUsesXForwardedForWhenTrusted(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("172.16.0.0/12")}
	headers := make(http.Header)
	headers.Set("X-Forwarded-For", "8.8.8.8, 172.18.0.24")

	got := ResolveRequestIP("172.18.0.24:58534", "X-Forwarded-For", headers, trusted)
	if got != "8.8.8.8:58534" {
		t.Fatalf("expected forwarded client address to be used, got %q", got)
	}
}

func TestResolveRequestIPFallsBackToFirstUntrustedHop(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("172.16.0.0/12")}
	headers := make(http.Header)
	headers.Set("X-Forwarded-For", "8.8.8.8, 172.18.0.24, 172.18.0.25")

	got := ResolveRequestIP("172.18.0.24:58534", "", headers, trusted)
	if got != "8.8.8.8:58534" {
		t.Fatalf("expected first untrusted hop to be used, got %q", got)
	}
}

func TestResolveRequestIPUsesForwardedHeader(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("172.16.0.0/12")}
	headers := make(http.Header)
	headers.Set("Forwarded", `for=8.8.8.8;proto=https;by=172.18.0.24`)

	got := ResolveRequestIP("172.18.0.24:58534", "", headers, trusted)
	if got != "8.8.8.8:58534" {
		t.Fatalf("expected Forwarded header client address to be used, got %q", got)
	}
}

func TestResolveRequestIPUsesCustomHeaderWhenTrusted(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("172.16.0.0/12")}
	headers := make(http.Header)
	headers.Set("X-Client-IP", "8.8.4.4")

	got := ResolveRequestIP("172.18.0.24:58534", "X-Client-IP", headers, trusted)
	if got != "8.8.4.4:58534" {
		t.Fatalf("expected custom header client address to be used, got %q", got)
	}
}

func TestResolveRequestIPUnmapsIPv4In6RemoteAddr(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	headers := make(http.Header)
	headers.Set("X-Forwarded-For", "8.8.8.8")

	got := ResolveRequestIP("[::ffff:127.0.0.1]:58534", "X-Forwarded-For", headers, trusted)
	if got != "8.8.8.8:58534" {
		t.Fatalf("expected 4-in-6 remote address to match IPv4 trusted network, got %q", got)
	}
}

func TestResolveRequestIPCustomHeaderDoesNotFallBackToXForwardedFor(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("172.16.0.0/12")}
	headers := make(http.Header)
	headers.Set("X-Forwarded-For", "8.8.8.8")

	got := ResolveRequestIP("172.18.0.24:58534", "X-Client-IP", headers, trusted)
	if got != "172.18.0.24:58534" {
		t.Fatalf("expected remote address to be used when custom header is missing, got %q", got)
	}
}
