package webhook

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestValueProvider_GetSecret_WebhookStores(t *testing.T) {
	ts := httptest.NewServer(newMockHandler())
	defer ts.Close()

	stores := `stores:
  login:
    version: v1
    url: "` + ts.URL + `/get/{{ .remote_ref.key }}"
    method: GET
    headers:
      Content-Type: application/json
    json_path: "result.{{ .remote_ref.property }}"
  fields:
    version: v1
    url: "` + ts.URL + `/get/{{ .remote_ref.key }}"
    method: GET
    json_path: "fields[?name=='{{ .remote_ref.property }}'].value"
  post-auth:
    version: v1
    url: "` + ts.URL + `/post"
    method: POST
    headers:
      Authorization: 'Basic {{ print .auth.username ":" .auth.password | b64enc }}'
      Content-Type: application/json
    body: '{"secret":"{{ .remote_ref.key }}"}'
    json_path: "result.value"
`

	t.Setenv("SECRET_PROVIDER_WEBHOOK_STORES", stores)
	t.Setenv("SECRET_PROVIDER_AUTH_USERNAME", "username")
	t.Setenv("SECRET_PROVIDER_AUTH_PASSWORD", "password")

	cfg, err := GetConfig()
	if err != nil {
		t.Fatalf("Unwanted config error: %v", err)
	}

	subject, err := NewValueProvider(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Failed to create Webhook value provider: %v", err)
	}

	t.Run("login_username", func(t *testing.T) {
		ref := mustJSONRef(t, SecretRefPayload{
			StoreRef: "login",
			RemoteRef: map[string]any{
				"key":      "user-1",
				"property": "username",
			},
		})

		got, err := subject.GetSecret(t.Context(), ref)
		if err != nil {
			t.Fatalf("Unwanted error: %v", err)
		}

		if got != "alice" {
			t.Fatalf("got %q, want %q", got, "alice")
		}
	})

	t.Run("login_username_snake_case_ref", func(t *testing.T) {
		ref := `{"store_ref":"login","remote_ref":{"key":"user-1","property":"username"}}`

		got, err := subject.GetSecret(t.Context(), ref)
		if err != nil {
			t.Fatalf("Unwanted error: %v", err)
		}

		if got != "alice" {
			t.Fatalf("got %q, want %q", got, "alice")
		}
	})

	t.Run("fields_first_result", func(t *testing.T) {
		ref := mustJSONRef(t, SecretRefPayload{
			StoreRef: "fields",
			RemoteRef: map[string]any{
				"key":      "user-1",
				"property": "api_key",
			},
		})

		got, err := subject.GetSecret(t.Context(), ref)
		if err != nil {
			t.Fatalf("Unwanted error: %v", err)
		}

		if got != "abc123" {
			t.Fatalf("got %q, want %q", got, "abc123")
		}
	})

	t.Run("post_with_basic_auth", func(t *testing.T) {
		ref := mustJSONRef(t, SecretRefPayload{
			StoreRef: "post-auth",
			RemoteRef: map[string]any{
				"key": "token",
			},
		})

		got, err := subject.GetSecret(t.Context(), ref)
		if err != nil {
			t.Fatalf("Unwanted error: %v", err)
		}

		if got != "Basic dXNlcm5hbWU6cGFzc3dvcmQ=" {
			t.Fatalf("got %q, want %q", got, "Basic dXNlcm5hbWU6cGFzc3dvcmQ=")
		}
	})

	t.Run("unknown_store", func(t *testing.T) {
		ref := mustJSONRef(t, SecretRefPayload{StoreRef: "missing"})
		if _, err := subject.GetSecret(t.Context(), ref); err == nil {
			t.Fatalf("expected error")
		}
	})

	t.Run("legacy_ref_hard_break", func(t *testing.T) {
		if _, err := subject.GetSecret(t.Context(), "legacy-id"); err == nil {
			t.Fatalf("expected legacy format error")
		}
	})

	t.Run("missing_remote_ref_field", func(t *testing.T) {
		ref := mustJSONRef(t, SecretRefPayload{
			StoreRef: "login",
			RemoteRef: map[string]any{
				"key": "user-1",
			},
		})
		if _, err := subject.GetSecret(t.Context(), ref); err == nil {
			t.Fatalf("expected missing field error")
		}
	})
}

