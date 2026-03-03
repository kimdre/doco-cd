package secretprovider

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

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
		"permission error": {
			err:  errPermission,
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
