package server

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpclient"
	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
	"github.com/tolmachov/mcp-gcp-observability/internal/tools"
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

// testServer constructs a minimal Server suitable for variant-build tests.
// It has no real GCP plumbing; tools registered on it can be listed but not
// invoked.
func testServer(_ *testing.T) *Server {
	return &Server{
		completer: &promptCompleter{},
		version:   "test",
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// listToolsViaInMemory connects an in-memory MCP client to srv and returns
// the tools the server advertises. Caller is responsible for the lifetime
// of srv.
func listToolsViaInMemory(t *testing.T, srv *mcp.Server) []*mcp.Tool {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ct, st := mcp.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, st) }()

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0.0.0"}, nil)
	session, err := client.Connect(ctx, ct, nil)
	require.NoError(t, err)

	result, err := session.ListTools(ctx, nil)
	require.NoError(t, err)
	return result.Tools
}

// TestRegisterAllToolsCount pins the "22 tools" claim that appears in the
// registerAllTools doc, the variant Description strings, and the
// buildVariantsServer doc. Anyone adding or removing a Register* call inside
// registerAllTools without updating this test will trip immediately.
func TestRegisterAllToolsCount(t *testing.T) {
	s := testServer(t)
	srv := s.newMCPInstance()
	registerAllTools(srv, tools.Deps{
		Client:         &gcpclient.Client{},
		Registry:       metrics.NewRegistry(),
		DefaultProject: "test",
		Mode:           tools.ModeStandard,
	})

	tls := listToolsViaInMemory(t, srv)
	assert.Len(t, tls, 22, "registerAllTools must register exactly 22 tools")
}

// TestBuildVariantsServerHappyPath verifies the core feature of the variants
// PR: buildVariantsServer must construct a non-nil *variants.Server when given
// valid dependencies. Without this, the only buildVariantsServer coverage was
// the unknown-variant error path of buildSingleVariantServer.
func TestBuildVariantsServerHappyPath(t *testing.T) {
	s := testServer(t)
	client := gcpclient.NewForTesting(gcpclient.Config{DefaultProject: "test"})
	vs, err := s.buildVariantsServer(client, nil, metrics.NewRegistry(), "test", nil)
	require.NoError(t, err)
	require.NotNil(t, vs)
	t.Cleanup(func() {
		if closeErr := vs.Close(); closeErr != nil {
			t.Logf("vs.Close: %v", closeErr)
		}
	})
}

// TestCompactModeRealDescriptionsSane checks every tool's compact description
// is non-empty, ends with a period, and does not end with a known abbreviation.
// Trailing "e.g." / "i.e." / "etc." indicates compactDesc cut mid-sentence
// (the documented foot-gun in TestCompactDesc) — current tool descriptions are
// safe but a future reword that moves an abbreviation into sentence one would
// silently ship a mangled description without this guard.
func TestCompactModeRealDescriptionsSane(t *testing.T) {
	s := testServer(t)
	srv := s.newMCPInstance()
	registerAllTools(srv, tools.Deps{
		Client:         &gcpclient.Client{},
		Registry:       metrics.NewRegistry(),
		DefaultProject: "test",
		Mode:           tools.ModeCompact,
	})

	tls := listToolsViaInMemory(t, srv)
	require.NotEmpty(t, tls)

	abbreviations := []string{"e.g.", "i.e.", "etc.", "vs.", "Mr.", "Dr.", "Mrs.", "Ms.", "Jr.", "Sr."}
	for _, tool := range tls {
		t.Run(tool.Name, func(t *testing.T) {
			assert.NotEmpty(t, tool.Description, "compact description must not be empty")
			assert.True(t, strings.HasSuffix(tool.Description, "."),
				"compact description must end with a period (got: %q)", tool.Description)
			for _, abbr := range abbreviations {
				assert.False(t, strings.HasSuffix(tool.Description, abbr),
					"compact description ends with abbreviation %q (compactDesc cut mid-sentence): %q",
					abbr, tool.Description)
			}
		})
	}
}
