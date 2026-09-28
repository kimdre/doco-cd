package secretprovider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	openbao "github.com/openbao/openbao/api/v2"

	openbaoprovider "github.com/kimdre/doco-cd/internal/secretprovider/openbao"
	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
)

// mockSecretProvider is a test double that records call counts and returns
// configurable results per invocation.
type mockSecretProvider struct {
	name string

	getSecretFunc          func(ctx context.Context, id string) (string, error)
	getSecretsFunc         func(ctx context.Context, ids []string) (map[string]string, error)
	resolveSecretRefsFunc  func(ctx context.Context, secrets map[string]string) (secrettypes.ResolvedSecrets, error)
	getSecretCalls         atomic.Int32
	getSecretsCalls        atomic.Int32
	resolveSecretRefsCalls atomic.Int32
	closeCalled            atomic.Int32
}

func (m *mockSecretProvider) Name() string { return m.name }
func (m *mockSecretProvider) Close()       { m.closeCalled.Add(1) }

func (m *mockSecretProvider) GetSecret(ctx context.Context, id string) (string, error) {
	m.getSecretCalls.Add(1)
	return m.getSecretFunc(ctx, id)
}

func (m *mockSecretProvider) GetSecrets(ctx context.Context, ids []string) (map[string]string, error) {
	m.getSecretsCalls.Add(1)
	return m.getSecretsFunc(ctx, ids)
}

func (m *mockSecretProvider) ResolveSecretReferences(ctx context.Context, secrets map[string]string) (secrettypes.ResolvedSecrets, error) {
	m.resolveSecretRefsCalls.Add(1)
	return m.resolveSecretRefsFunc(ctx, secrets)
}

var (
	errRateLimit  = errors.New("API error: Received error message from server: [429 Too Many Requests]")
	errPermission = errors.New("access denied: insufficient permissions")
)

func TestRetryingSecretProvider_GetSecret_SuccessFirstTry(t *testing.T) {
	t.Parallel()

	mock := &mockSecretProvider{
		name: "test",
		getSecretFunc: func(_ context.Context, id string) (string, error) {
			return "secret-value-" + id, nil
		},
	}

	subject := NewRetryingSecretProvider(mock)

	got, err := subject.GetSecret(t.Context(), "id-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got != "secret-value-id-1" {
		t.Errorf("got %q, want %q", got, "secret-value-id-1")
	}

	if calls := mock.getSecretCalls.Load(); calls != 1 {
		t.Errorf("expected 1 call, got %d", calls)
	}
}

func TestRetryingSecretProvider_GetSecret_RetriesOnRateLimit(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	mock := &mockSecretProvider{
		name: "test",
		getSecretFunc: func(_ context.Context, _ string) (string, error) {
			if calls.Add(1) <= 2 {
				return "", errRateLimit
			}

			return "success", nil
		},
	}

	subject := NewRetryingSecretProvider(mock)

	got, err := subject.GetSecret(t.Context(), "id-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got != "success" {
		t.Errorf("got %q, want %q", got, "success")
	}

	if totalCalls := mock.getSecretCalls.Load(); totalCalls != 3 {
		t.Errorf("expected 3 calls (2 retries + 1 success), got %d", totalCalls)
	}
}

func TestRetryingSecretProvider_GetSecret_NoRetryOnNonRetryableError(t *testing.T) {
	t.Parallel()

	mock := &mockSecretProvider{
		name: "test",
		getSecretFunc: func(_ context.Context, _ string) (string, error) {
			return "", errPermission
		},
	}

	subject := NewRetryingSecretProvider(mock)

	_, err := subject.GetSecret(t.Context(), "id-1")
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if calls := mock.getSecretCalls.Load(); calls != 1 {
		t.Errorf("expected 1 call (no retry for non-retryable error), got %d", calls)
	}
}

func TestRetryingSecretProvider_GetSecret_ExhaustsRetries(t *testing.T) {
	t.Parallel()

	mock := &mockSecretProvider{
		name: "test",
		getSecretFunc: func(_ context.Context, _ string) (string, error) {
			return "", errRateLimit
		},
	}

	subject := NewRetryingSecretProvider(mock)

	_, err := subject.GetSecret(t.Context(), "id-1")
	if err == nil {
		t.Fatal("expected error after exhausting retries, got nil")
	}

	// retrier is configured with 5 attempts
	if calls := mock.getSecretCalls.Load(); calls != 5 {
		t.Errorf("expected 5 calls (all attempts exhausted), got %d", calls)
	}
}

