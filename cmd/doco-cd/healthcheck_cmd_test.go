package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/kimdre/doco-cd/internal/api"
)

func TestRunHealthcheck(t *testing.T) {
	t.Parallel()

	var requestedPath string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedPath = r.URL.Path

		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}

	env := map[string]string{
		"HTTP_PORT": serverURL.Port(),
		"LOG_LEVEL": "error",
		// Settings the healthcheck does not need must not break it.
		"POLL_CONFIG": "{invalid",
	}

	lookupEnv := func(key string) (string, bool) {
		value, ok := env[key]
		return value, ok
	}

	if err = runHealthcheck(context.Background(), lookupEnv); err != nil {
		t.Fatalf("runHealthcheck() error = %v", err)
	}

	if requestedPath != api.HealthPath {
		t.Errorf("requested path = %q, want %q", requestedPath, api.HealthPath)
	}

	env["HTTP_PORT"] = "invalid"

	if err = runHealthcheck(context.Background(), lookupEnv); err == nil {
		t.Fatal("runHealthcheck() with invalid HTTP_PORT succeeded, want error")
	}
}
