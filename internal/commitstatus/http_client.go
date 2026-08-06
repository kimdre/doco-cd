package commitstatus

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/avast/retry-go/v5"

	"github.com/kimdre/doco-cd/internal/common/errdecode"
)

const (
	maxErrorResponseBodyBytes = 4 * 1024
	postRetryMaxAttempts      = 4
	postRetryInitialBackoff   = 250 * time.Millisecond
)

type commitStatusPostRetryError struct {
	err       error
	retryable bool
}

func (e *commitStatusPostRetryError) Error() string {
	return e.err.Error()
}

func (e *commitStatusPostRetryError) Unwrap() error {
	return e.err
}

func bearerAuthToken(token string) string {
	return "Bearer " + token
}

func giteaAuthToken(token string) string {
	return "token " + token
}

func azureDevOpsAuthToken(token string) string {
	pat := ":" + strings.TrimSpace(token)

	return "Basic " + base64.StdEncoding.EncodeToString([]byte(pat))
}

func doPost(ctx context.Context, apiURL, authHeaderValue string, body any) error {
	jsonData, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	client := &http.Client{Timeout: 15 * time.Second}

	return retry.New(
		retry.Attempts(postRetryMaxAttempts),
		retry.Delay(postRetryInitialBackoff),
		retry.DelayType(retry.BackOffDelay),
		retry.Context(ctx),
		retry.LastErrorOnly(true),
		retry.RetryIf(func(err error) bool {
			var postErr *commitStatusPostRetryError
			if !errors.As(err, &postErr) {
				return false
			}

			return postErr.retryable
		}),
	).Do(func() error {
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(jsonData)) // #nosec G107
		if reqErr != nil {
			return fmt.Errorf("failed to create request: %w", reqErr)
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", authHeaderValue)

		resp, postErr := client.Do(req)
		if postErr != nil {
			return &commitStatusPostRetryError{
				err:       fmt.Errorf("failed to post commit status: %w", postErr),
				retryable: true,
			}
		}

		defer func() {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}()

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return &commitStatusPostRetryError{
				err:       fmt.Errorf("commit status API returned %s for %s%s", resp.Status, apiURL, responseErrorDetails(resp)),
				retryable: isRetryablePostStatusCode(resp.StatusCode),
			}
		}

		return nil
	})
}

func isRetryablePostStatusCode(statusCode int) bool {
	return statusCode == http.StatusRequestTimeout ||
		statusCode == http.StatusTooManyRequests ||
		statusCode >= http.StatusInternalServerError
}

func doGet(ctx context.Context, apiURL, authHeaderValue string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil) // #nosec G107
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", authHeaderValue)

	client := &http.Client{Timeout: 15 * time.Second}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to get commit status: %w", err)
	}

	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("commit status API returned %s for %s%s", resp.Status, apiURL, responseErrorDetails(resp))
	}

	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		return fmt.Errorf("failed to decode response: %w", err)
	}

	return nil
}

// responseErrorDetails reads the response body and returns a string containing the error details, if any.
// It limits the amount of data read to avoid excessive memory usage with maxErrorResponseBodyBytes.
// If the response body is larger than the limit, it indicates that the content has been truncated.
// If the response body is empty or cannot be read, it returns an empty string.
func responseErrorDetails(resp *http.Response) string {
	if resp.Body == nil {
		return ""
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorResponseBodyBytes+1))
	if err != nil {
		return fmt.Sprintf(" (failed to read response body: %v)", err)
	}

	bodyText, structuredBodyText := errdecode.FormatErrorResponseBody(body)
	if bodyText == "" && structuredBodyText == "" {
		return ""
	}

	if len(body) > maxErrorResponseBodyBytes {
		if structuredBodyText != "" {
			return fmt.Sprintf(": %s (truncated)", structuredBodyText)
		}

		return fmt.Sprintf(": %s (truncated)", bodyText)
	}

	if structuredBodyText != "" {
		return ": " + structuredBodyText
	}

	return ": " + bodyText
}
