package commitstatus

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

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

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewBuffer(jsonData)) // #nosec G107
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", authHeaderValue)

	client := &http.Client{Timeout: 15 * time.Second}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to post commit status: %w", err)
	}

	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("commit status API returned %s for %s", resp.Status, apiURL)
	}

	return nil
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
		return fmt.Errorf("commit status API returned %s for %s", resp.Status, apiURL)
	}

	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		return fmt.Errorf("failed to decode response: %w", err)
	}

	return nil
}
