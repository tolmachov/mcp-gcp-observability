package server

import (
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGenerateMetricsRegistryPromptMentionsAggregation is a regression test
// that ensures the generate-metrics-registry MCP prompt describes the
// "aggregation" field. The prompt is a large string constant inside
// server.go registered via AddPrompt — there is no runtime API to inspect
// it without mocking the whole mcp-go server, so this test reads the Go
// source file and asserts that the prompt text contains the critical
// keywords. If someone removes the aggregation section from the prompt
// in the future, this test fails loudly.
//
// We tolerate three encoding shapes for the prompt body so a refactor
// from `fmt.Sprintf(\`...\`)` to a raw const or another templating call
// does not look like a regression in this test:
//   - fmt.Sprintf(`...`)  (current shape)
//   - `...` raw literals anywhere after the start marker
//   - "..." double-quoted strings (best-effort, last resort)
//
// In all three cases we scope to the region between the prompt's
// registration marker and the next AddPrompt/end of file, then assert
// the keyword set is present anywhere in that region.
func TestGenerateMetricsRegistryPromptMentionsAggregation(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	serverGoPath := strings.TrimSuffix(thisFile, "prompt_aggregation_test.go") + "server.go"

	data, err := os.ReadFile(serverGoPath)
	require.NoError(t, err)
	src := string(data)

	// Scope the search to the generate-metrics-registry prompt block so
	// we don't match random other occurrences of "aggregation" elsewhere
	// in server.go. The region runs from the registration marker to the
	// next AddPrompt call (or EOF) — keyword assertions then look at
	// only this slice regardless of how the body is constructed.
	startMarker := `"generate-metrics-registry"`
	start := strings.Index(src, startMarker)
	require.GreaterOrEqual(t, start, 0, "marker %q must be found in server.go", startMarker)
	end := strings.Index(src[start+len(startMarker):], "AddPrompt(")
	var body string
	if end < 0 {
		body = src[start:]
	} else {
		body = src[start : start+len(startMarker)+end]
	}

	musts := []string{
		// The field name itself.
		"aggregation:",
		// At least one reducer keyword.
		"across_groups",
		// The two-stage flow (group_by + within_group).
		"group_by",
		"within_group",
		// The two-stage example — a generic users/tenant deduplication
		// pattern, not a project-specific metric name.
		"online_users_count",
		"tenant_id",
	}
	for _, want := range musts {
		assert.Contains(t, body, want)
	}
}
