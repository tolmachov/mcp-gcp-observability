package tools

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
)

// TestRenderCompareChartHTML verifies that the embedded compare HTML template renders correctly.
func TestRenderCompareChartHTML(t *testing.T) {
	html := renderCompareChartHTML()
	require.NotEmpty(t, html)

	t.Run("valid html skeleton", func(t *testing.T) {
		assert.Contains(t, html, "<!DOCTYPE html>")
		assert.Contains(t, html, "<svg id=\"chart\"")
	})

	t.Run("no external scripts", func(t *testing.T) {
		assert.NotContains(t, html, "cdn.", "HTML must not reference any CDN")
		assert.NotContains(t, html, "<script src=", "HTML must not load external scripts")
	})

	t.Run("bridge initializes with tool name", func(t *testing.T) {
		assert.Contains(t, html, "metrics_compare")
	})

	t.Run("bridge uses appCapabilities not capabilities", func(t *testing.T) {
		assert.Contains(t, html, "appCapabilities", "ui/initialize must use appCapabilities field")
	})

	t.Run("initialized sent unconditionally via finally", func(t *testing.T) {
		assert.Contains(t, html, "ui/notifications/initialized")
		assert.Contains(t, html, ".finally(", "initialized must be sent in finally() to cover timeout/error paths")
	})

	t.Run("static uri is not embedded in html", func(t *testing.T) {
		assert.NotContains(t, html, compareChartStaticURI)
	})

	t.Run("dark theme colors present in CSS", func(t *testing.T) {
		assert.Contains(t, html, "#0d1117", "dark background color must be present")
		assert.Contains(t, html, "#e6edf3", "dark primary text color must be present")
	})

	t.Run("theme follows host via host-context-changed", func(t *testing.T) {
		assert.Contains(t, html, "host-context-changed", "bridge must handle host-context-changed notification")
		assert.Contains(t, html, "applyTheme", "applyTheme function must be defined and called")
	})

	t.Run("dual series colors present", func(t *testing.T) {
		assert.Contains(t, html, "#4e9af1", "series A blue color must be present")
		assert.Contains(t, html, "#3fb950", "series B green color must be present")
	})

	t.Run("reads chart_points_a and chart_points_b", func(t *testing.T) {
		assert.Contains(t, html, "chart_points_a")
		assert.Contains(t, html, "chart_points_b")
	})
}

// TestCompareChartStaticURI verifies the constant URI matches the registered resource.
func TestCompareChartStaticURI(t *testing.T) {
	assert.Equal(t, "ui://metrics/compare", compareChartStaticURI, "compareChartStaticURI must be the expected static address")
	assert.True(t, strings.HasPrefix(compareChartStaticURI, "ui://"), "compareChartStaticURI must use ui:// scheme")
	assert.NotContains(t, compareChartStaticURI, "{", "compareChartStaticURI must be a static URI, not a template")
}

// TestCompareCallResult verifies that compareCallResult strips chart points from
// LLM-facing content while keeping the rest of the result intact.
func TestCompareCallResult(t *testing.T) {
	now := time.Unix(1700000000, 0)
	pts := []chartPoint{
		{TS: now.Unix(), V: 0.1},
		{TS: now.Add(time.Minute).Unix(), V: 0.2},
	}
	result := &CompareResult{
		MetricType:   "compute.googleapis.com/instance/cpu/utilization",
		Unit:         "ratio",
		WindowALabel: "prev_hour",
		WindowBLabel: "last_hour",
		WindowAMean:  0.089,
		WindowBMean:  0.12,
		DeltaPct:     34.8,
		TrendShift:   "degraded",
		ChartPointsA: pts,
		ChartPointsB: pts,
	}

	callResult := compareCallResult(result)

	t.Run("not an error result", func(t *testing.T) {
		assert.False(t, callResult.IsError)
	})

	t.Run("meta carries compare URI", func(t *testing.T) {
		ui, ok := callResult.Meta["ui"].(map[string]any)
		require.True(t, ok, "_meta.ui must be present")
		assert.Equal(t, compareChartStaticURI, ui["resourceUri"])
	})

	t.Run("content is valid JSON with analysis fields", func(t *testing.T) {
		require.Len(t, callResult.Content, 1)
		text, ok := callResult.Content[0].(*mcp.TextContent)
		require.True(t, ok)
		var parsed map[string]any
		require.NoError(t, json.Unmarshal([]byte(text.Text), &parsed))
		assert.Equal(t, "prev_hour", parsed["window_a_label"])
		assert.Equal(t, float64(34.8), parsed["delta_pct"])
		assert.Equal(t, "compute.googleapis.com/instance/cpu/utilization", parsed["metric_type"])
		assert.Equal(t, "ratio", parsed["unit"])
	})

	t.Run("chart_points_a and chart_points_b absent from LLM content", func(t *testing.T) {
		text := callResult.Content[0].(*mcp.TextContent).Text
		assert.NotContains(t, text, "chart_points_a", "chart_points_a must not be in LLM-facing content")
		assert.NotContains(t, text, "chart_points_b", "chart_points_b must not be in LLM-facing content")
	})

	t.Run("caller struct not mutated (chart points intact)", func(t *testing.T) {
		require.NotNil(t, result.ChartPointsA, "compareCallResult must not mutate the original result")
		require.NotNil(t, result.ChartPointsB, "compareCallResult must not mutate the original result")
		assert.Len(t, result.ChartPointsA, 2)
		assert.Len(t, result.ChartPointsB, 2)
	})
}

