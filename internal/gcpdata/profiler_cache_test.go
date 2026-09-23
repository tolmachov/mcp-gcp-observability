package gcpdata

import (
	"bytes"
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
	require.True(t, c.Put("key1", data, meta))
	data[0] = 'X'

	got, gotMeta, ok := c.Get("key1")
	require.True(t, ok)
	assert.Equal(t, []byte("source"), got)
	assert.Equal(t, meta, gotMeta)
	assert.True(t, c.Has("key1"))
	assert.False(t, c.Has("key2"))
}

func TestProfileCache_ByteBoundedEviction(t *testing.T) {
	c := newSmallProfileCache(t, 6)
	require.True(t, c.Put("a", []byte("aaa"), ProfileMeta{ProfileID: "a"}))
	require.True(t, c.Put("b", []byte("bb"), ProfileMeta{ProfileID: "b"}))
	require.True(t, c.Put("c", []byte("cccc"), ProfileMeta{ProfileID: "c"}))

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
	require.True(t, c.Put("a", []byte("aa"), ProfileMeta{}))
	require.True(t, c.Put("b", []byte("bb"), ProfileMeta{}))
	c.Get("a")
	require.True(t, c.Put("c", []byte("cc"), ProfileMeta{}))

	_, _, ok := c.Get("a")
	assert.True(t, ok)
	_, _, ok = c.Get("b")
	assert.False(t, ok)
	_, _, ok = c.Get("c")
	assert.True(t, ok)
}

func TestProfileCache_HasRefreshesLRU(t *testing.T) {
	c := newSmallProfileCache(t, 4)
	require.True(t, c.Put("a", []byte("aa"), ProfileMeta{}))
	require.True(t, c.Put("b", []byte("bb"), ProfileMeta{}))
	assert.True(t, c.Has("a"))
	require.True(t, c.Put("c", []byte("cc"), ProfileMeta{}))

	assert.True(t, c.Has("a"))
	assert.False(t, c.Has("b"))
}

func TestProfileCache_ProcessBudgetRejectionKeepsEntries(t *testing.T) {
	c := newSmallProfileCache(t, 5)
	require.True(t, c.Put("a", []byte("aa"), ProfileMeta{}))
	require.True(t, c.Put("b", []byte("bb"), ProfileMeta{}))

	// Saturate the process budget: the insert would need to evict "a" and grow
	// process usage, so it must be rejected without evicting anything.
	reserved := profileCacheProcessBytes - processProfileCacheBytes.Load()
	processProfileCacheBytes.Add(reserved)
	t.Cleanup(func() { processProfileCacheBytes.Add(-reserved) })
	assert.False(t, c.Put("c", []byte("ccc"), ProfileMeta{}))

	assert.Equal(t, 2, c.Len())
	assert.Equal(t, int64(4), c.Bytes())
	assert.True(t, c.Has("a"))
	assert.True(t, c.Has("b"))
}

func TestProfileCache_OverwriteReleasesBudget(t *testing.T) {
	c := newSmallProfileCache(t, 8)
	require.True(t, c.Put("a", []byte("v1"), ProfileMeta{ProfileType: "CPU"}))
	require.True(t, c.Put("a", []byte("version2"), ProfileMeta{ProfileType: "HEAP"}))
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
	assert.False(t, c.Put("too-big", []byte("four"), ProfileMeta{}))
	require.True(t, c.Put("ok", []byte("123"), ProfileMeta{}))
	assert.Equal(t, before+3, processProfileCacheBytes.Load())
	c.Close()
	assert.Equal(t, before, processProfileCacheBytes.Load())
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
