package onepassword

import (
	"testing"
	"time"
)

func TestProviderCache_GetAndExpire(t *testing.T) {
	provider := &Provider{
		cache: NewCache(true, 15*time.Millisecond, 100),
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
		cache: NewCache(false, time.Minute, 100),
	}

	provider.setCachedSecret("op://vault/item/field", "cached-value")

	_, ok := provider.getCachedSecret("op://vault/item/field")
	if ok {
		t.Fatal("expected cache miss when cache is disabled")
	}
}

func TestProviderCache_EvictsLeastRecentlyUsedWhenFull(t *testing.T) {
	provider := &Provider{
		cache: NewCache(true, time.Minute, 2),
	}

	provider.setCachedSecret("op://vault/item/a", "A")
	provider.setCachedSecret("op://vault/item/b", "B")

	if _, ok := provider.getCachedSecret("op://vault/item/a"); !ok {
		t.Fatal("expected cache hit for A")
	}

	provider.setCachedSecret("op://vault/item/c", "C")

	if _, ok := provider.getCachedSecret("op://vault/item/b"); ok {
		t.Fatal("expected B to be evicted as least recently used")
	}

	if value, ok := provider.getCachedSecret("op://vault/item/a"); !ok || value != "A" {
		t.Fatalf("expected A to remain in cache, got value=%q hit=%v", value, ok)
	}

	if value, ok := provider.getCachedSecret("op://vault/item/c"); !ok || value != "C" {
		t.Fatalf("expected C to remain in cache, got value=%q hit=%v", value, ok)
	}
}