// TestCompareCallResultNilPoints verifies that compareCallResult handles the no-data path
// (where ChartPointsA/B are never populated) without errors, and that the chart_points
// keys are absent from the LLM-facing JSON output (omitempty on nil slices).
func TestCompareCallResultNilPoints(t *testing.T) {
	result := &CompareResult{
		MetricType:   "compute.googleapis.com/instance/cpu/utilization",
		WindowALabel: "prev_hour",
		WindowBLabel: "last_hour",
		TrendShift:   "unchanged",
		NoData:       true,
		// ChartPointsA and ChartPointsB intentionally left nil (no-data path).
	}

	callResult := compareCallResult(result)

	t.Run("not an error result", func(t *testing.T) {
		assert.False(t, callResult.IsError)
	})

	t.Run("meta carries compare URI", func(t *testing.T) {
		ui, ok := callResult.Meta["ui"].(map[string]any)
		require.True(t, ok, "_meta.ui must be present")
		assert.Equal(t, compareChartStaticURI, ui["resourceUri"])
	})

	t.Run("chart_points keys absent from LLM content", func(t *testing.T) {
		require.Len(t, callResult.Content, 1)
		text := callResult.Content[0].(*mcp.TextContent).Text
		var parsed map[string]any
		require.NoError(t, json.Unmarshal([]byte(text), &parsed))
		_, hasA := parsed["chart_points_a"]
		_, hasB := parsed["chart_points_b"]
		assert.False(t, hasA, "chart_points_a must be absent when nil (omitempty)")
		assert.False(t, hasB, "chart_points_b must be absent when nil (omitempty)")
	})

	t.Run("analysis fields present", func(t *testing.T) {
		text := callResult.Content[0].(*mcp.TextContent).Text
		var parsed map[string]any
		require.NoError(t, json.Unmarshal([]byte(text), &parsed))
		assert.Equal(t, "unchanged", parsed["trend_shift"])
		assert.Equal(t, true, parsed["no_data"])
	})
}

func TestClassificationSeverity(t *testing.T) {
	tests := []struct {
		class metrics.Classification
		want  int
	}{
		{metrics.ClassImprovement, -1},
		{metrics.ClassInsufficientData, 0},
		{metrics.ClassStable, 0},
		{metrics.ClassNoisy, 1},
		{metrics.ClassRecovery, 2},
		{metrics.ClassSpike, 3},
		{metrics.ClassFlapping, 4},
		{metrics.ClassStepRegression, 5},
		{metrics.ClassSustainedRegression, 6},
		{metrics.ClassSaturation, 7},
	}

	for _, tt := range tests {
		t.Run(string(tt.class), func(t *testing.T) {
			got := classificationSeverity(tt.class)
			assert.Equal(t, tt.want, got, "classificationSeverity(%v)", tt.class)
		})
	}
}

func TestClassificationSeverityOrdering(t *testing.T) {
	// Severity must be non-decreasing and end in saturation as the most
	// severe. insufficient_data deliberately shares rank with stable because
	// "we don't know" is not an alert.
	classes := []metrics.Classification{
		metrics.ClassImprovement,
		metrics.ClassStable,
		metrics.ClassNoisy,
		metrics.ClassRecovery,
		metrics.ClassSpike,
		metrics.ClassFlapping,
		metrics.ClassStepRegression,
		metrics.ClassSustainedRegression,
		metrics.ClassSaturation,
	}
	for i := 1; i < len(classes); i++ {
		prev := classificationSeverity(classes[i-1])
		curr := classificationSeverity(classes[i])
		assert.Greater(t, curr, prev, "severity(%v)=%d should be > severity(%v)=%d", classes[i], curr, classes[i-1], prev)
	}
}

func TestClassificationSeverityUnknownIsHigh(t *testing.T) {
	// Unknown classifications should be treated as high severity (fail-safe).
	got := classificationSeverity(metrics.Classification("some_future_classification"))
	assert.GreaterOrEqual(t, got, classificationSeverity(metrics.ClassStepRegression), "unknown severity should be >= step_regression")
}
