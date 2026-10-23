package secretprovider

import (
	"context"
	"errors"
	"io"
	"log/slog"
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
