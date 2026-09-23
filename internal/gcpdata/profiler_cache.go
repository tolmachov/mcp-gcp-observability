package gcpdata

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
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
	closed   bool // set by Close; later inserts are rejected so no budget leaks
}

func NewProfileCache() *ProfileCache { return &ProfileCache{maxBytes: profileCacheUserBytes} }

// Get returns the cached bytes for key and marks the entry most recently used.
// The returned slice is shared with the cache and must not be modified.
func (c *ProfileCache) Get(key string) ([]byte, ProfileMeta, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	i := c.touch(key)
	if i < 0 {
		return nil, ProfileMeta{}, false
	}
	e := c.entries[i]
	return e.data, e.meta, true
}

// touch moves the entry for key to the most-recently-used end and returns its
// new index, or -1 when key is not cached. c.mu must be held.
func (c *ProfileCache) touch(key string) int {
	i := slices.IndexFunc(c.entries, func(e profileCacheEntry) bool { return e.key == key })
	if i < 0 {
		return -1
	}
	e := c.entries[i]
	c.entries = append(slices.Delete(c.entries, i, i+1), e)
	return len(c.entries) - 1
}

// Put caches a copy of compressed data when both the per-user and process
// budgets permit, evicting the key's previous value and least recently used
// entries to fit the per-user budget. The process budget is reserved before
// anything is evicted, so a rejected insert leaves the cache unchanged. The
// returned error names why data was not cached: it is empty, larger than the
// per-user budget on its own, the process budget is full, or the cache is
// closed. Rejected data remains usable for the current request.
func (c *ProfileCache) Put(key string, data []byte, meta ProfileMeta) error {
	size := int64(len(data))
	if size == 0 {
		return errors.New("empty profile")
	}
	if size > c.maxBytes {
		return fmt.Errorf("profile is %d bytes, over the %d-byte per-user cache limit", size, c.maxBytes)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("profile cache is closed")
	}
	old := slices.IndexFunc(c.entries, func(e profileCacheEntry) bool { return e.key == key })
	var freed int64
	if old >= 0 {
		freed = int64(len(c.entries[old].data))
	}
	evict := 0 // entries [0, evict) are evicted; size <= maxBytes bounds it by len(entries)
	for ; c.bytes-freed+size > c.maxBytes; evict++ {
		if evict != old {
			freed += int64(len(c.entries[evict].data))
		}
	}
	delta := size - freed
	if total := processProfileCacheBytes.Add(delta); delta > 0 && total > profileCacheProcessBytes {
		processProfileCacheBytes.Add(-delta)
		return fmt.Errorf("process-wide profile cache budget of %d bytes is full", profileCacheProcessBytes)
	}
	kept := slices.Delete(c.entries, 0, evict)
	if old >= evict {
		kept = slices.Delete(kept, old-evict, old-evict+1)
	}
	c.entries = append(kept, profileCacheEntry{key: key, data: bytes.Clone(data), meta: meta})
	c.bytes += delta
	return nil
}

// Delete drops key from the cache, releasing its budget.
func (c *ProfileCache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	i := slices.IndexFunc(c.entries, func(e profileCacheEntry) bool { return e.key == key })
	if i < 0 {
		return
	}
	size := int64(len(c.entries[i].data))
	c.entries = slices.Delete(c.entries, i, i+1)
	c.bytes -= size
	processProfileCacheBytes.Add(-size)
}

// Close releases every entry and its process budget; the cache stays empty.
func (c *ProfileCache) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	processProfileCacheBytes.Add(-c.bytes)
	c.entries = nil
	c.bytes = 0
}
