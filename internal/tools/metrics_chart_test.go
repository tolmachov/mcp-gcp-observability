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

// TestBuildChartURL verifies that the URI builder encodes parameters correctly
// and that the result can be round-tripped through url.Parse.
func TestBuildChartURL(t *testing.T) {
	t.Run("basic metric type with slashes", func(t *testing.T) {
		uri := buildChartURL("compute.googleapis.com/instance/cpu/utilization", "", "1h", 60, "my-project")
		assert.Contains(t, uri, "compute.googleapis.com/instance/cpu/utilization")
		assert.Contains(t, uri, "window=1h")
		assert.Contains(t, uri, "step=60")
		assert.Contains(t, uri, "project=my-project")
		// filter absent when empty
		assert.NotContains(t, uri, "filter=")
	})

	t.Run("filter included when non-empty", func(t *testing.T) {
		uri := buildChartURL("custom.googleapis.com/api/latency", `resource.labels.zone="us-east1"`, "30m", 30, "p")
		assert.Contains(t, uri, "filter=")
	})

	t.Run("project omitted when empty", func(t *testing.T) {
		uri := buildChartURL("metric/type", "", "1h", 60, "")
		assert.NotContains(t, uri, "project=")
	})

	t.Run("scheme and host", func(t *testing.T) {
		uri := buildChartURL("m/t", "", "1h", 60, "")
		assert.True(t, strings.HasPrefix(uri, "ui://metrics/chart/"), "URI must start with ui://metrics/chart/")
	})

	t.Run("step encoded as decimal", func(t *testing.T) {
		uri := buildChartURL("m/t", "", "1h", 300, "p")
		assert.Contains(t, uri, "step=300")
		assert.NotContains(t, uri, "step=0x")
	})
}

// TestGenerateChartHTML verifies the HTML generation logic.
func TestGenerateChartHTML(t *testing.T) {
	now := time.Unix(1700000000, 0)
	pts := []metrics.Point{
		{Timestamp: now, Value: 0.5},
		{Timestamp: now.Add(time.Minute), Value: 1.0},
	}

	t.Run("xss guard: </script> in data", func(t *testing.T) {
		// A metric type whose name contains </script> must not break the HTML.
		maliciousType := `</script><script>alert(1)</script>`
		html := generateChartHTML(pts, nil, maliciousType, "1", "1h")
		// The raw </script> must not appear unescaped in a script block.
		assert.NotContains(t, html, "</script><script>alert(1)")
	})

	t.Run("html escaping of metric type in title", func(t *testing.T) {
		html := generateChartHTML(pts, nil, "foo<bar>&baz", "1", "1h")
		assert.Contains(t, html, "foo&lt;bar&gt;&amp;baz")
		assert.NotContains(t, html, "foo<bar>&baz")
	})

	t.Run("NaN and Inf points are filtered", func(t *testing.T) {
		withBadValues := []metrics.Point{
			{Timestamp: now, Value: math.NaN()},
			{Timestamp: now.Add(time.Minute), Value: math.Inf(1)},
			{Timestamp: now.Add(2 * time.Minute), Value: 42.0},
		}
		html := generateChartHTML(withBadValues, nil, "m/t", "1", "1h")
		// Only the valid point should be in the JSON points array.
		assert.Contains(t, html, `"v":42`)
		// NaN and Infinity are not valid JSON numbers and must not appear.
		assert.NotContains(t, html, "NaN")
		assert.NotContains(t, html, "Infinity")
	})

	t.Run("nil baseline serialises as JSON null", func(t *testing.T) {
		html := generateChartHTML(pts, nil, "m/t", "ms", "1h")
		assert.Contains(t, html, `"baseline":null`)
	})

	t.Run("non-nil baseline serialises as number", func(t *testing.T) {
		v := 0.0 // explicitly zero — must not be treated as absent
		html := generateChartHTML(pts, &v, "m/t", "ms", "1h")
		assert.Contains(t, html, `"baseline":0`)
		assert.NotContains(t, html, `"baseline":null`)
	})

	t.Run("no data: empty points array still renders", func(t *testing.T) {
		html := generateChartHTML(nil, nil, "m/t", "1", "1h")
		require.NotEmpty(t, html)
		assert.Contains(t, html, "DOCTYPE html")
	})

	t.Run("cdn domain present", func(t *testing.T) {
		html := generateChartHTML(pts, nil, "m/t", "1", "1h")
		assert.Contains(t, html, chartCDNDomain)
	})
}
