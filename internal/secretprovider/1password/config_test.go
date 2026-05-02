package onepassword

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/config"
)

func TestGetConfig_DefaultCacheSettings(t *testing.T) {
	t.Setenv("SECRET_PROVIDER_ACCESS_TOKEN", "test-token") // #nosec G101
	t.Setenv("SECRET_PROVIDER_ACCESS_TOKEN_FILE", "")
	t.Setenv("OP_CONNECT_HOST", "")
	t.Setenv("SECRET_PROVIDER_CONNECT_TOKEN", "")
	t.Setenv("SECRET_PROVIDER_CONNECT_TOKEN_FILE", "")
	t.Setenv("SECRET_PROVIDER_CACHE_ENABLED", "")
	t.Setenv("SECRET_PROVIDER_CACHE_TTL", "")
	t.Setenv("SECRET_PROVIDER_CACHE_MAX_SIZE", "")

	cfg, err := GetConfig()
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if cfg.CacheEnabled {
		t.Fatalf("expected cache to be disabled by default")
	}

	if cfg.CacheTTL != 5*time.Minute {
		t.Fatalf("expected default cache ttl 5m, got %s", cfg.CacheTTL)
	}

	if cfg.CacheMaxSize != 100 {
		t.Fatalf("expected default cache max size 100, got %d", cfg.CacheMaxSize)
	}

	if cfg.UseConnect() {
		t.Fatal("expected service-account mode by default")
	}
}

func TestGetConfig_DisabledCacheAllowsZeroTTL(t *testing.T) {
	t.Setenv("SECRET_PROVIDER_ACCESS_TOKEN", "test-token") // #nosec G101
	t.Setenv("SECRET_PROVIDER_ACCESS_TOKEN_FILE", "")
	t.Setenv("OP_CONNECT_HOST", "")
	t.Setenv("SECRET_PROVIDER_CONNECT_TOKEN", "")
	t.Setenv("SECRET_PROVIDER_CONNECT_TOKEN_FILE", "")
	t.Setenv("SECRET_PROVIDER_CACHE_ENABLED", "false")
	t.Setenv("SECRET_PROVIDER_CACHE_TTL", "0s")
	t.Setenv("SECRET_PROVIDER_CACHE_MAX_SIZE", "100")

	cfg, err := GetConfig()
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if cfg.CacheEnabled {
		t.Fatalf("expected cache to be disabled")
	}

	if cfg.CacheTTL != 0 {
		t.Fatalf("expected cache ttl 0s, got %s", cfg.CacheTTL)
	}
}

func TestGetConfig_EnabledCacheRequiresPositiveTTL(t *testing.T) {
	t.Setenv("SECRET_PROVIDER_ACCESS_TOKEN", "test-token") // #nosec G101
	t.Setenv("SECRET_PROVIDER_ACCESS_TOKEN_FILE", "")
	t.Setenv("OP_CONNECT_HOST", "")
	t.Setenv("SECRET_PROVIDER_CONNECT_TOKEN", "")
	t.Setenv("SECRET_PROVIDER_CONNECT_TOKEN_FILE", "")
	t.Setenv("SECRET_PROVIDER_CACHE_ENABLED", "true")
	t.Setenv("SECRET_PROVIDER_CACHE_TTL", "0s")
	t.Setenv("SECRET_PROVIDER_CACHE_MAX_SIZE", "100")

	_, err := GetConfig()
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if !errors.Is(err, config.ErrParseConfigFailed) {
		t.Fatalf("expected ErrParseConfigFailed, got %v", err)
	}
}

func TestGetConfig_ConnectModeWithToken(t *testing.T) {
	t.Setenv("SECRET_PROVIDER_ACCESS_TOKEN", "")
	t.Setenv("SECRET_PROVIDER_ACCESS_TOKEN_FILE", "")
	t.Setenv("SECRET_PROVIDER_CONNECT_HOST", "http://op-connect-api:8080")
	t.Setenv("SECRET_PROVIDER_CONNECT_TOKEN", "connect-token") // #nosec G101
	t.Setenv("SECRET_PROVIDER_CONNECT_TOKEN_FILE", "")
	t.Setenv("SECRET_PROVIDER_CACHE_ENABLED", "true")
	t.Setenv("SECRET_PROVIDER_CACHE_TTL", "5m")
	t.Setenv("SECRET_PROVIDER_CACHE_MAX_SIZE", "100")

	cfg, err := GetConfig()
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if !cfg.UseConnect() {
		t.Fatal("expected connect mode")
	}

	if cfg.CacheEnabled {
		t.Fatal("expected cache to be disabled in connect mode")
	}
}