func TestRetryingSecretProvider_GetSecrets_RetriesOnRateLimit(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	mock := &mockSecretProvider{
		name: "test",
		getSecretsFunc: func(_ context.Context, ids []string) (map[string]string, error) {
			if calls.Add(1) <= 1 {
				return nil, errRateLimit
			}

			result := make(map[string]string, len(ids))
			for _, id := range ids {
				result[id] = "val-" + id
			}

			return result, nil
		},
	}

	subject := NewRetryingSecretProvider(mock)

	got, err := subject.GetSecrets(t.Context(), []string{"a", "b"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(got) != 2 {
		t.Errorf("expected 2 secrets, got %d", len(got))
	}

	if totalCalls := mock.getSecretsCalls.Load(); totalCalls != 2 {
		t.Errorf("expected 2 calls (1 retry + 1 success), got %d", totalCalls)
	}
}

func TestRetryingSecretProvider_OpenBaoPKIIssuanceIsNotRetried(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		call  func(context.Context, *RetryingSecretProvider) error
		calls func(*mockSecretProvider) int32
	}{
		{
			name: "GetSecret",
			call: func(ctx context.Context, provider *RetryingSecretProvider) error {
				_, err := provider.GetSecret(ctx, "pki-role:pki:role:issued.example.com")
				return err
			},
			calls: func(provider *mockSecretProvider) int32 {
				return provider.getSecretCalls.Load()
			},
		},
		{
			name: "GetSecrets",
			call: func(ctx context.Context, provider *RetryingSecretProvider) error {
				_, err := provider.GetSecrets(ctx, []string{"pki-role:pki:role:issued.example.com"})
				return err
			},
			calls: func(provider *mockSecretProvider) int32 {
				return provider.getSecretsCalls.Load()
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var requests atomic.Int32

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				http.Error(w, `{"errors":["service unavailable"]}`, http.StatusServiceUnavailable)
			}))
			t.Cleanup(server.Close)

			openBaoProvider, err := openbaoprovider.NewProvider(t.Context(), server.URL, "token")
			if err != nil {
				t.Fatalf("NewProvider() error = %v", err)
			}

			provider := &mockSecretProvider{
				name:           openBaoProvider.Name(),
				getSecretFunc:  openBaoProvider.GetSecret,
				getSecretsFunc: openBaoProvider.GetSecrets,
			}

			err = tt.call(t.Context(), NewRetryingSecretProvider(provider))
			if !errors.Is(err, secrettypes.ErrNotRetryable) {
				t.Fatalf("%s() error = %v, want ErrNotRetryable", tt.name, err)
			}

			if got := tt.calls(provider); got != 1 {
				t.Errorf("%s() called the provider %d times, want 1", tt.name, got)
			}

			if got := requests.Load(); got != 1 {
				t.Errorf("%s() sent %d issuance requests, want 1", tt.name, got)
			}
		})
	}
}

