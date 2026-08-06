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
	"strconv"
	"strings"
	"time"

	"github.com/avast/retry-go/v5"
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

	bodyText, structuredBodyText := formatErrorResponseBody(body)
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

// formatErrorResponseBody formats the response body into a human-readable string.
// It returns two strings:
// the first is the raw body text, and the second is a structured representation of the error details if the body is in JSON format.
// If the body is empty or cannot be parsed, both strings will be empty.
func formatErrorResponseBody(body []byte) (string, string) {
	bodyText := strings.Join(strings.Fields(strings.TrimSpace(string(body))), " ")
	if bodyText == "" {
		return "", ""
	}

	parsedDetails := parseErrorResponseJSON(body)
	if parsedDetails == "" {
		return bodyText, ""
	}

	return bodyText, parsedDetails
}

// parseErrorResponseJSON attempts to parse the response body as JSON and extract relevant error details.
func parseErrorResponseJSON(body []byte) string {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err == nil {
		message := firstStringField(root, "message")
		typeName := firstStringField(root, "typeName")
		errorCode := firstField(root, "errorCode")

		if message == "" && typeName == "" && errorCode == nil {
			return ""
		}

		parts := make([]string, 0, 3)
		if message != "" {
			parts = append(parts, "message="+message)
		}

		if typeName != "" {
			parts = append(parts, "typeName="+typeName)
		}

		if errorCode != nil {
			parts = append(parts, "errorCode="+formatErrorCode(errorCode))
		}

		return strings.Join(parts, ", ")
	}

	var message string
	if err := json.Unmarshal(body, &message); err != nil {
		return ""
	}

	message = strings.Join(strings.Fields(strings.TrimSpace(message)), " ")
	if message == "" {
		return ""
	}

	return "message=" + message
}

// firstStringField retrieves the first occurrence of a string field from the root map or its nested "error" map.
func firstStringField(root map[string]any, field string) string {
	if value := firstField(root, field); value != nil {
		switch v := value.(type) {
		case string:
			return strings.Join(strings.Fields(strings.TrimSpace(v)), " ")
		default:
			return strings.Join(strings.Fields(strings.TrimSpace(fmt.Sprint(v))), " ")
		}
	}

	return ""
}

// firstField retrieves the first occurrence of a field from the root map or its nested "error" map.
func firstField(root map[string]any, field string) any {
	if root == nil {
		return nil
	}

	if value, ok := root[field]; ok {
		return value
	}

	errorData, ok := root["error"].(map[string]any)
	if !ok {
		return nil
	}

	if value, ok := errorData[field]; ok {
		return value
	}

	return nil
}

// formatErrorCode formats the error code into a string representation.
func formatErrorCode(code any) string {
	switch value := code.(type) {
	case string:
		return strings.TrimSpace(value)
	case float64:
		return strconv.FormatFloat(value, 'f', -1, 64)
	default:
		return strings.TrimSpace(fmt.Sprint(value))
	}
}
