package api

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/logger"
)

func TestDockerCliForRequestContextValidation(t *testing.T) {
	t.Parallel()

	h := Handler{log: logger.New(logger.LevelCritical)}
	jobLog := h.log.With()

	tests := []struct {
		name       string
		url        string
		wantOK     bool
		wantStatus int
		wantHeader string
	}{
		{"default omitted", "/v1/api/projects", true, http.StatusOK, "default"},
		{"default explicit", "/v1/api/projects?context=default", true, http.StatusOK, "default"},
		{"unknown named context", "/v1/api/projects?context=remote", false, http.StatusBadRequest, "remote"},
		{"repeated context", "/v1/api/projects?context=default&context=remote", false, http.StatusBadRequest, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(http.MethodGet, tt.url, nil)
			recorder := httptest.NewRecorder()

			_, _, ok := h.dockerCliForRequest(recorder, req, jobLog, "job-id")
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}

			status := recorder.Code
			if tt.wantOK {
				status = http.StatusOK
			}

			if status != tt.wantStatus {
				t.Fatalf("status = %d, want %d", status, tt.wantStatus)
			}

			if got := recorder.Header().Get(dockerContextHeader); got != tt.wantHeader {
				t.Fatalf("%s = %q, want %q", dockerContextHeader, got, tt.wantHeader)
			}
		})
	}
}

func TestHandlerRequestIPUsesConfiguredTrustList(t *testing.T) {
	t.Parallel()

	h := Handler{
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
	t.Parallel()

	h := Handler{
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
