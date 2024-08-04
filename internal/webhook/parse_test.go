package webhook

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var secretKey = "test_secret"

const webhookPath = "/v1/webhook"

func TestParse(t *testing.T) {
	t.Run("invalid http method", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, webhookPath, nil)
		_, err := Parse(r, secretKey)
		if !errors.Is(err, ErrInvalidHTTPMethod) {
			t.Errorf("expected ErrInvalidHTTPMethod, got %v", err)
		}
	})

	t.Run("failed to parse payload", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, webhookPath, nil)
		_, err := Parse(r, secretKey)
		if err == nil {
			t.Error("expected an error, got nil")
		}

		if !errors.Is(err, ErrParsingPayload) {
			t.Errorf("expected ErrParsingPayload, got %v", err)
		}
	})

	t.Run("missing security header", func(t *testing.T) {
		r := &http.Request{Method: http.MethodPost}
		r.Body = io.NopCloser(strings.NewReader("{}"))
		_, err := Parse(r, secretKey)
		if !errors.Is(err, ErrMissingSecurityHeader) {
			t.Errorf("expected ErrMissingSecurityHeader, got %v", err)
		}
	})

	t.Run("HMAC verification failed", func(t *testing.T) {
		r := &http.Request{Method: http.MethodPost}
		r.Body = io.NopCloser(strings.NewReader("{}"))
		r.Header = http.Header{"X-Hub-Signature-256": []string{"sha256=invalid"}}
		_, err := Parse(r, secretKey)
		if !errors.Is(err, ErrHMACVerificationFailed) {
			t.Errorf("expected ErrHMACVerificationFailed, got %v", err)
		}
	})

	t.Run("gitlab token verification failed", func(t *testing.T) {
		r := &http.Request{Method: http.MethodPost}
		r.Body = io.NopCloser(strings.NewReader("{}"))
		r.Header = http.Header{"X-Gitlab-Token": []string{"invalid"}}
		_, err := Parse(r, secretKey)
		if !errors.Is(err, ErrGitlabTokenVerificationFailed) {
			t.Errorf("expected ErrGitlabTokenVerificationFailed, got %v", err)
		}
	})

	t.Run("github provider", func(t *testing.T) {
		r := &http.Request{Method: http.MethodPost}
		r.Body = io.NopCloser(strings.NewReader("{}"))
		r.Header = http.Header{"X-Hub-Signature-256": []string{"sha256=invalid"}}
		_, err := Parse(r, secretKey)
		if !errors.Is(err, ErrHMACVerificationFailed) {
			t.Errorf("expected ErrHMACVerificationFailed, got %v", err)
		}
	})

	t.Run("gitea provider", func(t *testing.T) {
		r := &http.Request{Method: http.MethodPost}
		r.Body = io.NopCloser(strings.NewReader("{}"))
		r.Header = http.Header{"X-Gitea-Signature": []string{"invalid"}}
		_, err := Parse(r, secretKey)
		if !errors.Is(err, ErrHMACVerificationFailed) {
			t.Errorf("expected ErrHMACVerificationFailed, got %v", err)
		}
	})

	t.Run("gitlab provider", func(t *testing.T) {
		r := &http.Request{Method: http.MethodPost}
		r.Body = io.NopCloser(strings.NewReader("{}"))
		r.Header = http.Header{"X-Gitlab-Token": []string{"test_secret"}}
		_, err := Parse(r, secretKey)
		if err != nil {
			t.Errorf("expected nil, got %v", err)
		}
	})

	t.Run("successful parse", func(t *testing.T) {
		r := &http.Request{Method: http.MethodPost}
		r.Body = io.NopCloser(strings.NewReader("{}"))
		r.Header = http.Header{"X-Hub-Signature-256": []string{"sha256=invalid"}}
		_, err := Parse(r, secretKey)
		if !errors.Is(err, ErrHMACVerificationFailed) {
			t.Errorf("expected ErrHMACVerificationFailed, got %v", err)
		}
	})
}