func TestGetConfig_WebhookStores_MultiDocument(t *testing.T) {
	stores := `
---
stores:
  login:
    version: v1
    url: "https://example.com/{{ .remote_ref.key }}"
    method: GET
    json_path: "result.{{ .remote_ref.property }}"
---
name: fields
version: v1
url: "https://example.com/{{ .remote_ref.key }}"
method: GET
json_path: "result"
`

	t.Setenv("SECRET_PROVIDER_WEBHOOK_STORES", stores)

	cfg, err := GetConfig()
	if err != nil {
		t.Fatalf("unexpected config error: %v", err)
	}

	if len(cfg.Stores) != 2 {
		t.Fatalf("expected 2 stores, got %d", len(cfg.Stores))
	}
}

func TestGetConfig_WebhookStores_DefaultVersion(t *testing.T) {
	// version field omitted — should default to v1
	stores := `stores:
  no-version:
    url: "https://example.com/{{ .remote_ref.key }}"
    method: GET
    json_path: "result"
`

	t.Setenv("SECRET_PROVIDER_WEBHOOK_STORES", stores)

	cfg, err := GetConfig()
	if err != nil {
		t.Fatalf("unexpected config error when version is omitted: %v", err)
	}

	store, ok := cfg.Stores["no-version"]
	if !ok {
		t.Fatalf("expected store 'no-version' to be present")
	}

	if store.Version != StoreVersionV1 {
		t.Fatalf("expected version to default to %q, got %q", StoreVersionV1, store.Version)
	}
}

func TestValueProvider_newRequest_BodyNotCorruptedByTemplateRendering(t *testing.T) {
	stores := `stores:
  post-auth:
    version: v1
    url: "https://example.com/post"
    method: POST
    headers:
      Authorization: 'Basic {{ print .auth.username ":" .auth.password | b64enc }}'
      Content-Type: application/json
    body: '{"secret":"{{ .remote_ref.key }}"}'
    json_path: "x"
`

	t.Setenv("SECRET_PROVIDER_WEBHOOK_STORES", stores)
	t.Setenv("SECRET_PROVIDER_AUTH_USERNAME", "username")
	t.Setenv("SECRET_PROVIDER_AUTH_PASSWORD", "password")

	cfg, err := GetConfig()
	if err != nil {
		t.Fatalf("unexpected config error: %v", err)
	}

	subject, err := NewValueProvider(t.Context(), cfg)
	if err != nil {
		t.Fatalf("failed to create webhook value provider: %v", err)
	}

	store := cfg.Stores["post-auth"]

	req, renderedJSONPath, err := subject.newRequest(t.Context(), store, map[string]any{"key": "token"})
	if err != nil {
		t.Fatalf("unexpected request creation error: %v", err)
	}

	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("failed to read request body: %v", err)
	}

	if got, want := string(body), `{"secret":"token"}`; got != want {
		t.Fatalf("got body %q, want %q", got, want)
	}

	if got, want := renderedJSONPath, "x"; got != want {
		t.Fatalf("got json_path %q, want %q", got, want)
	}

	if got, want := req.ContentLength, int64(len(body)); got != want {
		t.Fatalf("got content length %d, want %d", got, want)
	}
}

func mustJSONRef(t *testing.T, ref SecretRefPayload) string {
	t.Helper()

	b, err := json.Marshal(ref)
	if err != nil {
		t.Fatalf("failed to marshal secret ref: %v", err)
	}

	return string(b)
}

func newMockHandler() http.Handler {
	handler := http.NewServeMux()

	handler.HandleFunc("/post", postSecret)
	handler.HandleFunc("/get/{secret}", getSecret)

	return handler
}

func getSecret(w http.ResponseWriter, r *http.Request) {
	lookup := r.PathValue("secret")

	body := map[string]any{
		"result": map[string]string{
			"username": "alice",
			"password": "pw-123",
			"value":    r.Header.Get("Authorization"),
		},
		"fields": []map[string]string{
			{"name": "api_key", "value": "abc123"},
			{"name": "other", "value": "zzz"},
		},
		"lookup": lookup,
	}

	w.Header().Set(HeaderContentType, ContentTypeJSON+"; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(body)
}

func postSecret(w http.ResponseWriter, r *http.Request) {
	var payload map[string]string

	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if payload["secret"] != "token" {
		http.Error(w, "unexpected secret payload", http.StatusBadRequest)
		return
	}

	body := map[string]any{
		"result": map[string]string{
			"value": r.Header.Get("Authorization"),
		},
	}

	w.Header().Set(HeaderContentType, ContentTypeJSON+"; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(body)
}
