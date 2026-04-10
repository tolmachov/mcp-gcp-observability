package tools

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

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
