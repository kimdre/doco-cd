package onepassword

import (
	"container/list"
	"sync"
	"time"
)

type cacheEntry struct {
	value     string
	expiresAt time.Time
}

type Cache struct {
	enabled bool
	ttl     time.Duration
	maxSize int
	mu      sync.RWMutex
	entries map[string]cacheEntry
	order   *list.List
	nodes   map[string]*list.Element
}

// NewCache creates a new cache instance with the given configuration.
func NewCache(enabled bool, ttl time.Duration, maxSize int) *Cache {
	return &Cache{
		enabled: enabled,
		ttl:     ttl,
		maxSize: maxSize,
		entries: make(map[string]cacheEntry),
		order:   list.New(),
		nodes:   make(map[string]*list.Element),
	}
}

// Get retrieves a cached value by key if it exists and hasn't expired.
// Returns the value and true if found and valid, empty string and false otherwise.
func (c *Cache) Get(key string) (string, bool) {
	if !c.enabled {
		return "", false
	}

	now := time.Now()

	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[key]

	if !ok {
		return "", false
	}

	if now.After(entry.expiresAt) {
		if current, exists := c.entries[key]; exists && now.After(current.expiresAt) {
			c.deleteEntry(key)
		}

		return "", false
	}

	c.touchEntry(key)

	return entry.value, true
}

// Set stores a value in the cache with an expiration time based on the configured TTL.
func (c *Cache) Set(key, value string) {
	if !c.enabled {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.entries == nil {
		c.entries = make(map[string]cacheEntry)
	}

	if c.order == nil {
		c.order = list.New()
	}

	if c.nodes == nil {
		c.nodes = make(map[string]*list.Element)
	}

	if _, exists := c.entries[key]; !exists && c.maxSize > 0 && len(c.entries) >= c.maxSize {
		c.evictLeastRecentlyUsed()
	}

	c.entries[key] = cacheEntry{value: value, expiresAt: time.Now().Add(c.ttl)}
	c.touchEntry(key)
}

// touchEntry moves the given key to the front of the LRU order list.
// Must be called while holding the lock.
func (c *Cache) touchEntry(key string) {
	if c.order == nil {
		c.order = list.New()
	}

	if c.nodes == nil {
		c.nodes = make(map[string]*list.Element)
	}

	if node, ok := c.nodes[key]; ok {
		c.order.MoveToFront(node)
		return
	}

	c.nodes[key] = c.order.PushFront(key)
}

// deleteEntry removes a key from the cache.
// Must be called while holding the lock.
func (c *Cache) deleteEntry(key string) {
	delete(c.entries, key)

	if node, ok := c.nodes[key]; ok {
		c.order.Remove(node)
		delete(c.nodes, key)
	}
}

// evictLeastRecentlyUsed removes the least recently used item from the cache.
// Must be called while holding the lock.
func (c *Cache) evictLeastRecentlyUsed() {
	if c.order == nil {
		return
	}

	leastRecent := c.order.Back()
	if leastRecent == nil {
		return
	}

	key, ok := leastRecent.Value.(string)
	if !ok {
		c.order.Remove(leastRecent)
		return
	}

	c.deleteEntry(key)
}
