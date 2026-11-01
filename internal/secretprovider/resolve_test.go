package secretprovider

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"maps"
	"strings"
	"testing"

	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
)

// bulkMockProvider adds the bulk capability to mockSecretProvider.
type bulkMockProvider struct {
	mockSecretProvider

	byPath map[string]map[string]string
	err    error
	calls  []string
}

func (b *bulkMockProvider) GetSecretsByPath(_ context.Context, path string, recursive bool) (map[string]string, error) {
	b.calls = append(b.calls, path)

	if recursive {
		b.calls = append(b.calls, "recursive")
	}

	if b.err != nil {
		return nil, b.err
	}

	return b.byPath[path], nil
}

func newBulkMock(byPath map[string]map[string]string) *bulkMockProvider {
	m := &bulkMockProvider{byPath: byPath}
	m.name = "bulk"
	m.resolveSecretRefsFunc = func(_ context.Context, secrets map[string]string) (secrettypes.ResolvedSecrets, error) {
		out := make(secrettypes.ResolvedSecrets, len(secrets))
		for k, v := range secrets {
			out[k] = "resolved:" + v
		}

		return out, nil
	}

	return m
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestResolveExternalSecrets_BulkOnly(t *testing.T) {
	t.Parallel()

	mock := newBulkMock(map[string]map[string]string{
		"apps/": {"db-password": "one", "api/key": "two"},
	})

	got, err := ResolveExternalSecrets(t.Context(), mock, nil,
		[]secrettypes.ExternalSecretsPath{{Path: "apps/", Prefix: "APP_", Recursive: true}},
		ResolveOptions{}, discardLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got.Values["APP_DB_PASSWORD"] != "one" || got.Values["APP_API_KEY"] != "two" || len(got.Values) != 2 {
		t.Errorf("unexpected values: %v", got.Values)
	}

	if calls := mock.resolveSecretRefsCalls.Load(); calls != 0 {
		t.Errorf("ResolveSecretReferences must not run without explicit refs, got %d calls", calls)
	}

	if strings.Join(mock.calls, ",") != "apps/,recursive" {
		t.Errorf("unexpected provider calls: %v", mock.calls)
	}
}

func TestResolveExternalSecrets_ExplicitWinsOverBulk(t *testing.T) {
	t.Parallel()

	mock := newBulkMock(map[string]map[string]string{
		"apps/": {"db_password": "bulk", "other": "x"},
	})

	refs := map[string]secrettypes.ExternalSecretRef{"DB_PASSWORD": {LegacyRef: "ref-id"}}

	got, err := ResolveExternalSecrets(t.Context(), mock, refs,
		[]secrettypes.ExternalSecretsPath{{Path: "apps/"}}, ResolveOptions{}, discardLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got.Values["DB_PASSWORD"] != "resolved:ref-id" {
		t.Errorf("explicit reference must win, got %q", got.Values["DB_PASSWORD"])
	}

	if got.Values["OTHER"] != "x" {
		t.Errorf("bulk value missing: %v", got.Values)
	}

	if len(got.Refs) != 1 {
		t.Errorf("unexpected refs: %v", got.Refs)
	}
}

func TestResolveExternalSecrets_CollisionBetweenPaths(t *testing.T) {
	t.Parallel()

	mock := newBulkMock(map[string]map[string]string{
		"a/": {"token": "leak-one"},
		"b/": {"TOKEN": "leak-two"},
	})

	_, err := ResolveExternalSecrets(t.Context(), mock, nil,
		[]secrettypes.ExternalSecretsPath{{Path: "a/"}, {Path: "b/"}}, ResolveOptions{}, discardLogger())
	if err == nil {
		t.Fatal("expected a collision error")
	}

	if strings.Contains(err.Error(), "leak-") {
		t.Errorf("error should not leak values: %v", err)
	}
}

func TestResolveExternalSecrets_EmptyPathIsNotAnError(t *testing.T) {
	t.Parallel()

	mock := newBulkMock(map[string]map[string]string{})

	got, err := ResolveExternalSecrets(t.Context(), mock, nil,
		[]secrettypes.ExternalSecretsPath{{Path: "empty/"}}, ResolveOptions{}, discardLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(got.Values) != 0 {
		t.Errorf("expected no values, got %v", got.Values)
	}
}

func TestResolveExternalSecrets_ProviderWithoutBulkSupport(t *testing.T) {
	t.Parallel()

	mock := &mockSecretProvider{name: "plain"}

	_, err := ResolveExternalSecrets(t.Context(), mock, nil,
		[]secrettypes.ExternalSecretsPath{{Path: "apps/"}}, ResolveOptions{}, discardLogger())
	if !errors.Is(err, secrettypes.ErrBulkNotSupported) {
		t.Fatalf("expected ErrBulkNotSupported, got %v", err)
	}
}

func TestResolveExternalSecrets_ProviderError(t *testing.T) {
	t.Parallel()

	mock := newBulkMock(nil)
	mock.err = errors.New("access denied: insufficient permissions")

	_, err := ResolveExternalSecrets(t.Context(), mock, nil,
		[]secrettypes.ExternalSecretsPath{{Path: "apps/"}}, ResolveOptions{}, discardLogger())
	if err == nil || !strings.Contains(err.Error(), "apps/") {
		t.Fatalf("expected an error naming the path, got %v", err)
	}
}

func TestResolveExternalSecrets_InterpolatesPathAndValues(t *testing.T) {
	t.Setenv("BULK_RESOLVE_STAGE", "lab")

	mock := newBulkMock(map[string]map[string]string{
		"apps/lab/": {"user": "admin", "url": "http://${USER}@host"},
	})

	got, err := ResolveExternalSecrets(t.Context(), mock, nil,
		[]secrettypes.ExternalSecretsPath{{Path: "apps/${BULK_RESOLVE_STAGE}/"}},
		ResolveOptions{InterpolateRefs: true, InterpolateResolved: true}, discardLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got.Values["URL"] != "http://admin@host" {
		t.Errorf("resolved values must reference other secrets, got %q", got.Values["URL"])
	}
}

func TestRetryingSecretProvider_GetSecretsByPath(t *testing.T) {
	t.Parallel()

	t.Run("forwards to bulk capable provider", func(t *testing.T) {
		t.Parallel()

		mock := newBulkMock(map[string]map[string]string{"p/": {"k": "v"}})

		got, err := NewRetryingSecretProvider(mock).GetSecretsByPath(t.Context(), "p/", false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if got["k"] != "v" {
			t.Errorf("unexpected result: %v", got)
		}
	})

	t.Run("not supported", func(t *testing.T) {
		t.Parallel()

		_, err := NewRetryingSecretProvider(&mockSecretProvider{name: "plain"}).GetSecretsByPath(t.Context(), "p/", false)
		if !errors.Is(err, secrettypes.ErrBulkNotSupported) {
			t.Fatalf("expected ErrBulkNotSupported, got %v", err)
		}
	})

	t.Run("retries transient errors", func(t *testing.T) {
		t.Parallel()

		mock := newBulkMock(map[string]map[string]string{"p/": {"k": "v"}})
		attempts := 0
		wrapped := &flakyBulkProvider{bulkMockProvider: mock, failures: 1, attempts: &attempts}

		got, err := NewRetryingSecretProvider(wrapped).GetSecretsByPath(t.Context(), "p/", false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if got["k"] != "v" || attempts != 2 {
			t.Errorf("got %v after %d attempts", got, attempts)
		}
	})
}

// flakyBulkProvider fails with a rate limit error before it succeeds.
type flakyBulkProvider struct {
	*bulkMockProvider

	failures int
	attempts *int
}

func (f *flakyBulkProvider) GetSecretsByPath(ctx context.Context, path string, recursive bool) (map[string]string, error) {
	*f.attempts++

	if *f.attempts <= f.failures {
		return nil, errRateLimit
	}

	return f.bulkMockProvider.GetSecretsByPath(ctx, path, recursive)
}

type plainValueProvider struct{}

func (plainValueProvider) GetSecret(context.Context, string) (string, error) { return "", nil }

type bulkValueProvider struct{ plainValueProvider }

func (bulkValueProvider) GetSecretsByPath(_ context.Context, path string, _ bool) (map[string]string, error) {
	return map[string]string{"path": path}, nil
}

func TestSecretProviderAdapter_GetSecretsByPath(t *testing.T) {
	t.Parallel()

	got, err := AdaptSecretValueProvider("bulk", bulkValueProvider{}).GetSecretsByPath(t.Context(), "p/", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got["path"] != "p/" {
		t.Errorf("unexpected result: %v", got)
	}

	_, err = AdaptSecretValueProvider("plain", plainValueProvider{}).GetSecretsByPath(t.Context(), "p/", true)
	if !errors.Is(err, secrettypes.ErrBulkNotSupported) {
		t.Fatalf("expected ErrBulkNotSupported, got %v", err)
	}
}

func TestResolveExternalSecrets_DeferPKIRoleRefs(t *testing.T) {
	t.Parallel()

	mock := newBulkMock(nil)
	refs := map[string]secrettypes.ExternalSecretRef{
		"DB_HOST": {LegacyRef: "kv:secret:app:host"},
		"CERT":    {LegacyRef: "pki-role:pki:web:app.example"},
	}

	got, err := ResolveExternalSecrets(t.Context(), mock, refs, nil,
		ResolveOptions{DeferPKIRoleRefs: true}, discardLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := map[string]string{
		"DB_HOST":   "resolved:kv:secret:app:host",
		"CERT":      "pki-role:pki:web:app.example",
		"CERT_KEY":  "pki-role:pki:web:app.example_KEY",
		"CERT_FULL": "pki-role:pki:web:app.example_FULL",
	}
	if len(got.Values) != len(want) {
		t.Fatalf("unexpected values: %v", got.Values)
	}

	for name, value := range want {
		if got.Values[name] != value {
			t.Errorf("%s = %q, want %q", name, got.Values[name], value)
		}
	}

	if len(got.Refs) != 2 {
		t.Errorf("Refs must keep every reference, got %v", got.Refs)
	}

	if len(got.Deferred) != 1 || got.Deferred["CERT"].LegacyRef != "pki-role:pki:web:app.example" {
		t.Errorf("unexpected deferred refs: %v", got.Deferred)
	}

	issued, err := IssueDeferred(t.Context(), mock, got.Deferred)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if issued["CERT"] != "resolved:pki-role:pki:web:app.example" {
		t.Errorf("unexpected issued values: %v", issued)
	}

	if calls := mock.resolveSecretRefsCalls.Load(); calls != 2 {
		t.Errorf("expected one resolution for kv and one for the deferred pki-role, got %d", calls)
	}
}

func TestResolveExternalSecrets_DeferPKIRoleRefsOnly(t *testing.T) {
	t.Parallel()

	mock := newBulkMock(nil)
	refs := map[string]secrettypes.ExternalSecretRef{"CERT": {LegacyRef: "pki-role:pki:web:app.example"}}

	got, err := ResolveExternalSecrets(t.Context(), mock, refs, nil,
		ResolveOptions{DeferPKIRoleRefs: true}, discardLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if calls := mock.resolveSecretRefsCalls.Load(); calls != 0 {
		t.Errorf("a deferred pki-role must not reach the provider, got %d calls", calls)
	}

	if len(got.Deferred) != 1 {
		t.Errorf("unexpected deferred refs: %v", got.Deferred)
	}
}

// TestResolveExternalSecrets_DeferPKIRoleRefsWithPaths checks that a deferred pki-role reference
// keeps the precedence of an explicit reference over path secrets. Its placeholders replace path
// secrets with the same variable names. After issuance, the environment is the same as without
// deferral.
func TestResolveExternalSecrets_DeferPKIRoleRefsWithPaths(t *testing.T) {
	t.Parallel()

	const certRef = "pki-role:pki:web:app.example"

	newProvider := func() *bulkMockProvider {
		mock := newBulkMock(map[string]map[string]string{
			"apps/": {
				"cert":      "path-cert",
				"cert_key":  "path-key",
				"cert_full": "path-full",
				"db_user":   "path-user",
			},
		})
		// Like OpenBao, the provider returns the key and full chain with each issued certificate.
		mock.resolveSecretRefsFunc = func(_ context.Context, secrets map[string]string) (secrettypes.ResolvedSecrets, error) {
			out := make(secrettypes.ResolvedSecrets, len(secrets))

			for envVar, ref := range secrets {
				if !strings.HasPrefix(ref, secrettypes.PKIRoleRefPrefix) {
					out[envVar] = "resolved:" + ref

					continue
				}

				out[envVar] = "issued-cert:" + ref
				out[envVar+secrettypes.PKIRoleKeySuffix] = "issued-key:" + ref
				out[envVar+secrettypes.PKIFullChainSuffix] = "issued-full:" + ref
			}

			return out, nil
		}

		return mock
	}

	refs := map[string]secrettypes.ExternalSecretRef{
		"CERT":    {LegacyRef: certRef},
		"DB_HOST": {LegacyRef: "kv:secret:app:host"},
	}
	paths := []secrettypes.ExternalSecretsPath{{Path: "apps/"}}

	mock := newProvider()

	got, err := ResolveExternalSecrets(t.Context(), mock, refs, paths,
		ResolveOptions{DeferPKIRoleRefs: true}, discardLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wantPlaceholders := map[string]string{
		"CERT":      certRef,
		"CERT_KEY":  certRef + secrettypes.PKIRoleKeySuffix,
		"CERT_FULL": certRef + secrettypes.PKIFullChainSuffix,
		"DB_HOST":   "resolved:kv:secret:app:host",
		"DB_USER":   "path-user",
	}
	if !maps.Equal(got.Values, wantPlaceholders) {
		t.Errorf("placeholders must replace path secrets with the same names\ngot:  %v\nwant: %v", got.Values, wantPlaceholders)
	}

	if calls := mock.resolveSecretRefsCalls.Load(); calls != 1 {
		t.Errorf("only the kv reference may reach the provider before issuance, got %d calls", calls)
	}

	issued, err := IssueDeferred(t.Context(), mock, got.Deferred)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	deployed := maps.Clone(got.Values)
	maps.Copy(deployed, issued)

	eager, err := ResolveExternalSecrets(t.Context(), newProvider(), refs, paths, ResolveOptions{}, discardLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !maps.Equal(deployed, eager.Values) {
		t.Errorf("issued values must match resolution without deferral\ngot:  %v\nwant: %v", deployed, eager.Values)
	}

	if deployed["CERT_KEY"] != "issued-key:"+certRef {
		t.Errorf("CERT_KEY must come from the issued certificate, got %q", deployed["CERT_KEY"])
	}
}

func TestResolveExternalSecrets_DeferPKIRoleRefsFallsBack(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		refs map[string]secrettypes.ExternalSecretRef
		opts ResolveOptions
	}{
		{
			name: "interpolated values",
			refs: map[string]secrettypes.ExternalSecretRef{"CERT": {LegacyRef: "pki-role:pki:web:app.example"}},
			opts: ResolveOptions{DeferPKIRoleRefs: true, InterpolateResolved: true},
		},
		{
			name: "companion conflict",
			refs: map[string]secrettypes.ExternalSecretRef{
				"CERT":     {LegacyRef: "pki-role:pki:web:app.example"},
				"CERT_KEY": {LegacyRef: "kv:secret:app:key"},
			},
			opts: ResolveOptions{DeferPKIRoleRefs: true},
		},
		{
			name: "deferral off",
			refs: map[string]secrettypes.ExternalSecretRef{"CERT": {LegacyRef: "pki-role:pki:web:app.example"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mock := newBulkMock(nil)

			got, err := ResolveExternalSecrets(t.Context(), mock, tt.refs, nil, tt.opts, discardLogger())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if len(got.Deferred) != 0 {
				t.Errorf("nothing may be deferred, got %v", got.Deferred)
			}

			if got.Values["CERT"] != "resolved:pki-role:pki:web:app.example" {
				t.Errorf("CERT must be resolved by the provider, got %q", got.Values["CERT"])
			}
		})
	}
}
