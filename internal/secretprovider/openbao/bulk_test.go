package openbao

import (
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

// mockKV serves a minimal KVv2 mount named "kv". Each entry maps a secret path to its data.
type mockKV struct {
	secrets map[string]map[string]any
	// denyList makes list requests fail with a permission error.
	denyList bool

	mu       sync.Mutex
	requests []string
}

func (m *mockKV) provider(t *testing.T) *Provider {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(server.Close)

	provider, err := NewProvider(t.Context(), server.URL, "token")
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	return provider
}

func (m *mockKV) handle(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/v1/")

	m.mu.Lock()
	m.requests = append(m.requests, path)
	m.mu.Unlock()

	switch {
	case strings.HasPrefix(path, "kv/metadata"):
		if m.denyList {
			w.WriteHeader(http.StatusForbidden)

			return
		}

		m.list(w, strings.Trim(strings.TrimPrefix(path, "kv/metadata"), "/"))
	case strings.HasPrefix(path, "kv/data/"):
		data, ok := m.secrets[strings.TrimPrefix(path, "kv/data/")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)

			return
		}

		writeJSON(w, map[string]any{"data": map[string]any{"data": data, "metadata": map[string]any{"version": 1}}})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (m *mockKV) list(w http.ResponseWriter, folder string) {
	prefix := ""
	if folder != "" {
		prefix = folder + "/"
	}

	children := make(map[string]struct{})

	for secretPath := range m.secrets {
		rest, ok := strings.CutPrefix(secretPath, prefix)
		if !ok || rest == "" {
			continue
		}

		if name, _, nested := strings.Cut(rest, "/"); nested {
			children[name+"/"] = struct{}{}
		} else {
			children[name] = struct{}{}
		}
	}

	if len(children) == 0 {
		w.WriteHeader(http.StatusNotFound)

		return
	}

	keys := slices.Sorted(maps.Keys(children))

	writeJSON(w, map[string]any{"data": map[string]any{"keys": keys}})
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")

	_ = json.NewEncoder(w).Encode(body)
}

func newTestKV() *mockKV {
	return &mockKV{secrets: map[string]map[string]any{
		"apps/db":          {"password": "one", "user": "admin", "port": float64(5432)},
		"apps/web":         {"token": "two"},
		"apps/web/nested":  {"key": "three"},
		"apps/deep/a/b/c":  {"secret": "four"},
		"standalone/creds": {"api": "five"},
	}}
}

func TestProvider_GetSecretsByPath_Folder(t *testing.T) {
	t.Parallel()

	provider := newTestKV().provider(t)

	got, err := provider.GetSecretsByPath(t.Context(), "kv:kv:apps", false)
	if err != nil {
		t.Fatalf("GetSecretsByPath() error = %v", err)
	}

	want := map[string]string{"db/password": "one", "db/user": "admin", "web/token": "two"}
	if !maps.Equal(got, want) {
		t.Errorf("GetSecretsByPath() = %v, want %v", got, want)
	}
}

func TestProvider_GetSecretsByPath_Recursive(t *testing.T) {
	t.Parallel()

	mock := newTestKV()
	provider := mock.provider(t)

	got, err := provider.GetSecretsByPath(t.Context(), "kv:root:kv:apps/", true)
	if err != nil {
		t.Fatalf("GetSecretsByPath() error = %v", err)
	}

	reads := 0

	for _, request := range mock.requests {
		if request == "kv/data/apps/web" {
			reads++
		}
	}

	if reads != 1 {
		t.Errorf("apps/web was read %d times, want 1", reads)
	}

	want := map[string]string{
		"db/password":       "one",
		"db/user":           "admin",
		"web/token":         "two",
		"web/nested/key":    "three",
		"deep/a/b/c/secret": "four",
	}
	if !maps.Equal(got, want) {
		t.Errorf("GetSecretsByPath() = %v, want %v", got, want)
	}
}

func TestProvider_GetSecretsByPath_SingleSecret(t *testing.T) {
	t.Parallel()

	provider := newTestKV().provider(t)

	got, err := provider.GetSecretsByPath(t.Context(), "kv:kv:standalone/creds", false)
	if err != nil {
		t.Fatalf("GetSecretsByPath() error = %v", err)
	}

	if !maps.Equal(got, map[string]string{"api": "five"}) {
		t.Errorf("GetSecretsByPath() = %v", got)
	}
}

func TestProvider_GetSecretsByPath_Missing(t *testing.T) {
	t.Parallel()

	got, err := newTestKV().provider(t).GetSecretsByPath(t.Context(), "kv:kv:does/not/exist", true)
	if err != nil {
		t.Fatalf("GetSecretsByPath() error = %v", err)
	}

	if len(got) != 0 {
		t.Errorf("expected no secrets, got %v", got)
	}
}

func TestProvider_GetSecretsByPath_ListDenied(t *testing.T) {
	t.Parallel()

	mock := newTestKV()
	mock.denyList = true

	if _, err := mock.provider(t).GetSecretsByPath(t.Context(), "kv:kv:apps", true); err == nil {
		t.Fatal("expected an error when listing is denied")
	}
}

func TestProvider_GetSecretsByPath_DuplicateName(t *testing.T) {
	t.Parallel()

	mock := &mockKV{secrets: map[string]map[string]any{
		"apps":       {"db/password": "one"},
		"apps/db":    {"password": "two"},
		"apps/other": {"key": "three"},
	}}

	if _, err := mock.provider(t).GetSecretsByPath(t.Context(), "kv:kv:apps", false); !errors.Is(err, ErrDuplicateSecretName) {
		t.Fatalf("expected ErrDuplicateSecretName, got %v", err)
	}
}

func TestParseBulkPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		path          string
		wantNamespace string
		wantEngine    string
		wantPath      string
		wantErr       bool
	}{
		{path: "kv:secret:apps/web", wantNamespace: "root", wantEngine: "secret", wantPath: "apps/web"},
		{path: "kv:team:secret:apps", wantNamespace: "team", wantEngine: "secret", wantPath: "apps"},
		{path: "kv:secret:", wantNamespace: "root", wantEngine: "secret", wantPath: ""},
		{path: "pki:secret:apps", wantErr: true},
		{path: "kv:secret", wantErr: true},
		{path: "kv::apps", wantErr: true},
		{path: "kv:a:b:c:d", wantErr: true},
		{path: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			t.Parallel()

			namespace, engine, path, err := parseBulkPath(tt.path)
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidSecretReference) {
					t.Fatalf("expected ErrInvalidSecretReference, got %v", err)
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if namespace != tt.wantNamespace || engine != tt.wantEngine || path != tt.wantPath {
				t.Errorf("got (%q, %q, %q)", namespace, engine, path)
			}
		})
	}
}
