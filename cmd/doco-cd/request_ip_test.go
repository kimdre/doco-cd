package main

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/kimdre/doco-cd/internal/config/app"
)

func TestResolveRequestIPUsesRemoteAddrWhenUntrusted(t *testing.T) {
	got := resolveRequestIP("172.18.0.24:58534", "", nil, nil)
	if got != "172.18.0.24:58534" {
		t.Fatalf("expected remote address to be used, got %q", got)
	}
}

func TestResolveRequestIPUsesXForwardedForWhenTrusted(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("172.16.0.0/12")}
	headers := make(http.Header)
	headers.Set("X-Forwarded-For", "8.8.8.8, 172.18.0.24")

	got := resolveRequestIP("172.18.0.24:58534", "X-Forwarded-For", headers, trusted)
	if got != "8.8.8.8:58534" {
		t.Fatalf("expected forwarded client address to be used, got %q", got)
	}
}

func TestResolveRequestIPFallsBackToFirstUntrustedHop(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("172.16.0.0/12")}
	headers := make(http.Header)
	headers.Set("X-Forwarded-For", "8.8.8.8, 172.18.0.24, 172.18.0.25")

	got := resolveRequestIP("172.18.0.24:58534", "", headers, trusted)
	if got != "8.8.8.8:58534" {
		t.Fatalf("expected first untrusted hop to be used, got %q", got)
	}
}

func TestResolveRequestIPUsesForwardedHeader(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("172.16.0.0/12")}
	headers := make(http.Header)
	headers.Set("Forwarded", `for=8.8.8.8;proto=https;by=172.18.0.24`)

	got := resolveRequestIP("172.18.0.24:58534", "", headers, trusted)
	if got != "8.8.8.8:58534" {
		t.Fatalf("expected Forwarded header client address to be used, got %q", got)
	}
}

func TestResolveRequestIPUsesCustomHeaderWhenTrusted(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("172.16.0.0/12")}
	headers := make(http.Header)
	headers.Set("X-Client-IP", "8.8.4.4")

	got := resolveRequestIP("172.18.0.24:58534", "X-Client-IP", headers, trusted)
	if got != "8.8.4.4:58534" {
		t.Fatalf("expected custom header client address to be used, got %q", got)
	}
}

func TestResolveRequestIPUnmapsIPv4In6RemoteAddr(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	headers := make(http.Header)
	headers.Set("X-Forwarded-For", "8.8.8.8")

	got := resolveRequestIP("[::ffff:127.0.0.1]:58534", "X-Forwarded-For", headers, trusted)
	if got != "8.8.8.8:58534" {
		t.Fatalf("expected 4-in-6 remote address to match IPv4 trusted network, got %q", got)
	}
}

func TestResolveRequestIPCustomHeaderDoesNotFallBackToXForwardedFor(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("172.16.0.0/12")}
	headers := make(http.Header)
	headers.Set("X-Forwarded-For", "8.8.8.8")

	got := resolveRequestIP("172.18.0.24:58534", "X-Client-IP", headers, trusted)
	if got != "172.18.0.24:58534" {
		t.Fatalf("expected remote address to be used when custom header is missing, got %q", got)
	}
}

func TestHandlerRequestIPUsesConfiguredTrustList(t *testing.T) {
	h := handlerData{
		appConfig: &app.Config{
			TrustedProxyNetworks: []netip.Prefix{netip.MustParsePrefix("172.16.0.0/12")},
		},
	}

	req := httptest.NewRequest(http.MethodGet, "http://example.com", nil)
	req.RemoteAddr = "172.18.0.24:58534"
	req.Header.Set("X-Forwarded-For", "8.8.8.8")

	if got := h.requestIP(req); got != "8.8.8.8:58534" {
		t.Fatalf("expected handler request IP to use configured trust list, got %q", got)
	}
}

func TestHandlerRequestIPUsesConfiguredCustomHeader(t *testing.T) {
	h := handlerData{
		appConfig: &app.Config{
			TrustedProxyNetworks: []netip.Prefix{netip.MustParsePrefix("172.16.0.0/12")},
			TrustedProxyHeader:   "X-Client-IP",
		},
	}

	req := httptest.NewRequest(http.MethodGet, "http://example.com", nil)
	req.RemoteAddr = "172.18.0.24:58534"
	req.Header.Set("X-Client-IP", "8.8.4.4")

	if got := h.requestIP(req); got != "8.8.4.4:58534" {
		t.Fatalf("expected handler request IP to use custom header, got %q", got)
	}
}
