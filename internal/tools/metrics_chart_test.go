package tools

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
)

// TestRenderChartHTML verifies that the embedded HTML template renders correctly.
func TestRenderChartHTML(t *testing.T) {
	html := renderChartHTML()
	require.NotEmpty(t, html)

	t.Run("valid html skeleton", func(t *testing.T) {
		assert.Contains(t, html, "<!DOCTYPE html>")
		assert.Contains(t, html, "<canvas id=\"c\"")
	})

	t.Run("cdn domain present", func(t *testing.T) {
		assert.Contains(t, html, chartCDNDomain)
	})

	t.Run("bridge initializes with tool name", func(t *testing.T) {
		assert.Contains(t, html, "metrics_snapshot")
	})

	t.Run("static uri is not embedded in html", func(t *testing.T) {
		// HTML is a template; URIs come via bridge, not baked in.
		assert.NotContains(t, html, "ui://metrics/chart/")
	})

	t.Run("css variables used for colors", func(t *testing.T) {
		assert.Contains(t, html, "--color-background-primary")
		assert.Contains(t, html, "--color-text-primary")
	})
}

// TestToChartPoints verifies NaN/Inf filtering and correct conversion.
func TestToChartPoints(t *testing.T) {
	now := time.Unix(1700000000, 0)

	t.Run("NaN and Inf are filtered out", func(t *testing.T) {
		pts := []metrics.Point{
			{Timestamp: now, Value: math.NaN()},
			{Timestamp: now.Add(time.Minute), Value: math.Inf(1)},
			{Timestamp: now.Add(2 * time.Minute), Value: math.Inf(-1)},
			{Timestamp: now.Add(3 * time.Minute), Value: 42.0},
		}
		got := toChartPoints(pts)
		require.Len(t, got, 1)
		assert.Equal(t, int64(now.Add(3*time.Minute).Unix()), got[0].TS)
		assert.Equal(t, 42.0, got[0].V)
	})

	t.Run("empty input returns empty slice", func(t *testing.T) {
		got := toChartPoints(nil)
		assert.Empty(t, got)
	})

	t.Run("zero value is preserved", func(t *testing.T) {
		pts := []metrics.Point{{Timestamp: now, Value: 0.0}}
		got := toChartPoints(pts)
		require.Len(t, got, 1)
		assert.Equal(t, 0.0, got[0].V)
	})

	t.Run("timestamps preserved as unix seconds", func(t *testing.T) {
		pts := []metrics.Point{{Timestamp: now, Value: 1.0}}
		got := toChartPoints(pts)
		assert.Equal(t, now.Unix(), got[0].TS)
	})
}

// TestChartStaticURI verifies the constant URI matches the registered resource.
func TestChartStaticURI(t *testing.T) {
	assert.True(t, strings.HasPrefix(chartStaticURI, "ui://"), "chartStaticURI must use ui:// scheme")
	assert.NotContains(t, chartStaticURI, "{", "chartStaticURI must be a static URI, not a template")
}
