package secretprovider

import (
	"context"
	"errors"
	"maps"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/avast/retry-go/v5"
	openbao "github.com/openbao/openbao/api/v2"

	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
)

// retryableKeywords are phrases in provider error messages that mark a transient
// failure. Providers like Bitwarden return plain string errors, so message matching is all we have.
var retryableKeywords = []string{
	"too many requests",
	"rate limit",
	"rate-limit",
	"internal server error",
	"bad gateway",
	"service unavailable",
	"gateway timeout",
	"timeout",
	"connection reset",
}

// retryableStatusText matches 429/5xx written as a status, e.g. "[520 Unknown Error]" or
// "StatusCode: 503", but not digits that are part of a secret id like "secret500".
var retryableStatusText = regexp.MustCompile(`(?i)(?:\[|(?:status|code|http)\W{0,3})(429|5\d\d)\b`)

// isRetryableStatus reports whether an HTTP status is transient: 429 or 5xx.
func isRetryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code >= http.StatusInternalServerError
}

// isRetryable reports whether an upstream secret provider error is transient
// (rate limit, 5xx, network timeout) and worth another attempt.
func isRetryable(err error) bool {
	if err == nil || errors.Is(err, secrettypes.ErrNotRetryable) {
		return false
	}

	if respErr, ok := errors.AsType[*openbao.ResponseError](err); ok {
		return isRetryableStatus(respErr.StatusCode)
	}

	if _, ok := errors.AsType[*url.Error](err); ok {
		return true
	}

	if netErr, ok := errors.AsType[net.Error](err); ok && netErr.Timeout() {
		return true
	}

	msg := strings.ToLower(err.Error())

	return retryableStatusText.MatchString(msg) || slices.ContainsFunc(retryableKeywords, func(keyword string) bool {
		return strings.Contains(msg, keyword)
	})
}

// retryOpts are the shared retry options for secret provider operations that
// may fail due to transient upstream errors (rate limit, 5xx, network timeout).
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
// due to transient errors using exponential backoff with jitter.
type RetryingSecretProvider struct {
	inner SecretProvider
}

// NewRetryingSecretProvider wraps the given SecretProvider with retry logic for
// transient upstream errors.
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

// GetSecret retrieves a single secret, retrying on transient errors.
func (r *RetryingSecretProvider) GetSecret(ctx context.Context, id string) (string, error) {
	return retry.NewWithData[string](newOptsWithContext(ctx)...).Do(
		func() (string, error) {
			return r.inner.GetSecret(ctx, id)
		},
	)
}

// GetSecrets retrieves multiple secrets, retrying on transient errors.
func (r *RetryingSecretProvider) GetSecrets(ctx context.Context, ids []string) (map[string]string, error) {
	return retry.NewWithData[map[string]string](newOptsWithContext(ctx)...).Do(
		func() (map[string]string, error) {
			return r.inner.GetSecrets(ctx, ids)
		},
	)
}

// ResolveSecretReferences resolves secret references, retrying on transient errors.
func (r *RetryingSecretProvider) ResolveSecretReferences(ctx context.Context, secrets map[string]string) (secrettypes.ResolvedSecrets, error) {
	// Create a copy of the input map so that retries don't operate on
	// a partially-mutated map from a previous failed attempt.
	original := make(map[string]string, len(secrets))
	maps.Copy(original, secrets)

	return retry.NewWithData[secrettypes.ResolvedSecrets](newOptsWithContext(ctx)...).Do(
		func() (secrettypes.ResolvedSecrets, error) {
			// Work on a fresh copy each attempt
			attempt := make(map[string]string, len(original))
			maps.Copy(attempt, original)

			return r.inner.ResolveSecretReferences(ctx, attempt)
		},
	)
}

// DeploymentHasRevokedCertificate delegates to the wrapped provider when it supports revocation
// checks, while still applying the same retry policy used for other provider operations.
func (r *RetryingSecretProvider) DeploymentHasRevokedCertificate(ctx context.Context, certState string) (bool, error) {
	checker, ok := r.inner.(DeploymentCertificateRevocationChecker)
	if !ok {
		return false, nil
	}

	return retry.NewWithData[bool](newOptsWithContext(ctx)...).Do(
		func() (bool, error) {
			return checker.DeploymentHasRevokedCertificate(ctx, certState)
		},
	)
}
