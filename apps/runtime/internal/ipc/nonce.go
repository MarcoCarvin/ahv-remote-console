package ipc

import (
	"sort"
	"sync"
	"time"
)

// nonceCache prevents replay of HMAC-signed IPC requests. It is bounded in
// size (LRU-ish via timestamp) and entries expire after `ttl`.
type nonceCache struct {
	mu      sync.Mutex
	entries map[string]time.Time
	cap     int
	ttl     time.Duration
	// forcedEvictions counts cap-pressure evictions (the TTL sweep didn't free
	// enough). Non-zero means the cap is undersized for the request volume.
	forcedEvictions int
}

func newNonceCache(cap int, ttl time.Duration) *nonceCache {
	return &nonceCache{
		entries: make(map[string]time.Time, cap),
		cap:     cap,
		ttl:     ttl,
	}
}

// use atomically asserts the nonce has not been seen. Returns false on replay.
func (c *nonceCache) use(nonce string, ts time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if _, dup := c.entries[nonce]; dup {
		return false
	}
	// Garbage-collect expired entries opportunistically.
	if len(c.entries) >= c.cap {
		for k, t := range c.entries {
			if now.Sub(t) > c.ttl {
				delete(c.entries, k)
			}
		}
		if len(c.entries) >= c.cap {
			// Still full after the TTL sweep. Evict the oldest ~25% BY TIMESTAMP,
			// not by map-iteration order: random eviction could drop a freshly
			// inserted nonce (full replay window still ahead of it) while keeping
			// near-expired ones, widening the replay surface. Oldest-first drops
			// the nonces closest to expiring anyway.
			type aged struct {
				key string
				ts  time.Time
			}
			all := make([]aged, 0, len(c.entries))
			for k, t := range c.entries {
				all = append(all, aged{k, t})
			}
			sort.Slice(all, func(i, j int) bool { return all[i].ts.Before(all[j].ts) })
			drop := c.cap / 4
			if drop < 1 {
				drop = 1
			}
			for i := 0; i < drop && i < len(all); i++ {
				delete(c.entries, all[i].key)
			}
			c.forcedEvictions++
		}
	}
	c.entries[nonce] = ts
	return true
}
