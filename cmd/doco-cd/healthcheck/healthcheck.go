package healthcheck

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const defaultHTTPPort = 80

// Target is the local health endpoint of the running server.
type Target struct {
	URL           string
	SkipTLSVerify bool
}

// TargetFromEnv resolves the health endpoint from HTTP_PORT, HTTP_TLS_CERT_FILE
// and HTTP_TLS_KEY_FILE only, so the healthcheck command does not need the full
// application configuration. TLS is used when both certificate and key are set,
// matching the server.
func TargetFromEnv(lookupEnv func(string) (string, bool), path string) (Target, error) {
	port := uint64(defaultHTTPPort)

	if value, ok := lookupEnv("HTTP_PORT"); ok && strings.TrimSpace(value) != "" {
		parsed, err := strconv.ParseUint(strings.TrimSpace(value), 10, 16)
		if err != nil || parsed == 0 {
			return Target{}, fmt.Errorf("invalid HTTP_PORT %q", value)
		}

		port = parsed
	}

	certFile, _ := lookupEnv("HTTP_TLS_CERT_FILE")
	keyFile, _ := lookupEnv("HTTP_TLS_KEY_FILE")
	hasCert := strings.TrimSpace(certFile) != ""
	hasKey := strings.TrimSpace(keyFile) != ""

	if hasCert != hasKey {
		return Target{}, errors.New("HTTP_TLS_CERT_FILE and HTTP_TLS_KEY_FILE must be set together")
	}

	scheme := "http"
	if hasCert {
		scheme = "https"
	}

	return Target{
		URL:           fmt.Sprintf("%s://localhost:%d%s", scheme, port, path),
		SkipTLSVerify: hasCert,
	}, nil
}

func Check(ctx context.Context, url string, skipTLSVerify bool) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	client := http.DefaultClient
	if skipTLSVerify {
		client = &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // #nosec G402 -- Container-local healthchecks may use self-signed certificates on localhost.
			},
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("health check request failed: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check failed with status: %s", resp.Status)
	}

	return nil
}
