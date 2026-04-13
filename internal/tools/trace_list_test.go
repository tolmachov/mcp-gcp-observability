package tools

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseTraceTimeRange(t *testing.T) {
	t.Run("both empty defaults to last 1 hour", func(t *testing.T) {
		before := time.Now().UTC()
		start, end, err := parseTraceTimeRange("", "")
		after := time.Now().UTC()

		require.NoError(t, err)
		assert.True(t, end.After(before.Add(-time.Second)))
		assert.True(t, end.Before(after.Add(time.Second)))
		assert.True(t, start.Before(end))

		diff := end.Sub(start)
		assert.InDelta(t, time.Hour.Seconds(), diff.Seconds(), 1.0)
	})

	t.Run("both specified", func(t *testing.T) {
		start, end, err := parseTraceTimeRange("2025-01-15T10:00:00Z", "2025-01-15T11:00:00Z")
		require.NoError(t, err)
		assert.Equal(t, "2025-01-15T10:00:00Z", start.Format(time.RFC3339))
		assert.Equal(t, "2025-01-15T11:00:00Z", end.Format(time.RFC3339))
	})

	t.Run("only end_time", func(t *testing.T) {
		start, end, err := parseTraceTimeRange("", "2025-01-15T12:00:00Z")
		require.NoError(t, err)
		assert.Equal(t, "2025-01-15T12:00:00Z", end.Format(time.RFC3339))
		assert.Equal(t, "2025-01-15T11:00:00Z", start.Format(time.RFC3339))
	})

	t.Run("only start_time", func(t *testing.T) {
		start, end, err := parseTraceTimeRange("2025-01-15T10:00:00Z", "")
		require.NoError(t, err)
		assert.Equal(t, "2025-01-15T10:00:00Z", start.Format(time.RFC3339))
		assert.True(t, end.After(start))
	})

	t.Run("end before start", func(t *testing.T) {
		_, _, err := parseTraceTimeRange("2025-01-15T12:00:00Z", "2025-01-15T10:00:00Z")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "end_time must be after start_time")
	})

	t.Run("invalid start_time format", func(t *testing.T) {
		_, _, err := parseTraceTimeRange("not-a-date", "")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "invalid start_time")
	})

	t.Run("invalid end_time format", func(t *testing.T) {
		_, _, err := parseTraceTimeRange("", "not-a-date")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "invalid end_time")
	})
}
