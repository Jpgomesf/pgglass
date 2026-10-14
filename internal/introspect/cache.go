package introspect

import (
	"sync"
	"time"
)

// DefaultCacheTTL is how long NewCache keeps an entry when given no TTL.
const DefaultCacheTTL = 5 * time.Minute

// Cache keeps rendered introspection output for a TTL, so repeated schema
// calls do not hit the catalogs. It is safe for concurrent use.
type Cache struct {
	ttl time.Duration
	now func() time.Time

	mu      sync.Mutex
	entries map[string]cacheEntry
}

type cacheEntry struct {
	value   string
	expires time.Time
}

// NewCache returns an empty Cache whose entries expire ttl after they are
// stored. A ttl of zero or less means DefaultCacheTTL.
func NewCache(ttl time.Duration) *Cache {
	if ttl <= 0 {
		ttl = DefaultCacheTTL
	}
	return &Cache{ttl: ttl, now: time.Now, entries: make(map[string]cacheEntry)}
}

// Get returns the value cached under key, or what load returns, caching it
// unless load fails; concurrent misses on a key may each call load.
func (c *Cache) Get(key string, load func() (string, error)) (string, error) {
	if v, ok := c.lookup(key); ok {
		return v, nil
	}
	v, err := load()
	if err != nil {
		return "", err
	}
	c.store(key, v)
	return v, nil
}

func (c *Cache) lookup(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || !c.now().Before(e.expires) {
		return "", false
	}
	return e.value, true
}

func (c *Cache) store(key, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for k, e := range c.entries {
		if !now.Before(e.expires) {
			delete(c.entries, k)
		}
	}
	c.entries[key] = cacheEntry{value: value, expires: now.Add(c.ttl)}
}
