package proxy

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// CacheEntry holds a cached response with its expiry time.
type CacheEntry struct {
	Body      []byte
	ExpiresAt time.Time
}

// ResponseCache caches non-streaming responses keyed by a hash of the request body.
type ResponseCache struct {
	mu      sync.RWMutex
	entries map[string]*CacheEntry
	ttl     time.Duration
}

// NewResponseCache creates a ResponseCache with the given TTL in seconds.
func NewResponseCache(ttlSeconds int) *ResponseCache {
	c := &ResponseCache{
		entries: make(map[string]*CacheEntry),
		ttl:     time.Duration(ttlSeconds) * time.Second,
	}
	// Start background eviction
	go c.evictLoop()
	return c
}

// Get returns a cached response for the given request body, or nil if not cached.
func (c *ResponseCache) Get(reqBody map[string]any) []byte {
	key := cacheKey(reqBody)
	c.mu.RLock()
	e, ok := c.entries[key]
	c.mu.RUnlock()
	if !ok || time.Now().After(e.ExpiresAt) {
		return nil
	}
	return e.Body
}

// Set stores a response in the cache.
func (c *ResponseCache) Set(reqBody map[string]any, respBody []byte) {
	if c.ttl <= 0 {
		return
	}
	key := cacheKey(reqBody)
	c.mu.Lock()
	c.entries[key] = &CacheEntry{
		Body:      respBody,
		ExpiresAt: time.Now().Add(c.ttl),
	}
	c.mu.Unlock()
}

func (c *ResponseCache) evictLoop() {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		c.evict()
	}
}

func (c *ResponseCache) evict() {
	now := time.Now()
	c.mu.Lock()
	for k, e := range c.entries {
		if now.After(e.ExpiresAt) {
			delete(c.entries, k)
		}
	}
	c.mu.Unlock()
}

func cacheKey(req map[string]any) string {
	data, _ := json.Marshal(req)
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum)
}
