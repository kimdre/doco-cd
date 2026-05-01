package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGetLatestAppReleaseVersion(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/releases" {
			t.Fatalf("expected request path /releases, got %q", r.URL.Path)
		}

		w.Header().Set("Content-Type", "application/json")

		_, err := w.Write([]byte(`[
			{"tag_name":"v9.9.9-rc1","prerelease":true,"draft":false},
			{"tag_name":"v9.9.9","prerelease":false,"draft":false},
			{"tag_name":"v9.9.8","prerelease":false,"draft":false}
		]`))
		if err != nil {
			t.Fatalf("failed to write test response: %v", err)
		}
	}))
	defer server.Close()

	version, err := getLatestAppReleaseVersionFromURL(server.URL+"/releases", server.Client())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if version != "v9.9.9" {
		t.Fatalf("expected latest stable version %q, got %q", "v9.9.9", version)
	}
}

func TestGetLatestAppReleaseVersion_StatusError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate limited", http.StatusForbidden)
	}))
	defer server.Close()

	_, err := getLatestAppReleaseVersionFromURL(server.URL, server.Client())
	if err == nil {
		t.Fatal("expected an error, got nil")
	}

	if got := err.Error(); got == "" || !strings.Contains(got, "403 Forbidden") {
		t.Fatalf("expected 403 error, got %q", got)
	}
}

func TestGetLatestAppReleaseVersion_NoStableRelease(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		_, err := w.Write([]byte(`[
			{"tag_name":"v9.9.9-rc1","prerelease":true,"draft":false},
			{"tag_name":"v9.9.9-draft","prerelease":false,"draft":true}
		]`))
		if err != nil {
			t.Fatalf("failed to write test response: %v", err)
		}
	}))
	defer server.Close()

	_, err := getLatestAppReleaseVersionFromURL(server.URL, server.Client())
	if err == nil {
		t.Fatal("expected an error, got nil")
	}

	if got := err.Error(); got != "no stable release found" {
		t.Fatalf("expected %q, got %q", "no stable release found", got)
	}
}
