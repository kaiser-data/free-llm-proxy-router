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

// ResponseCache caches non-streaming responses keyed by a hash of the request
// body. It holds at most maxBytes, so distinct requests within the TTL cannot
// grow it without limit on a small board.
type ResponseCache struct {
	mu       sync.RWMutex
	entries  map[string]*CacheEntry
	ttl      time.Duration
	bytes    int // sum of entrySize over entries
	maxBytes int
}

const (
	// cacheMaxBytes bounds the whole cache; cacheMaxEntryBytes skips caching
	// an answer so large it would crowd out many others.
	cacheMaxBytes      = 16 << 20
	cacheMaxEntryBytes = 1 << 20
	// cacheEntryOverhead approximates the key, map slot and CacheEntry, so
	// many tiny answers also count against the budget.
	cacheEntryOverhead = 160
)

func entrySize(e *CacheEntry) int { return len(e.Body) + cacheEntryOverhead }

// NewResponseCache creates a ResponseCache with the given TTL in seconds.
func NewResponseCache(ttlSeconds int) *ResponseCache {
	c := &ResponseCache{
		entries:  make(map[string]*CacheEntry),
		ttl:      time.Duration(ttlSeconds) * time.Second,
		maxBytes: cacheMaxBytes,
	}
	// Start background eviction
	go c.evictLoop()
	return c
}

// Get returns a cached response for the given request body, or nil if not cached.
func (c *ResponseCache) Get(reqBody map[string]any) []byte {
	if c.ttl <= 0 {
		return nil // cache off: skip serialising and hashing the request
	}
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
	if c.ttl <= 0 || len(respBody) > cacheMaxEntryBytes {
		return
	}
	key := cacheKey(reqBody)
	e := &CacheEntry{Body: respBody, ExpiresAt: time.Now().Add(c.ttl)}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.remove(key)
	if c.bytes+entrySize(e) > c.maxBytes {
		c.evictLocked(time.Now())
	}
	// Still full of live entries: drop arbitrary ones (map order) until the
	// new entry fits. A cache miss later costs one upstream call; going over
	// budget could cost the board its memory.
	for k := range c.entries {
		if c.bytes+entrySize(e) <= c.maxBytes {
			break
		}
		c.remove(k)
	}
	c.entries[key] = e
	c.bytes += entrySize(e)
}

// remove deletes key and its bytes from the budget. c.mu must be held.
func (c *ResponseCache) remove(key string) {
	if old, ok := c.entries[key]; ok {
		c.bytes -= entrySize(old)
		delete(c.entries, key)
	}
}

func (c *ResponseCache) evictLoop() {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		c.evict()
	}
}

func (c *ResponseCache) evict() {
	c.mu.Lock()
	c.evictLocked(time.Now())
	c.mu.Unlock()
}

// evictLocked drops expired entries. c.mu must be held.
func (c *ResponseCache) evictLocked(now time.Time) {
	for k, e := range c.entries {
		if now.After(e.ExpiresAt) {
			c.remove(k)
		}
	}
}

// cacheable reports whether a request asks for a repeatable answer. Tool
// requests are agent turns (repeating one after a failed tool call must reach
// a model again), and temperature > 0 or n > 1 ask for varied samples. An
// unset temperature stays cacheable, as before.
func cacheable(req map[string]any) bool {
	if hasTools(req) {
		return false
	}
	if f, ok := req["functions"].([]any); ok && len(f) > 0 { // legacy function calling
		return false
	}
	switch t := req["temperature"].(type) { // JSON gives float64, a YAML profile int
	case float64:
		if t > 0 {
			return false
		}
	case int:
		if t > 0 {
			return false
		}
	}
	if n, ok := toInt(req["n"]); ok && n > 1 {
		return false
	}
	return true
}

func cacheKey(req map[string]any) string {
	data, _ := json.Marshal(req)
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum)
}
