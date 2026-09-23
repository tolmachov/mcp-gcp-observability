package gcpdata

import (
	"bytes"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newSmallProfileCache(t *testing.T, max int64) *ProfileCache {
	t.Helper()
	c := NewProfileCache()
	c.maxBytes = max
	t.Cleanup(c.Close)
	return c
}

func TestProfileCache_GetMiss(t *testing.T) {
	c := newSmallProfileCache(t, 16)
	_, _, ok := c.Get("nonexistent")
	assert.False(t, ok)
}

func TestProfileCache_PutCopiesBytes(t *testing.T) {
	c := newSmallProfileCache(t, 16)
	data := []byte("source")
	meta := ProfileMeta{ProfileID: "p1", ProfileType: "CPU"}
	require.NoError(t, c.Put("key1", data, meta, nil))
	data[0] = 'X'

	got, gotMeta, ok := c.Get("key1")
	require.True(t, ok)
	assert.Equal(t, []byte("source"), got)
	assert.Equal(t, meta, gotMeta)
	_, _, ok = c.Get("key2")
	assert.False(t, ok)
}

func TestProfileCache_ByteBoundedEviction(t *testing.T) {
	c := newSmallProfileCache(t, 6)
	require.NoError(t, c.Put("a", []byte("aaa"), ProfileMeta{ProfileID: "a"}, nil))
	require.NoError(t, c.Put("b", []byte("bb"), ProfileMeta{ProfileID: "b"}, nil))
	require.NoError(t, c.Put("c", []byte("cccc"), ProfileMeta{ProfileID: "c"}, nil))

	_, _, ok := c.Get("a")
	assert.False(t, ok)
	_, _, ok = c.Get("b")
	assert.True(t, ok)
	_, _, ok = c.Get("c")
	assert.True(t, ok)
	assert.Equal(t, int64(6), c.Bytes())
}

func TestProfileCache_LRUOrder(t *testing.T) {
	c := newSmallProfileCache(t, 4)
	require.NoError(t, c.Put("a", []byte("aa"), ProfileMeta{}, nil))
	require.NoError(t, c.Put("b", []byte("bb"), ProfileMeta{}, nil))
	c.Get("a")
	require.NoError(t, c.Put("c", []byte("cc"), ProfileMeta{}, nil))

	_, _, ok := c.Get("a")
	assert.True(t, ok)
	_, _, ok = c.Get("b")
	assert.False(t, ok)
	_, _, ok = c.Get("c")
	assert.True(t, ok)
}

func TestProfileCache_ProcessBudgetRejectionKeepsEntries(t *testing.T) {
	c := newSmallProfileCache(t, 5)
	require.NoError(t, c.Put("a", []byte("aa"), ProfileMeta{}, nil))
	require.NoError(t, c.Put("b", []byte("bb"), ProfileMeta{}, nil))

	// Saturate the process budget: the insert would need to evict "a" and grow
	// process usage, so it must be rejected without evicting anything.
	reserved := profileCacheProcessBytes - processProfileCacheBytes.Load()
	processProfileCacheBytes.Add(reserved)
	t.Cleanup(func() { processProfileCacheBytes.Add(-reserved) })
	assert.Error(t, c.Put("c", []byte("ccc"), ProfileMeta{}, nil))

	assert.Equal(t, 2, c.Len())
	assert.Equal(t, int64(4), c.Bytes())
	assert.True(t, c.contains("a"))
	assert.True(t, c.contains("b"))
}

func TestProfileCache_OverwriteReleasesBudget(t *testing.T) {
	c := newSmallProfileCache(t, 8)
	require.NoError(t, c.Put("a", []byte("v1"), ProfileMeta{ProfileType: "CPU"}, nil))
	require.NoError(t, c.Put("a", []byte("version2"), ProfileMeta{ProfileType: "HEAP"}, nil))
	got, meta, ok := c.Get("a")
	require.True(t, ok)
	assert.True(t, bytes.Equal([]byte("version2"), got))
	assert.Equal(t, "HEAP", meta.ProfileType)
	assert.Equal(t, 1, c.Len())
	assert.Equal(t, int64(8), c.Bytes())
}

func TestProfileCache_RejectsOversizedEntryAndCloseReleasesProcessBudget(t *testing.T) {
	before := processProfileCacheBytes.Load()
	c := NewProfileCache()
	c.maxBytes = 3
	assert.Error(t, c.Put("too-big", []byte("four"), ProfileMeta{}, nil))
	require.NoError(t, c.Put("ok", []byte("123"), ProfileMeta{}, nil))
	assert.Equal(t, before+3, processProfileCacheBytes.Load())
	c.Close()
	assert.Equal(t, before, processProfileCacheBytes.Load())
}

// contains reports whether key is cached without refreshing its LRU position.
func (c *ProfileCache) contains(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.ContainsFunc(c.entries, func(e profileCacheEntry) bool { return e.key == key })
}

func (c *ProfileCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

func (c *ProfileCache) Bytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytes
}

// TestProfileCache_OverwriteInsideEvictionRange covers an overwrite whose old
// entry is not the LRU one but still falls inside the evicted prefix: it must
// be released once, not twice, and the budgets must match the survivors.
func TestProfileCache_OverwriteInsideEvictionRange(t *testing.T) {
	before := processProfileCacheBytes.Load()
	c := newSmallProfileCache(t, 6)
	require.NoError(t, c.Put("a", []byte("aa"), ProfileMeta{}, nil))
	require.NoError(t, c.Put("b", []byte("bb"), ProfileMeta{}, nil))
	require.NoError(t, c.Put("c", []byte("cc"), ProfileMeta{}, nil))

	// "b" sits at index 1; fitting 5 bytes evicts a, b and c.
	require.NoError(t, c.Put("b", []byte("bbbbb"), ProfileMeta{}, nil))

	assert.Equal(t, 1, c.Len())
	assert.Equal(t, int64(5), c.Bytes())
	assert.True(t, c.contains("b"))
	assert.Equal(t, before+5, processProfileCacheBytes.Load())
}

func TestProfileCache_ShrinkingOverwriteSucceedsWhenProcessBudgetFull(t *testing.T) {
	c := newSmallProfileCache(t, 8)
	require.NoError(t, c.Put("a", []byte("aaaa"), ProfileMeta{}, nil))

	reserved := profileCacheProcessBytes - processProfileCacheBytes.Load()
	processProfileCacheBytes.Add(reserved)
	t.Cleanup(func() { processProfileCacheBytes.Add(-reserved) })

	require.NoError(t, c.Put("a", []byte("aa"), ProfileMeta{}, nil), "shrinking frees budget, so it needs none")
	assert.Equal(t, int64(2), c.Bytes())
	assert.Equal(t, profileCacheProcessBytes-2, processProfileCacheBytes.Load())
}

func TestProfileCache_PutAfterCloseIsRejected(t *testing.T) {
	before := processProfileCacheBytes.Load()
	c := NewProfileCache()
	c.Close()
	assert.ErrorContains(t, c.Put("a", []byte("aa"), ProfileMeta{}, nil), "closed")
	assert.Equal(t, 0, c.Len())
	assert.Equal(t, before, processProfileCacheBytes.Load())
}

func TestProfileCache_PutNamesRejectionCause(t *testing.T) {
	c := newSmallProfileCache(t, 3)
	assert.ErrorContains(t, c.Put("empty", nil, ProfileMeta{}, nil), "empty")
	assert.ErrorContains(t, c.Put("big", []byte("four"), ProfileMeta{}, nil), "per-user cache limit")

	reserved := profileCacheProcessBytes - processProfileCacheBytes.Load()
	processProfileCacheBytes.Add(reserved)
	t.Cleanup(func() { processProfileCacheBytes.Add(-reserved) })
	assert.ErrorIs(t, c.Put("ok", []byte("ok"), ProfileMeta{}, nil), errProcessCacheFull)
}

// TestProfileCache_PutNeverEvictsKeptEntries pins the prefetch eviction rule:
// an insert may evict other entries but not the kept ones, and a rejected
// insert leaves the cache unchanged.
func TestProfileCache_PutNeverEvictsKeptEntries(t *testing.T) {
	c := newSmallProfileCache(t, 4)
	require.NoError(t, c.Put("old", []byte("oo"), ProfileMeta{}, nil))
	require.NoError(t, c.Put("a", []byte("aa"), ProfileMeta{}, nil))
	keep := map[string]bool{"a": true, "b": true, "c": true}

	require.NoError(t, c.Put("b", []byte("bb"), ProfileMeta{}, keep), "evicting an entry outside keep is allowed")
	_, _, ok := c.Get("old")
	assert.False(t, ok)

	require.ErrorIs(t, c.Put("c", []byte("cc"), ProfileMeta{}, keep), errWouldEvict)
	assert.Equal(t, 2, c.Len())
	assert.Equal(t, int64(4), c.Bytes())
	for _, key := range []string{"a", "b"} {
		_, _, ok := c.Get(key)
		assert.True(t, ok, key)
	}
}

func TestProfileCache_DeleteReleasesBudget(t *testing.T) {
	before := processProfileCacheBytes.Load()
	c := newSmallProfileCache(t, 8)
	require.NoError(t, c.Put("a", []byte("aaa"), ProfileMeta{}, nil))
	c.Delete("a")
	c.Delete("missing")
	assert.Equal(t, 0, c.Len())
	assert.Equal(t, int64(0), c.Bytes())
	assert.Equal(t, before, processProfileCacheBytes.Load())
}
