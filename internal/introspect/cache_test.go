package introspect

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestCache(ttl time.Duration) (*Cache, *fakeClock) {
	clock := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	c := NewCache(ttl)
	c.now = clock.Now
	return c, clock
}

type counter struct{ calls atomic.Int64 }

func (l *counter) load(key string) func() (string, error) {
	return func() (string, error) {
		n := l.calls.Add(1)
		return fmt.Sprintf("%s#%d", key, n), nil
	}
}

func get(t *testing.T, c *Cache, key string, load func() (string, error), want string) {
	t.Helper()
	got, err := c.Get(key, load)
	if err != nil || got != want {
		t.Errorf("Get(%q) = %q, %v; want %q", key, got, err, want)
	}
}

func TestCacheKeepsValueForTTL(t *testing.T) {
	c, clock := newTestCache(time.Minute)
	var l counter

	get(t, c, "k", l.load("k"), "k#1")
	clock.Advance(time.Minute - time.Nanosecond)
	get(t, c, "k", l.load("k"), "k#1")
	clock.Advance(time.Nanosecond)
	get(t, c, "k", l.load("k"), "k#2")
	get(t, c, "k", l.load("k"), "k#2")
	if n := l.calls.Load(); n != 2 {
		t.Errorf("load ran %d times, want 2", n)
	}
}

func TestCacheTTLIsPerKey(t *testing.T) {
	c, clock := newTestCache(5 * time.Minute)
	var a, b counter

	get(t, c, "a", a.load("a"), "a#1")
	clock.Advance(3 * time.Minute)
	get(t, c, "b", b.load("b"), "b#1")
	clock.Advance(2 * time.Minute)
	get(t, c, "a", a.load("a"), "a#2")
	get(t, c, "b", b.load("b"), "b#1")
	clock.Advance(3 * time.Minute)
	get(t, c, "b", b.load("b"), "b#2")
}

func TestCacheDefaultTTL(t *testing.T) {
	for _, ttl := range []time.Duration{0, -time.Second} {
		c, clock := newTestCache(ttl)
		var l counter
		get(t, c, "k", l.load("k"), "k#1")
		clock.Advance(DefaultCacheTTL - time.Nanosecond)
		get(t, c, "k", l.load("k"), "k#1")
		clock.Advance(time.Nanosecond)
		get(t, c, "k", l.load("k"), "k#2")
	}
}

func TestCacheDoesNotCacheErrors(t *testing.T) {
	c, _ := newTestCache(time.Minute)
	failure := errors.New("catalog unavailable")

	if _, err := c.Get("k", func() (string, error) { return "partial", failure }); !errors.Is(err, failure) {
		t.Fatalf("Get error = %v, want %v", err, failure)
	}
	var l counter
	get(t, c, "k", l.load("k"), "k#1")
	get(t, c, "k", l.load("k"), "k#1")
}

func TestCacheDropsExpiredEntriesOnStore(t *testing.T) {
	c, clock := newTestCache(time.Minute)
	var l counter
	get(t, c, "old1", l.load("old1"), "old1#1")
	get(t, c, "old2", l.load("old2"), "old2#2")
	clock.Advance(time.Minute)
	get(t, c, "new", l.load("new"), "new#3")

	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) != 1 {
		t.Errorf("entries = %v, want only the new one", c.entries)
	}
}

func TestCacheConcurrentUse(t *testing.T) {
	c, clock := newTestCache(time.Minute)
	var l counter
	var wg sync.WaitGroup
	for g := range 16 {
		wg.Go(func() {
			for i := range 200 {
				key := fmt.Sprintf("k%d", (g+i)%8)
				got, err := c.Get(key, l.load(key))
				if err != nil || len(got) <= len(key) || got[:len(key)+1] != key+"#" {
					t.Errorf("Get(%q) = %q, %v", key, got, err)
					return
				}
				if i%50 == 0 {
					clock.Advance(20 * time.Second)
				}
			}
		})
	}
	wg.Wait()
	if n := l.calls.Load(); n < 8 {
		t.Errorf("load ran %d times for 8 keys", n)
	}
}
