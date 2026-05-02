package onepassword

import (
	"testing"
	"time"
)

func TestProviderCache_GetAndExpire(t *testing.T) {
	provider := &Provider{
		cacheEnabled: true,
		cacheTTL:     15 * time.Millisecond,
		cache:        make(map[string]cacheEntry),
	}

	provider.setCachedSecret("op://vault/item/field", "cached-value")

	value, ok := provider.getCachedSecret("op://vault/item/field")
	if !ok {
		t.Fatal("expected cache hit")
	}

	if value != "cached-value" {
		t.Fatalf("expected cached value, got %q", value)
	}

	time.Sleep(20 * time.Millisecond)

	_, ok = provider.getCachedSecret("op://vault/item/field")
	if ok {
		t.Fatal("expected cache miss after ttl expiration")
	}
}

func TestProviderCache_Disabled(t *testing.T) {
	provider := &Provider{
		cacheEnabled: false,
		cacheTTL:     time.Minute,
		cache:        make(map[string]cacheEntry),
	}

	provider.setCachedSecret("op://vault/item/field", "cached-value")

	_, ok := provider.getCachedSecret("op://vault/item/field")
	if ok {
		t.Fatal("expected cache miss when cache is disabled")
	}
}
