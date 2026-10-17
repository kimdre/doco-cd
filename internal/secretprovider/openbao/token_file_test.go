package openbao

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
)

func writeTokenFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil { // #nosec G703 -- Tests supply paths inside t.TempDir.
		t.Fatal(err)
	}
}

func tokenFileProvider(t *testing.T, address, path string) *Provider {
	t.Helper()

	provider, err := NewProviderWithConfig(t.Context(), &Config{SiteUrl: address, AccessTokenFile: path})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(provider.Close)

	return provider
}

func tokenFileOperations(t *testing.T) map[string]func(*Provider) error {
	t.Helper()

	return map[string]func(*Provider) error{
		"KV lookup": func(p *Provider) error {
			got, err := p.GetSecret(t.Context(), "kv:team:secret:app:value")
			if err == nil && got != "secret-value" {
				t.Errorf("GetSecret() = %q, want secret-value", got)
			}

			return err
		},
		"PKI lookup": func(p *Provider) error {
			got, err := p.GetSecret(t.Context(), "pki:team:pki:app.example.com")
			if err == nil && got != testLeafPEM {
				t.Error("GetSecret() did not return the certificate")
			}

			return err
		},
		"PKI issuance": func(p *Provider) error {
			got, err := p.GetSecret(t.Context(), "pki-role:team:pki:app:app.example.com")
			if err == nil && got != testLeafPEM {
				t.Error("GetSecret() did not return the issued certificate")
			}

			return err
		},
		"batch lookup": func(p *Provider) error {
			got, err := p.GetSecrets(t.Context(), []string{"kv:team:secret:app:value", "pki:team:pki:app.example.com", "pki-role:team:pki:app:app.example.com"})
			if err == nil && len(got) != 3 {
				t.Errorf("GetSecrets() returned %d values, want 3", len(got))
			}

			return err
		},
		"reference resolution": func(p *Provider) error {
			got, err := p.ResolveSecretReferences(t.Context(), map[string]string{
				"VALUE":  "kv:team:secret:app:value",
				"CERT":   "pki:team:pki:app.example.com",
				"ISSUED": "pki-role:team:pki:app:app.example.com",
			})
			if err == nil && (got["VALUE"] != "secret-value" || got["CERT_FULL"] != testLeafPEM+"\n"+testRootPEM || got["ISSUED_KEY"] != "private-key") {
				t.Error("ResolveSecretReferences() did not return the secret, chain, and private key")
			}

			return err
		},
		"revocation check": func(p *Provider) error {
			got, err := p.DeploymentHasRevokedCertificate(t.Context(), `[{"ref":"pki-role:team:pki:app:app.example.com","serial":"aa:bb"}]`)
			if err == nil && !got {
				t.Error("DeploymentHasRevokedCertificate() did not report the revoked certificate")
			}

			return err
		},
	}
}

func serveTokenFileSecrets(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()

	var data map[string]any

	switch r.URL.Path {
	case "/v1/secret/data/app":
		data = map[string]any{"data": map[string]string{"value": "secret-value"}}
	case "/v1/pki/certs/detailed":
		data = map[string]any{"key_info": map[string]any{"aa:bb": map[string]string{"common_name": "app.example.com"}}}
	case "/v1/pki/cert/aa:bb":
		data = map[string]any{"certificate": testLeafPEM, "ca_chain": []string{testRootPEM}}
	case "/v1/pki/issue/app":
		data = map[string]any{"certificate": testLeafPEM, "private_key": "private-key", "ca_chain": []string{testRootPEM}, "expiration": 2000000000}
	case "/v1/pki/certs/revoked":
		data = map[string]any{"keys": []string{"aa:bb"}}
	default:
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)

		return
	}

	w.Header().Set("Content-Type", "application/json")

	if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
		t.Error(err)
	}
}

func TestProviderTokenRotation_AllOperations(t *testing.T) {
	t.Parallel()

	for name := range tokenFileOperations(t) {
		t.Run(name, func(t *testing.T) {
			run := tokenFileOperations(t)[name]

			var expected atomic.Value
			expected.Store("token-A")

			var requests atomic.Int32

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)

				if got := r.Header.Get("X-Vault-Token"); got != expected.Load().(string) {
					t.Error("request used an outdated token")
					http.Error(w, `{"errors":["permission denied"]}`, http.StatusForbidden)

					return
				}

				if got := r.Header.Get("X-Vault-Namespace"); got != "team" {
					t.Errorf("request namespace = %q, want team", got)
				}

				serveTokenFileSecrets(t, w, r)
			}))
			t.Cleanup(server.Close)

			path := filepath.Join(t.TempDir(), "token")
			writeTokenFile(t, path, "token-A")
			provider := tokenFileProvider(t, server.URL, path)

			if err := run(provider); err != nil {
				t.Fatal(err)
			}

			before := requests.Load()

			writeTokenFile(t, path, " \ttoken-B\n")
			expected.Store("token-B")

			if err := run(provider); err != nil {
				t.Fatalf("operation after rotation: %v", err)
			}

			if requests.Load() == before {
				t.Error("operation after rotation sent no requests")
			}

			if provider.Client.Token() != "token-A" || provider.issuer.Token() != "token-A" {
				t.Error("operation mutated a shared client token")
			}
		})
	}
}

