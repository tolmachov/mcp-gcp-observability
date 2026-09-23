package gcpdata

import (
	"bytes"
	"sync"
	"sync/atomic"
)

const (
	profileCacheUserBytes    int64 = 16 << 20
	profileCacheProcessBytes int64 = 64 << 20
)

var processProfileCacheBytes atomic.Int64

func profileCacheKey(project, profileName string) string { return project + "/" + profileName }

type profileCacheEntry struct {
	key  string
	data []byte
	meta ProfileMeta
}

// ProfileCache is a byte-bounded LRU of compressed source profiles. Parsed
// pprof graphs are request-local and are never retained.
type ProfileCache struct {
	mu       sync.Mutex
	entries  []profileCacheEntry
	bytes    int64
	maxBytes int64
}

func NewProfileCache() *ProfileCache { return &ProfileCache{maxBytes: profileCacheUserBytes} }

func (c *ProfileCache) Get(key string) ([]byte, ProfileMeta, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, e := range c.entries {
		if e.key == key {
			c.entries = append(c.entries[:i], c.entries[i+1:]...)
			c.entries = append(c.entries, e)
			return bytes.Clone(e.data), e.meta, true
		}
	}
	return nil, ProfileMeta{}, false
}

// Put caches compressed data when both the per-user and process budgets permit.
// Oversized or process-saturated entries remain usable for the current request
// but are intentionally not cached.
func (c *ProfileCache) Put(key string, data []byte, meta ProfileMeta) bool {
	size := int64(len(data))
	if size == 0 || size > c.maxBytes {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, e := range c.entries {
		if e.key == key {
			c.remove(i)
			break
		}
	}
	for c.bytes+size > c.maxBytes && len(c.entries) > 0 {
		c.remove(0)
	}
	if processProfileCacheBytes.Add(size) > profileCacheProcessBytes {
		processProfileCacheBytes.Add(-size)
		return false
	}
	c.entries = append(c.entries, profileCacheEntry{key: key, data: bytes.Clone(data), meta: meta})
	c.bytes += size
	return true
}

func (c *ProfileCache) remove(i int) {
	size := int64(len(c.entries[i].data))
	c.entries = append(c.entries[:i], c.entries[i+1:]...)
	c.bytes -= size
	processProfileCacheBytes.Add(-size)
}

func (c *ProfileCache) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	processProfileCacheBytes.Add(-c.bytes)
	c.entries = nil
	c.bytes = 0
}
