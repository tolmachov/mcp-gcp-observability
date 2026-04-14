package tools

import (
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSnapshotCallResult verifies that snapshotCallResult excludes chart_points
// from the LLM-facing content while leaving the original struct intact.
func TestSnapshotCallResult(t *testing.T) {
	pts := []chartPoint{{TS: 1700000000, V: 1.0}, {TS: 1700000060, V: 2.0}}
	result := &MetricSnapshotResult{
		MetricType:  "test/metric",
		Kind:        "GAUGE",
		Unit:        "1",
		Trend:       "stable",
		ChartPoints: pts,
	}

	cr := snapshotCallResult(result)

	t.Run("content does not contain chart_points", func(t *testing.T) {
		require.Len(t, cr.Content, 1)
		text := cr.Content[0].(*mcp.TextContent).Text
		assert.NotContains(t, text, "chart_points", "LLM content must not include raw time-series data")
		var parsed map[string]any
		require.NoError(t, json.Unmarshal([]byte(text), &parsed))
		_, hasChartPoints := parsed["chart_points"]
		assert.False(t, hasChartPoints, "chart_points key must be absent from marshaled content")
	})

	t.Run("original struct is not mutated", func(t *testing.T) {
		assert.Equal(t, pts, result.ChartPoints, "snapshotCallResult must not modify the caller's struct")
	})

	t.Run("not an error result", func(t *testing.T) {
		assert.False(t, cr.IsError)
	})

	t.Run("meta carries static chart URI", func(t *testing.T) {
		ui, ok := cr.Meta["ui"].(map[string]any)
		require.True(t, ok, "_meta.ui must be present")
		assert.Equal(t, chartStaticURI, ui["resourceUri"])
	})
}

// TestEmptyWindowMessage verifies that the "metric exists but window is
// empty" error message is tailored per metric kind and acknowledges the
// presence of a label filter when one was used. These branches are hard to
// hit from integration tests because each handler only surfaces one kind
// per test — a dedicated unit test locks them in.
func TestEmptyWindowMessage(t *testing.T) {
	const metric = "pubsub.googleapis.com/subscription/dead_letter_message_count"
	const window = "1h"

	tests := []struct {
		name        string
		kind        string
		labelFilter string
		// substrs are phrases that must all appear in the message. Used
		// for structural assertions rather than brittle exact-match checks.
		wantSubstrs []string
		// notSubstrs are phrases that must NOT appear. Guards against
		// regressions to the old "verify the metric_type" wording.
		notSubstrs []string
	}{
		{
			name: "delta counter with no filter suggests inactive counter",
			kind: "DELTA",
			wantSubstrs: []string{
				metric,
				window,
				"registered in Cloud Monitoring",
				"no events occurred",
				"dead_letter_message_count",
			},
			notSubstrs: []string{"verify the metric_type", "No data found"},
		},
		{
			name: "cumulative counter uses the same delta wording",
			kind: "CUMULATIVE",
			wantSubstrs: []string{
				"DELTA/CUMULATIVE counters",
				"no events occurred",
			},
			notSubstrs: []string{"verify the metric_type"},
		},
		{
			name: "gauge points at resources, not events",
			kind: "GAUGE",
			wantSubstrs: []string{
				"GAUGE metrics",
				"resources are reporting values",
			},
			notSubstrs: []string{"no events occurred"},
		},
		{
			name: "unknown kind falls through with no kind hint",
			kind: "",
			wantSubstrs: []string{
				"registered in Cloud Monitoring",
			},
			notSubstrs: []string{"no events occurred", "GAUGE metrics"},
		},
		{
			name:        "label filter present swaps the suffix",
			kind:        "DELTA",
			labelFilter: `resource.labels.subscription_id="sub-a"`,
			wantSubstrs: []string{
				`label filter "resource.labels.subscription_id=\"sub-a\"" may also be excluding`,
			},
			notSubstrs: []string{
				"Try widening the window or removing any dimension/filter",
			},
		},
		{
			name: "no label filter uses the widening hint",
			kind: "DELTA",
			wantSubstrs: []string{
				"Try widening the window or removing any dimension/filter",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := emptyWindowMessage(metric, window, tc.kind, tc.labelFilter)
			for _, s := range tc.wantSubstrs {
				assert.Contains(t, got, s, "message should contain substring")
			}
			for _, s := range tc.notSubstrs {
				assert.NotContains(t, got, s, "message should not contain forbidden phrase")
			}
		})
	}
}
