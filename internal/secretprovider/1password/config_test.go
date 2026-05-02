package onepassword

import (
	"errors"
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/config"
)

func TestGetConfig_DefaultCacheSettings(t *testing.T) {
	t.Setenv("SECRET_PROVIDER_ACCESS_TOKEN", "test-token") // #nosec G101
	t.Setenv("SECRET_PROVIDER_ACCESS_TOKEN_FILE", "")
	t.Setenv("SECRET_PROVIDER_CACHE_ENABLED", "")
	t.Setenv("SECRET_PROVIDER_CACHE_TTL", "")

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
}

func TestGetConfig_DisabledCacheAllowsZeroTTL(t *testing.T) {
	t.Setenv("SECRET_PROVIDER_ACCESS_TOKEN", "test-token") // #nosec G101
	t.Setenv("SECRET_PROVIDER_ACCESS_TOKEN_FILE", "")
	t.Setenv("SECRET_PROVIDER_CACHE_ENABLED", "false")
	t.Setenv("SECRET_PROVIDER_CACHE_TTL", "0s")

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
	t.Setenv("SECRET_PROVIDER_CACHE_ENABLED", "true")
	t.Setenv("SECRET_PROVIDER_CACHE_TTL", "0s")

	_, err := GetConfig()
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if !errors.Is(err, config.ErrParseConfigFailed) {
		t.Fatalf("expected ErrParseConfigFailed, got %v", err)
	}
}