func TestProviderTokenFile_Replacement(t *testing.T) {
	t.Parallel()

	for _, symlink := range []bool{false, true} {
		t.Run(fmt.Sprintf("symlink=%t", symlink), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "token")
			writeTokenFile(t, path, "token-A")

			if symlink {
				link := filepath.Join(dir, "current")
				if err := os.Symlink(path, link); err != nil {
					t.Fatal(err)
				}

				path = link
			}

			var expected atomic.Value
			expected.Store("token-A")

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Vault-Token") != expected.Load().(string) {
					t.Error("request used an outdated token")
				}

				serveTokenFileSecrets(t, w, r)
			}))
			t.Cleanup(server.Close)

			provider := tokenFileProvider(t, server.URL, path)
			if _, err := provider.GetSecret(t.Context(), "kv:secret:app:value"); err != nil {
				t.Fatal(err)
			}

			replacement := filepath.Join(dir, "replacement")
			if symlink {
				target := filepath.Join(dir, "next-token")
				writeTokenFile(t, target, "token-B")

				if err := os.Symlink(target, replacement); err != nil {
					t.Fatal(err)
				}
			} else {
				writeTokenFile(t, replacement, "token-B")
			}

			if err := os.Rename(replacement, path); err != nil {
				t.Fatal(err)
			}

			expected.Store("token-B")

			if _, err := provider.GetSecret(t.Context(), "kv:secret:app:value"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestProviderTokenFile_FailureAndRecovery(t *testing.T) {
	t.Parallel()

	for _, failure := range []string{"missing", "directory", "empty", "whitespace"} {
		t.Run(failure, func(t *testing.T) {
			var requests atomic.Int32

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)

				if r.Header.Get("X-Vault-Token") != "recovered-token" {
					t.Error("request did not use the recovered token")
				}

				serveTokenFileSecrets(t, w, r)
			}))
			t.Cleanup(server.Close)

			path := filepath.Join(t.TempDir(), "token")
			writeTokenFile(t, path, "startup-token")
			provider := tokenFileProvider(t, server.URL, path)

			switch failure {
			case "missing", "directory":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}

				if failure == "directory" {
					if err := os.Mkdir(path, 0o700); err != nil {
						t.Fatal(err)
					}
				}
			case "empty":
				writeTokenFile(t, path, "")
			case "whitespace":
				writeTokenFile(t, path, " \n\t")
			}

			for name, run := range tokenFileOperations(t) {
				if err := run(provider); err == nil || !strings.Contains(err.Error(), "SECRET_PROVIDER_ACCESS_TOKEN_FILE") {
					t.Errorf("%s error = %v, want contextual file error", name, err)
				}
			}

			if requests.Load() != 0 {
				t.Error("invalid token file sent a request using cached credentials")
			}

			if failure == "directory" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}

			writeTokenFile(t, path, "recovered-token")

			for name, run := range tokenFileOperations(t) {
				if err := run(provider); err != nil {
					t.Errorf("%s after recovery: %v", name, err)
				}
			}
		})
	}
}

func TestProviderTokenFile_ConcurrentNamespaces(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "token")
	writeTokenFile(t, path, "token-A")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		namespace := r.Header.Get("X-Vault-Namespace")
		if !strings.HasPrefix(namespace, "team-") || r.Header.Get("X-Vault-Token") != "token-B" {
			t.Error("concurrent request used the wrong credentials or namespace")
		}

		w.Header().Set("Content-Type", "application/json")

		if err := json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": map[string]string{"value": namespace}}}); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)

	provider := tokenFileProvider(t, server.URL, path)
	writeTokenFile(t, path, "token-B")

	var wg sync.WaitGroup

	for i := range 32 {
		wg.Go(func() {
			namespace := fmt.Sprintf("team-%d", i)

			got, err := provider.GetSecret(t.Context(), "kv:"+namespace+":secret:app:value")
			if err != nil || got != namespace {
				t.Errorf("concurrent lookup = %q, %v, want namespace %s", got, err, namespace)
			}
		})
	}

	wg.Wait()

	if provider.Client.Token() != "token-A" || provider.Client.Namespace() != "" {
		t.Error("concurrent operations mutated the shared client")
	}
}

func TestProviderTokenFile_IssuanceNotRetried(t *testing.T) {
	t.Parallel()

	for _, status := range []int{http.StatusForbidden, http.StatusServiceUnavailable} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var requests atomic.Int32

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)

				if r.Header.Get("X-Vault-Token") != "token-B" {
					t.Error("issuance did not use the rotated token")
				}

				http.Error(w, `{"errors":["issuance failed"]}`, status)
			}))
			t.Cleanup(server.Close)

			path := filepath.Join(t.TempDir(), "token")
			writeTokenFile(t, path, "token-A")
			provider := tokenFileProvider(t, server.URL, path)
			writeTokenFile(t, path, "token-B")

			_, err := provider.ResolveSecretReferences(t.Context(), map[string]string{"CERT": "pki-role:pki:app:app.example.com"})
			if !errors.Is(err, secrettypes.ErrNotRetryable) {
				t.Fatalf("issuance error = %v, want ErrNotRetryable", err)
			}

			if requests.Load() != 1 {
				t.Errorf("issuance sent %d requests, want 1", requests.Load())
			}
		})
	}
}

func TestProviderTokenFile_StaticToken(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Vault-Token") != "static-token" {
			t.Error("request did not use the static token")
		}

		serveTokenFileSecrets(t, w, r)
	}))
	t.Cleanup(server.Close)

	provider, err := NewProviderWithConfig(t.Context(), &Config{SiteUrl: server.URL, AccessToken: "static-token"})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(provider.Close)

	for name, run := range tokenFileOperations(t) {
		if err := run(provider); err != nil {
			t.Errorf("%s with a static token: %v", name, err)
		}
	}
}
