package gcpdata

import (
	"testing"

	"github.com/google/pprof/profile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func makeTestProfile(label string) *profile.Profile {
	return &profile.Profile{
		Comments: []string{label},
	}
}

func TestProfileCache_GetMiss(t *testing.T) {
	c := NewProfileCache(3)
	_, _, ok := c.Get("nonexistent")
	assert.False(t, ok)
}

func TestProfileCache_PutAndGet(t *testing.T) {
	c := NewProfileCache(3)
	p := makeTestProfile("test")
	meta := ProfileMeta{ProfileID: "p1", ProfileType: "CPU"}

	c.Put("key1", p, meta)
	got, gotMeta, ok := c.Get("key1")
	require.True(t, ok)
	assert.Equal(t, p, got)
	assert.Equal(t, meta, gotMeta)
}

func TestProfileCache_Eviction(t *testing.T) {
	c := NewProfileCache(2)
	c.Put("a", makeTestProfile("a"), ProfileMeta{ProfileID: "a"})
	c.Put("b", makeTestProfile("b"), ProfileMeta{ProfileID: "b"})
	c.Put("c", makeTestProfile("c"), ProfileMeta{ProfileID: "c"})

	// "a" should be evicted.
	_, _, ok := c.Get("a")
	assert.False(t, ok)
	// "b" and "c" should still be present.
	_, _, ok = c.Get("b")
	assert.True(t, ok)
	_, _, ok = c.Get("c")
	assert.True(t, ok)
	assert.Equal(t, 2, c.Len())
}

func TestProfileCache_LRUOrder(t *testing.T) {
	c := NewProfileCache(2)
	c.Put("a", makeTestProfile("a"), ProfileMeta{ProfileID: "a"})
	c.Put("b", makeTestProfile("b"), ProfileMeta{ProfileID: "b"})

	// Access "a" to make it recently used.
	c.Get("a")

	// Adding "c" should evict "b" (least recently used), not "a".
	c.Put("c", makeTestProfile("c"), ProfileMeta{ProfileID: "c"})

	_, _, ok := c.Get("a")
	assert.True(t, ok, "a should survive because it was accessed recently")
	_, _, ok = c.Get("b")
	assert.False(t, ok, "b should be evicted as least recently used")
	_, _, ok = c.Get("c")
	assert.True(t, ok)
}

func TestProfileCache_OverwriteSameKey(t *testing.T) {
	c := NewProfileCache(2)
	c.Put("a", makeTestProfile("v1"), ProfileMeta{ProfileID: "a", ProfileType: "CPU"})
	c.Put("a", makeTestProfile("v2"), ProfileMeta{ProfileID: "a", ProfileType: "HEAP"})

	got, gotMeta, ok := c.Get("a")
	require.True(t, ok)
	assert.Equal(t, "v2", got.Comments[0])
	assert.Equal(t, "HEAP", gotMeta.ProfileType)
	assert.Equal(t, 1, c.Len())
}

func TestProfileCache_DefaultSize(t *testing.T) {
	c := NewProfileCache(0)
	assert.Equal(t, defaultProfileCacheSize, c.maxSize)
	c = NewProfileCache(-5)
	assert.Equal(t, defaultProfileCacheSize, c.maxSize)
}
