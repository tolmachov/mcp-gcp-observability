package server

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
)

func TestPromptCompleter_EmptyPrefix(t *testing.T) {
	c := &promptCompleter{}
	result, err := c.Handle(context.Background(), &mcp.CompleteRequest{
		Params: &mcp.CompleteParams{
			Ref:      &mcp.CompleteReference{Type: "ref/prompt", Name: "investigate-metrics"},
			Argument: mcp.CompleteParamsArgument{Name: "metric_type", Value: ""},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, len(defaultMetricCandidates), len(result.Completion.Values))
}

func TestPromptCompleter_FilterByPrefix(t *testing.T) {
	c := &promptCompleter{}
	result, err := c.Handle(context.Background(), &mcp.CompleteRequest{
		Params: &mcp.CompleteParams{
			Ref:      &mcp.CompleteReference{Type: "ref/prompt", Name: "investigate-metrics"},
			Argument: mcp.CompleteParamsArgument{Name: "metric_type", Value: "compute"},
		},
	})
	require.NoError(t, err)
	for _, v := range result.Completion.Values {
		assert.True(t, len(v) >= 7 && v[:7] == "compute")
	}
	assert.NotEmpty(t, result.Completion.Values)
}

func TestPromptCompleter_CaseInsensitive(t *testing.T) {
	c := &promptCompleter{}
	result, err := c.Handle(context.Background(), &mcp.CompleteRequest{
		Params: &mcp.CompleteParams{
			Ref:      &mcp.CompleteReference{Type: "ref/prompt", Name: "investigate-metrics"},
			Argument: mcp.CompleteParamsArgument{Name: "metric_type", Value: "CPU"},
		},
	})
	require.NoError(t, err)
	assert.NotEmpty(t, result.Completion.Values)
}

func TestPromptCompleter_UnknownPrompt(t *testing.T) {
	c := &promptCompleter{}
	result, err := c.Handle(context.Background(), &mcp.CompleteRequest{
		Params: &mcp.CompleteParams{
			Ref:      &mcp.CompleteReference{Type: "ref/prompt", Name: "unknown-prompt"},
			Argument: mcp.CompleteParamsArgument{Name: "metric_type", Value: "cpu"},
		},
	})
	require.NoError(t, err)
	assert.Empty(t, result.Completion.Values)
}

func TestPromptCompleter_UnknownArgument(t *testing.T) {
	c := &promptCompleter{}
	result, err := c.Handle(context.Background(), &mcp.CompleteRequest{
		Params: &mcp.CompleteParams{
			Ref:      &mcp.CompleteReference{Type: "ref/prompt", Name: "investigate-metrics"},
			Argument: mcp.CompleteParamsArgument{Name: "unknown_arg", Value: "cpu"},
		},
	})
	require.NoError(t, err)
	assert.Empty(t, result.Completion.Values)
}

func TestPromptCompleter_UsesRegistry(t *testing.T) {
	reg := metrics.NewRegistry()
	c := &promptCompleter{registry: reg}
	result, err := c.Handle(context.Background(), &mcp.CompleteRequest{
		Params: &mcp.CompleteParams{
			Ref:      &mcp.CompleteReference{Type: "ref/prompt", Name: "investigate-metrics"},
			Argument: mcp.CompleteParamsArgument{Name: "metric_type", Value: ""},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, len(defaultMetricCandidates), len(result.Completion.Values))
}

// TestPromptCompleter_NonEmptyRegistry verifies that when the registry has
// entries, completions come from the registry rather than from
// defaultMetricCandidates. Regression guard: a refactor that always returned
// defaults would pass the empty-registry test above but fail here.
func TestPromptCompleter_NonEmptyRegistry(t *testing.T) {
	const metricType = "custom.googleapis.com/my_metric"
	reg := metrics.NewRegistryFromMetaMap(map[string]metrics.MetricMeta{
		metricType: {Kind: metrics.KindThroughput, BetterDirection: metrics.DirectionNone},
	})
	c := &promptCompleter{registry: reg}
	result, err := c.Handle(context.Background(), &mcp.CompleteRequest{
		Params: &mcp.CompleteParams{
			Ref:      &mcp.CompleteReference{Type: "ref/prompt", Name: "investigate-metrics"},
			Argument: mcp.CompleteParamsArgument{Name: "metric_type", Value: ""},
		},
	})
	require.NoError(t, err)
	require.Len(t, result.Completion.Values, 1)
	assert.Equal(t, metricType, result.Completion.Values[0])
}

// TestBuildSingleVariantServerUnknownVariant verifies that buildSingleVariantServer
// returns a descriptive error for unknown variant IDs without panicking.
// This guards the case where Run()'s upfront validation is bypassed (e.g. in tests).
func TestBuildSingleVariantServerUnknownVariant(t *testing.T) {
	s := &Server{
		completer: &promptCompleter{},
		version:   "test",
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	_, err := s.buildSingleVariantServer("bogus", nil, nil, nil, "", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bogus")
	assert.Contains(t, err.Error(), "must be one of")
}

// TestValidVariantIDsCoversSwitch ensures ValidVariantIDs lists every ID handled
// by buildSingleVariantServer's switch, so the two cannot silently drift.
func TestValidVariantIDsCoversSwitch(t *testing.T) {
	// The switch in buildSingleVariantServer covers "full", "compact", "monitoring".
	// Any ID not in that set must be in the default branch and return an error.
	switchCases := []string{"full", "compact", "monitoring"}
	assert.ElementsMatch(t, switchCases, ValidVariantIDs,
		"ValidVariantIDs must match the cases in buildSingleVariantServer switch")
}
