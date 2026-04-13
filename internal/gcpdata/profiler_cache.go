package gcpdata

import (
	"sync"

	"github.com/google/pprof/profile"
)

const defaultProfileCacheSize = 10

type profileCacheEntry struct {
	key     string
	profile *profile.Profile
	meta    ProfileMeta
}

// ProfileCache is a bounded LRU cache for parsed pprof profiles.
type ProfileCache struct {
	mu      sync.Mutex
	entries []profileCacheEntry
	maxSize int
}

// NewProfileCache creates a new profile cache with the given maximum size.
// maxSize is clamped to [1, 100] to prevent unbounded memory usage.
func NewProfileCache(maxSize int) *ProfileCache {
	if maxSize <= 0 {
		maxSize = defaultProfileCacheSize
	}
	if maxSize > 100 {
		maxSize = 100
	}
	return &ProfileCache{maxSize: maxSize}
}

// Get retrieves a cached profile by key, moving it to the most-recently-used position.
// The returned *profile.Profile is shared — callers must not mutate it.
// Use profile.Copy() if mutation is needed (e.g. buildDiffProfile copies both inputs
// because base samples are negated in-place before merging, and both originals are
// shared cache entries that must not be modified).
func (c *ProfileCache) Get(key string) (*profile.Profile, ProfileMeta, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, e := range c.entries {
		if e.key == key {
			// Move to end (most recently used).
			c.entries = append(c.entries[:i], c.entries[i+1:]...)
			c.entries = append(c.entries, e)
			return e.profile, e.meta, true
		}
	}
	return nil, ProfileMeta{}, false
}

// Put adds a profile to the cache, evicting the oldest entry if at capacity.
func (c *ProfileCache) Put(key string, p *profile.Profile, meta ProfileMeta) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Remove existing entry with same key.
	for i, e := range c.entries {
		if e.key == key {
			c.entries = append(c.entries[:i], c.entries[i+1:]...)
			break
		}
	}
	// Evict oldest if at capacity.
	if len(c.entries) >= c.maxSize {
		c.entries = c.entries[1:]
	}
	c.entries = append(c.entries, profileCacheEntry{key: key, profile: p, meta: meta})
}

// Len returns the number of entries in the cache.
func (c *ProfileCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
