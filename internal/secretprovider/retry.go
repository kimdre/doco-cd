package secretprovider

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/avast/retry-go/v5"

	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
)

// retryableKeywords contains substrings that indicate a retryable (rate-limited) error.
var retryableKeywords = []string{
	"429",
	"too many requests",
	"rate limit",
	"rate-limit",
}

// isRetryable returns true if the error message indicates a rate-limit or
// throttling response from the upstream secret provider API.
func isRetryable(err error) bool {
	if err == nil {
		return false
	}

	msg := strings.ToLower(err.Error())

	return slices.ContainsFunc(retryableKeywords, func(keyword string) bool {
		return strings.Contains(msg, keyword)
	})
}

// retryOpts are the shared retry options for secret provider operations that
// may fail due to rate limiting (HTTP 429) from the upstream API.
var retryOpts = []retry.Option{
	retry.Attempts(5),
	retry.Delay(1 * time.Second),
	retry.DelayType(retry.CombineDelay(retry.BackOffDelay, retry.RandomDelay)),
	retry.MaxJitter(500 * time.Millisecond),
	retry.RetryIf(isRetryable),
	retry.LastErrorOnly(true),
}

// newOptsWithContext returns a copy of the shared retry options with the provided context included.
// This allows retries to be canceled if the context is canceled, while still sharing the same retry configuration.
func newOptsWithContext(ctx context.Context) []retry.Option {
	return append(retryOpts, retry.Context(ctx))
}

// RetryingSecretProvider wraps a SecretProvider and retries operations that fail
// due to rate-limiting errors using exponential backoff with jitter.
type RetryingSecretProvider struct {
	inner SecretProvider
}

// NewRetryingSecretProvider wraps the given SecretProvider with retry logic for
// rate-limited API calls.
func NewRetryingSecretProvider(inner SecretProvider) *RetryingSecretProvider {
	return &RetryingSecretProvider{inner: inner}
}

// Name delegates to the wrapped provider.
func (r *RetryingSecretProvider) Name() string {
	return r.inner.Name()
}

// Close delegates to the wrapped provider.
func (r *RetryingSecretProvider) Close() {
	r.inner.Close()
}

// GetSecret retrieves a single secret, retrying on rate-limit errors.
func (r *RetryingSecretProvider) GetSecret(ctx context.Context, id string) (string, error) {
	return retry.NewWithData[string](newOptsWithContext(ctx)...).Do(
		func() (string, error) {
			return r.inner.GetSecret(ctx, id)
		},
	)
}

// GetSecrets retrieves multiple secrets, retrying on rate-limit errors.
func (r *RetryingSecretProvider) GetSecrets(ctx context.Context, ids []string) (map[string]string, error) {
	return retry.NewWithData[map[string]string](newOptsWithContext(ctx)...).Do(
		func() (map[string]string, error) {
			return r.inner.GetSecrets(ctx, ids)
		},
	)
}

// ResolveSecretReferences resolves secret references, retrying on rate-limit errors.
func (r *RetryingSecretProvider) ResolveSecretReferences(ctx context.Context, secrets map[string]string) (secrettypes.ResolvedSecrets, error) {
	// Create a copy of the input map so that retries don't operate on
	// a partially-mutated map from a previous failed attempt.
	original := make(map[string]string, len(secrets))
	for k, v := range secrets {
		original[k] = v
	}

	return retry.NewWithData[secrettypes.ResolvedSecrets](newOptsWithContext(ctx)...).Do(
		func() (secrettypes.ResolvedSecrets, error) {
			// Work on a fresh copy each attempt
			attempt := make(map[string]string, len(original))
			for k, v := range original {
				attempt[k] = v
			}

			return r.inner.ResolveSecretReferences(ctx, attempt)
		},
	)
}
