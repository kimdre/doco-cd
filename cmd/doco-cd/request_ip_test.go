package main

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/kimdre/doco-cd/internal/config/app"
)

func TestHandlerRequestIPUsesConfiguredTrustList(t *testing.T) {
	h := orchestrationHandler{
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
	h := orchestrationHandler{
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