func TestGetConfig_ConnectModeWithTokenFile(t *testing.T) {
	tokenFile, err := os.CreateTemp(t.TempDir(), "op-connect-token")
	if err != nil {
		t.Fatalf("failed to create temp token file: %v", err)
	}

	if _, err = tokenFile.WriteString("connect-token"); err != nil {
		t.Fatalf("failed to write temp token file: %v", err)
	}

	if err = tokenFile.Close(); err != nil {
		t.Fatalf("failed to close temp token file: %v", err)
	}

	t.Setenv("SECRET_PROVIDER_ACCESS_TOKEN", "")
	t.Setenv("SECRET_PROVIDER_ACCESS_TOKEN_FILE", "")
	t.Setenv("SECRET_PROVIDER_CONNECT_HOST", "http://op-connect-api:8080")
	t.Setenv("SECRET_PROVIDER_CONNECT_TOKEN", "")
	t.Setenv("SECRET_PROVIDER_CONNECT_TOKEN_FILE", tokenFile.Name()) // #nosec G101
	t.Setenv("SECRET_PROVIDER_CACHE_ENABLED", "false")
	t.Setenv("SECRET_PROVIDER_CACHE_TTL", "5m")
	t.Setenv("SECRET_PROVIDER_CACHE_MAX_SIZE", "100")

	cfg, err := GetConfig()
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if !cfg.UseConnect() {
		t.Fatal("expected connect mode")
	}
}

func TestGetConfig_ConnectModeRequiresBothHostAndToken(t *testing.T) {
	t.Setenv("SECRET_PROVIDER_ACCESS_TOKEN", "")
	t.Setenv("SECRET_PROVIDER_ACCESS_TOKEN_FILE", "")
	t.Setenv("SECRET_PROVIDER_CONNECT_HOST", "http://op-connect-api:8080")
	t.Setenv("SECRET_PROVIDER_CONNECT_TOKEN", "")
	t.Setenv("SECRET_PROVIDER_CONNECT_TOKEN_FILE", "")
	t.Setenv("SECRET_PROVIDER_CACHE_ENABLED", "false")
	t.Setenv("SECRET_PROVIDER_CACHE_TTL", "5m")
	t.Setenv("SECRET_PROVIDER_CACHE_MAX_SIZE", "100")

	_, err := GetConfig()
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if !errors.Is(err, config.ErrParseConfigFailed) {
		t.Fatalf("expected ErrParseConfigFailed, got %v", err)
	}
}

func TestGetConfig_ConnectPreferredOverServiceAccount(t *testing.T) {
	t.Setenv("SECRET_PROVIDER_ACCESS_TOKEN", "service-token") // #nosec G101
	t.Setenv("SECRET_PROVIDER_ACCESS_TOKEN_FILE", "")
	t.Setenv("SECRET_PROVIDER_CONNECT_HOST", "http://op-connect-api:8080")
	t.Setenv("SECRET_PROVIDER_CONNECT_TOKEN", "connect-token") // #nosec G101
	t.Setenv("SECRET_PROVIDER_CONNECT_TOKEN_FILE", "")
	t.Setenv("SECRET_PROVIDER_CACHE_ENABLED", "true")
	t.Setenv("SECRET_PROVIDER_CACHE_TTL", "5m")
	t.Setenv("SECRET_PROVIDER_CACHE_MAX_SIZE", "100")

	cfg, err := GetConfig()
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if !cfg.UseConnect() {
		t.Fatal("expected connect mode to be preferred")
	}

	if cfg.CacheEnabled {
		t.Fatal("expected cache to be disabled in connect mode")
	}
}

func TestGetConfig_ServiceAccountRequiredWithoutConnect(t *testing.T) {
	t.Setenv("SECRET_PROVIDER_ACCESS_TOKEN", "")
	t.Setenv("SECRET_PROVIDER_ACCESS_TOKEN_FILE", "")
	t.Setenv("SECRET_PROVIDER_CONNECT_HOST", "")
	t.Setenv("SECRET_PROVIDER_CONNECT_TOKEN", "")
	t.Setenv("SECRET_PROVIDER_CONNECT_TOKEN_FILE", "")
	t.Setenv("SECRET_PROVIDER_CACHE_ENABLED", "false")
	t.Setenv("SECRET_PROVIDER_CACHE_TTL", "5m")
	t.Setenv("SECRET_PROVIDER_CACHE_MAX_SIZE", "100")

	_, err := GetConfig()
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if !errors.Is(err, config.ErrParseConfigFailed) {
		t.Fatalf("expected ErrParseConfigFailed, got %v", err)
	}
}

func TestGetConfig_CacheMaxSizeRequiresMinimumOfOne(t *testing.T) {
	t.Setenv("SECRET_PROVIDER_ACCESS_TOKEN", "test-token") // #nosec G101
	t.Setenv("SECRET_PROVIDER_ACCESS_TOKEN_FILE", "")
	t.Setenv("OP_CONNECT_HOST", "")
	t.Setenv("OP_CONNECT_TOKEN", "")
	t.Setenv("OP_CONNECT_TOKEN_FILE", "")
	t.Setenv("SECRET_PROVIDER_CACHE_ENABLED", "true")
	t.Setenv("SECRET_PROVIDER_CACHE_TTL", "5m")
	t.Setenv("SECRET_PROVIDER_CACHE_MAX_SIZE", "0")

	_, err := GetConfig()
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if !errors.Is(err, config.ErrParseConfigFailed) {
		t.Fatalf("expected ErrParseConfigFailed, got %v", err)
	}
}