func TestRetryingSecretProvider_OpenBaoPKIBatchIsNotRetriedAfterPartialIssuance(t *testing.T) {
	t.Parallel()

	var issuanceRequests atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/pki/issue/role" {
			issuanceRequests.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"data":{"certificate":"cert","private_key":"key","expiration":4102444800}}`)

			return
		}

		http.Error(w, `{"errors":["service unavailable"]}`, http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)

	openBaoProvider, err := openbaoprovider.NewProvider(t.Context(), server.URL, "token")
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	provider := &mockSecretProvider{
		name:           openBaoProvider.Name(),
		getSecretsFunc: openBaoProvider.GetSecrets,
	}

	_, err = NewRetryingSecretProvider(provider).GetSecrets(t.Context(), []string{
		"pki-role:pki:role:issued.example.com",
		"kv:kv:secret:key",
	})
	if !errors.Is(err, secrettypes.ErrNotRetryable) {
		t.Fatalf("GetSecrets() error = %v, want ErrNotRetryable", err)
	}

	if got := provider.getSecretsCalls.Load(); got != 1 {
		t.Errorf("GetSecrets() called the provider %d times, want 1", got)
	}

	if got := issuanceRequests.Load(); got != 1 {
		t.Errorf("GetSecrets() issued %d certificates, want 1", got)
	}
}

func TestRetryingSecretProvider_ResolveSecretReferences_RetriesOnRateLimit(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	mock := &mockSecretProvider{
		name: "test",
		resolveSecretRefsFunc: func(_ context.Context, secrets map[string]string) (secrettypes.ResolvedSecrets, error) {
			if calls.Add(1) <= 2 {
				return nil, errRateLimit
			}

			resolved := make(secrettypes.ResolvedSecrets, len(secrets))
			for k := range secrets {
				resolved[k] = "resolved-" + k
			}

			return resolved, nil
		},
	}

	subject := NewRetryingSecretProvider(mock)

	input := map[string]string{"ENV_A": "secret-id-a", "ENV_B": "secret-id-b"}

	got, err := subject.ResolveSecretReferences(t.Context(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(got) != 2 {
		t.Errorf("expected 2 resolved secrets, got %d", len(got))
	}

	if totalCalls := mock.resolveSecretRefsCalls.Load(); totalCalls != 3 {
		t.Errorf("expected 3 calls (2 retries + 1 success), got %d", totalCalls)
	}
}

func TestRetryingSecretProvider_ResolveSecretReferences_NoRetryOnNotRetryable(t *testing.T) {
	t.Parallel()

	mock := &mockSecretProvider{
		name: "test",
		resolveSecretRefsFunc: func(_ context.Context, _ map[string]string) (secrettypes.ResolvedSecrets, error) {
			return nil, fmt.Errorf("%w: failed to issue certificate: %w", secrettypes.ErrNotRetryable, errRateLimit)
		},
	}

	subject := NewRetryingSecretProvider(mock)

	_, err := subject.ResolveSecretReferences(t.Context(), map[string]string{"CERT": "pki-role:pki:role:cn"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if calls := mock.resolveSecretRefsCalls.Load(); calls != 1 {
		t.Errorf("expected 1 call (no replay of non-idempotent issuance), got %d", calls)
	}
}

func TestRetryingSecretProvider_ResolveSecretReferences_PreservesInputOnRetry(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	mock := &mockSecretProvider{
		name: "test",
		resolveSecretRefsFunc: func(_ context.Context, secrets map[string]string) (secrettypes.ResolvedSecrets, error) {
			call := calls.Add(1)

			// First call: mutate the map (simulating partial resolution) then fail
			if call == 1 {
				secrets["ENV_A"] = "MUTATED"

				return nil, errRateLimit
			}

			// Second call: verify the input was NOT mutated from the first attempt
			if v, ok := secrets["ENV_A"]; ok && v == "MUTATED" {
				t.Error("input map was mutated from previous retry attempt")
			}

			resolved := make(secrettypes.ResolvedSecrets, len(secrets))
			for k, v := range secrets {
				resolved[k] = "resolved-" + v
			}

			return resolved, nil
		},
	}

	subject := NewRetryingSecretProvider(mock)

	input := map[string]string{"ENV_A": "secret-id-a"}

	_, err := subject.ResolveSecretReferences(t.Context(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRetryingSecretProvider_Name(t *testing.T) {
	t.Parallel()

	mock := &mockSecretProvider{name: "bitwarden_sm"}
	subject := NewRetryingSecretProvider(mock)

	if got := subject.Name(); got != "bitwarden_sm" {
		t.Errorf("got %q, want %q", got, "bitwarden_sm")
	}
}

func TestRetryingSecretProvider_Close(t *testing.T) {
	t.Parallel()

	mock := &mockSecretProvider{name: "test"}
	subject := NewRetryingSecretProvider(mock)

	subject.Close()

	if calls := mock.closeCalled.Load(); calls != 1 {
		t.Errorf("expected Close to be called once, got %d", calls)
	}
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestIsRetryable(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		err  error
		want bool
	}{
		"nil error": {
			err:  nil,
			want: false,
		},
		"429 status": {
			err:  errors.New("[429 Too Many Requests]"),
			want: true,
		},
		"too many requests lowercase": {
			err:  errors.New("too many requests"),
			want: true,
		},
		"rate limit": {
			err:  errors.New("rate limit exceeded"),
			want: true,
		},
		"bitwarden real error": {
			err:  errors.New(`API error: Received error message from server: [429 Too Many Requests] {"message":"Slow down! Too many requests. Try again in 1s."}`),
			want: true,
		},
		"503 from bitwarden": {
			err:  errors.New("API error: Received error message from server: [503 Service Unavailable]"),
			want: true,
		},
		"500 from aws": {
			err:  errors.New("operation error Secrets Manager: GetSecretValue, https response error StatusCode: 500, InternalServiceError"),
			want: true,
		},
		"bad gateway": {
			err:  errors.New("502 Bad Gateway"),
			want: true,
		},
		"tls handshake timeout string": {
			err:  errors.New("Post \"https://api.bitwarden.com/identity/connect/token\": net/http: TLS handshake timeout"),
			want: true,
		},
		"url error": {
			err:  &url.Error{Op: "Get", URL: "https://vault.example.com", Err: errors.New("connection refused")},
			want: true,
		},
		"net timeout": {
			err:  fmt.Errorf("resolve: %w", &net.OpError{Op: "dial", Err: timeoutError{}}),
			want: true,
		},
		"connection reset": {
			err:  errors.New("read tcp 10.0.0.1:443: connection reset by peer"),
			want: true,
		},
		"permission error": {
			err:  errPermission,
			want: false,
		},
		"not found": {
			err:  errors.New("secret not found: 404"),
			want: false,
		},
		"520 from bitwarden": {
			err:  errors.New("API error: Received error message from server: [520 Unknown Error]"),
			want: true,
		},
		"digits inside secret id": {
			err:  errors.New("secret not found: secret500"),
			want: false,
		},
		"openbao 404 mentioning secret500": {
			err:  fmt.Errorf("failed to retrieve secret with ID secret500: %w", &openbao.ResponseError{StatusCode: http.StatusNotFound, URL: "/v1/kv/data/secret500"}),
			want: false,
		},
		"openbao 503": {
			err:  fmt.Errorf("resolve: %w", &openbao.ResponseError{StatusCode: http.StatusServiceUnavailable}),
			want: true,
		},
		"openbao 429": {
			err:  &openbao.ResponseError{StatusCode: http.StatusTooManyRequests},
			want: true,
		},
		"not retryable marker": {
			err:  fmt.Errorf("%w: failed to issue certificate: %w", secrettypes.ErrNotRetryable, errRateLimit),
			want: false,
		},
		"generic error": {
			err:  errors.New("something went wrong"),
			want: false,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := isRetryable(tc.err); got != tc.want {
				t.Errorf("isRetryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
