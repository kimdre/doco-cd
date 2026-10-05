package healthcheck

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kimdre/doco-cd/internal/logger"
)

const defaultHTTPPort = 80

// Path is the health endpoint of the doco-cd server.
const Path = "/v1/health"

// Run checks the health endpoint of the server running in the same container.
// It only reads HTTP_PORT, HTTP_TLS_CERT_FILE, HTTP_TLS_KEY_FILE and LOG_LEVEL,
// so it stays cheap and works even when unrelated settings (e.g. secret files)
// cannot be loaded.
func Run(ctx context.Context, lookupEnv func(string) (string, bool)) error {
	logLevel := slog.LevelInfo

	if value, ok := lookupEnv("LOG_LEVEL"); ok {
		if level, err := logger.ParseLevel(value); err == nil {
			logLevel = level
		}
	}

	log := logger.New(logLevel)

	target, err := TargetFromEnv(lookupEnv, Path)
	if err != nil {
		log.Log(ctx, logger.LevelCritical, "health check failed", logger.ErrAttr(err))
		return err
	}

	if err = Check(ctx, target.URL, target.SkipTLSVerify); err != nil {
		log.Log(ctx, logger.LevelCritical, "health check failed", logger.ErrAttr(err), slog.String("url", target.URL))
		return err
	}

	log.InfoContext(ctx, "health check successful", slog.String("url", target.URL))

	return nil
}

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
