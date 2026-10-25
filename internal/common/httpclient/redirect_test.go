package httpclient

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
)

func TestSameOriginRedirects(t *testing.T) {
	t.Parallel()

	first := mustParse(t, "https://api.example.com/repos/a/b")

	testCases := []struct {
		name    string
		target  string
		wantErr bool
	}{
		{name: "same origin", target: "https://api.example.com/repositories/1"},
		{name: "explicit default port", target: "https://API.example.com:443/repositories/1"},
		{name: "other host", target: "https://evil.example.net/x", wantErr: true},
		{name: "subdomain", target: "https://x.api.example.com/x", wantErr: true},
		{name: "other port", target: "https://api.example.com:8443/x", wantErr: true},
		{name: "scheme downgrade", target: "http://api.example.com/x", wantErr: true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := &http.Request{URL: mustParse(t, tc.target)}

			err := SameOriginRedirects(req, []*http.Request{{URL: first}})
			if tc.wantErr != (err != nil) {
				t.Fatalf("SameOriginRedirects() error = %v, want error %v", err, tc.wantErr)
			}

			if tc.wantErr && !errors.Is(err, ErrCrossOriginRedirect) {
				t.Fatalf("error = %v, want ErrCrossOriginRedirect", err)
			}
		})
	}
}

func TestSameOriginRedirects_ClientDoesNotContactOtherOrigin(t *testing.T) {
	t.Parallel()

	var targetHits atomic.Int32

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/moved":
			http.Redirect(w, r, "/final", http.StatusFound)
		case "/final":
			w.WriteHeader(http.StatusOK)
		case "/loop":
			http.Redirect(w, r, "/loop", http.StatusFound)
		default:
			http.Redirect(w, r, target.URL+"/steal", http.StatusTemporaryRedirect)
		}
	}))
	t.Cleanup(origin.Close)

	client := &http.Client{CheckRedirect: SameOriginRedirects}

	do := func(path string) error {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, origin.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}

		req.Header.Set("Authorization", "Bearer sentinel")

		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}

		return err
	}

	if err := do("/moved"); err != nil {
		t.Fatalf("same-origin redirect failed: %v", err)
	}

	if err := do("/cross"); !errors.Is(err, ErrCrossOriginRedirect) {
		t.Fatalf("cross-origin redirect error = %v, want ErrCrossOriginRedirect", err)
	}

	if hits := targetHits.Load(); hits != 0 {
		t.Fatalf("redirect target received %d requests, want 0", hits)
	}

	if err := do("/loop"); err == nil {
		t.Fatal("redirect loop succeeded, want error")
	}
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()

	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}

	return u
}
