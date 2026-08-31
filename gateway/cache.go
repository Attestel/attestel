package main

import (
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ttlCache is a tiny concurrency-safe in-memory cache. Upstreams (Tiingo/Alpha Vantage/Marketaux)
// have tight free-tier rate limits, so caching per-ticker responses is essential, not optional.
type ttlCache struct {
	mu    sync.RWMutex
	items map[string]cacheItem
}

type cacheItem struct {
	value   []byte
	expires time.Time
}

func newCache() *ttlCache { return &ttlCache{items: map[string]cacheItem{}} }

func (c *ttlCache) get(key string) ([]byte, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	it, ok := c.items[key]
	if !ok || time.Now().After(it.expires) {
		return nil, false
	}
	return it.value, true
}

func (c *ttlCache) set(key string, val []byte, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[key] = cacheItem{value: val, expires: time.Now().Add(ttl)}
}

func (c *ttlCache) deletePrefix(prefixes ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key := range c.items {
		for _, prefix := range prefixes {
			if strings.HasPrefix(key, prefix) {
				delete(c.items, key)
				break
			}
		}
	}
}

// --- middleware --- (withCORS lives in auth.go — it needs the config's Origin allow-list)

func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}
