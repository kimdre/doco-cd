// Package hook sends per-deployment HTTP webhook hooks on deployment success or failure.
package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// requestTimeout bounds a single hook request.
const requestTimeout = 10 * time.Second

var (
	ErrEmptyURL    = errors.New("hook url must not be empty")
	ErrInvalidURL  = errors.New("hook url must be a valid http(s) url")
	ErrHookFailed  = errors.New("hook request failed")
	httpHookClient = &http.Client{Timeout: requestTimeout}
)

// Webhook is a single HTTP hook target.
type Webhook struct {
	URL     string            `yaml:"url" json:"url"`         // URL is the endpoint to call
	Method  string            `yaml:"method" json:"method"`   // Method is the HTTP method (defaults to POST when empty)
	Headers map[string]string `yaml:"headers" json:"headers"` // Headers are additional request headers
}

// Config holds the hooks fired at the success and failure lifecycle points of a deployment.
type Config struct {
	OnSuccess []Webhook `yaml:"on_success" json:"on_success"` // OnSuccess hooks fire after a successful deployment
	OnFailure []Webhook `yaml:"on_failure" json:"on_failure"` // OnFailure hooks fire when a deployment fails
}

// Payload is the JSON body sent to a hook endpoint.
type Payload struct {
	Event      string   `json:"event"`            // Event is "success" or "failure"
	Repository string   `json:"repository"`       // Repository is the source repository/artifact name
	Stack      string   `json:"stack"`            // Stack is the deployment/stack name
	Revision   string   `json:"revision"`         // Revision is the deployed reference/commit
	JobID      string   `json:"job_id"`           // JobID is the unique job identifier
	Images     []string `json:"images,omitempty"` // Images are the resolved image references of the changed services
	Error      string   `json:"error,omitempty"`  // Error is the failure reason (failure event only)
}

// Validate checks that every configured hook has a valid http(s) URL.
func (c Config) Validate() error {
	for _, w := range append(append([]Webhook{}, c.OnSuccess...), c.OnFailure...) {
		if strings.TrimSpace(w.URL) == "" {
			return ErrEmptyURL
		}

		u, err := url.Parse(w.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("%w: %s", ErrInvalidURL, w.URL)
		}
	}

	return nil
}

// Send delivers the payload to a single hook target. Any non-2xx response is an error.
func Send(ctx context.Context, w Webhook, payload Payload) error {
	method := strings.TrimSpace(w.Method)
	if method == "" {
		method = http.MethodPost
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal hook payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, method, w.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create hook request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	for k, v := range w.Headers {
		req.Header.Set(k, v)
	}

	resp, err := httpHookClient.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrHookFailed, err)
	}

	defer func() {
		_ = resp.Body.Close()
	}()

	// Drain the body so the underlying transport can safely reuse the connection.
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("%w: status %s", ErrHookFailed, resp.Status)
	}

	return nil
}
