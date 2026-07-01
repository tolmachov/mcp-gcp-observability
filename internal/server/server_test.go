package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpclient"
	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
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

func TestPromptCompleter_ProfileType(t *testing.T) {
	c := &promptCompleter{}
	result, err := c.Handle(context.Background(), &mcp.CompleteRequest{
		Params: &mcp.CompleteParams{
			Ref:      &mcp.CompleteReference{Type: "ref/prompt", Name: "investigate-profile"},
			Argument: mcp.CompleteParamsArgument{Name: "profile_type", Value: "he"},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"HEAP", "HEAP_ALLOC", "PEAK_HEAP"}, result.Completion.Values)
}

// TestPromptCompleter_ProfileTypeWrongPrompt guards that profile_type is only
// completed for the prompt that declares it.
func TestPromptCompleter_ProfileTypeWrongPrompt(t *testing.T) {
	c := &promptCompleter{}
	result, err := c.Handle(context.Background(), &mcp.CompleteRequest{
		Params: &mcp.CompleteParams{
			Ref:      &mcp.CompleteReference{Type: "ref/prompt", Name: "investigate-metrics"},
			Argument: mcp.CompleteParamsArgument{Name: "profile_type", Value: ""},
		},
	})
	require.NoError(t, err)
	assert.Empty(t, result.Completion.Values)
}

func TestPromptCompleter_Service(t *testing.T) {
	calls := 0
	c := &promptCompleter{
		loadServices: func(context.Context) []string {
			calls++
			return []string{"checkout", "checkout-worker", "payments"}
		},
	}
	for _, prompt := range []string{"investigate-errors", "investigate-metrics", "investigate-profile"} {
		result, err := c.Handle(context.Background(), &mcp.CompleteRequest{
			Params: &mcp.CompleteParams{
				Ref:      &mcp.CompleteReference{Type: "ref/prompt", Name: prompt},
				Argument: mcp.CompleteParamsArgument{Name: "service", Value: "checkout"},
			},
		})
		require.NoError(t, err)
		assert.Equal(t, []string{"checkout", "checkout-worker"}, result.Completion.Values, "prompt %s", prompt)
	}
	assert.Equal(t, 3, calls, "loadServices should be consulted for each service-bearing prompt")
}

// TestPromptCompleter_ServiceNoLoader verifies completion degrades gracefully
// when no GCP client has been wired in (loadServices is nil).
func TestPromptCompleter_ServiceNoLoader(t *testing.T) {
	c := &promptCompleter{}
	result, err := c.Handle(context.Background(), &mcp.CompleteRequest{
		Params: &mcp.CompleteParams{
			Ref:      &mcp.CompleteReference{Type: "ref/prompt", Name: "investigate-errors"},
			Argument: mcp.CompleteParamsArgument{Name: "service", Value: ""},
		},
	})
	require.NoError(t, err)
	assert.Empty(t, result.Completion.Values)
}

func TestPromptCompleter_ResourceProject(t *testing.T) {
	c := &promptCompleter{defaultProject: "my-project"}
	result, err := c.Handle(context.Background(), &mcp.CompleteRequest{
		Params: &mcp.CompleteParams{
			Ref:      &mcp.CompleteReference{Type: "ref/resource", Name: "gcp-logs://{project}/recent"},
			Argument: mcp.CompleteParamsArgument{Name: "project", Value: "my"},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"my-project"}, result.Completion.Values)
}

// TestPromptCompleter_TruncatesAt100 verifies the result is capped at 100 values
// with HasMore set when more candidates match.
func TestPromptCompleter_TruncatesAt100(t *testing.T) {
	services := make([]string, 250)
	for i := range services {
		services[i] = fmt.Sprintf("svc-%03d", i)
	}
	c := &promptCompleter{loadServices: func(context.Context) []string { return services }}
	result, err := c.Handle(context.Background(), &mcp.CompleteRequest{
		Params: &mcp.CompleteParams{
			Ref:      &mcp.CompleteReference{Type: "ref/prompt", Name: "investigate-errors"},
			Argument: mcp.CompleteParamsArgument{Name: "service", Value: "svc"},
		},
	})
	require.NoError(t, err)
	assert.Len(t, result.Completion.Values, maxCompletionValues)
	assert.True(t, result.Completion.HasMore)
}

// TestNewCachedServiceLister verifies the lister caches the first fetch (success
// or failure) for the session and never calls through twice.
func TestNewCachedServiceLister(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("caches successful fetch", func(t *testing.T) {
		calls := 0
		load := newCachedServiceLister(func(context.Context) (*gcpdata.ServiceList, error) {
			calls++
			return &gcpdata.ServiceList{Services: []gcpdata.ServiceInfo{{Name: "a"}, {Name: ""}, {Name: "b"}}}, nil
		}, logger)
		got := load(context.Background())
		assert.Equal(t, []string{"a", "b"}, got)
		_ = load(context.Background())
		assert.Equal(t, 1, calls, "fetch must run at most once")
	})

	t.Run("caches failure as empty", func(t *testing.T) {
		calls := 0
		load := newCachedServiceLister(func(context.Context) (*gcpdata.ServiceList, error) {
			calls++
			return nil, fmt.Errorf("boom")
		}, logger)
		assert.Empty(t, load(context.Background()))
		assert.Empty(t, load(context.Background()))
		assert.Equal(t, 1, calls, "fetch must not retry on every call")
	})
}

func TestProjectFromURI(t *testing.T) {
	tests := []struct {
		uri        string
		defProject string
		want       string
	}{
		{"gcp-logs://my-project/recent", "fallback", "my-project"},
		{"gcp-errors://proj-123/groups", "fallback", "proj-123"},
		{"gcp-traces://p/recent", "fallback", "p"},
		{"gcp-logs:///recent", "fallback", "fallback"},    // empty host → default
		{"::not a uri::", "fallback", "fallback"},         // parse error → default
		{"gcp-logs://only-host", "fallback", "only-host"}, // no path still yields host
		{"gcp-logs:///recent", "", ""},                    // empty host, no default
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, projectFromURI(tc.uri, tc.defProject), "uri=%s", tc.uri)
	}
}

// TestResourceTemplateURIsValid guards that the navigable resource-template URIs
// are valid RFC 6570 templates: AddResourceTemplate panics on an invalid or
// non-absolute template, so this would crash a real server at startup otherwise.
func TestResourceTemplateURIsValid(t *testing.T) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	uris := []string{
		"gcp-logs://{project}/recent",
		"gcp-errors://{project}/groups",
		"gcp-traces://{project}/recent",
	}
	require.NotPanics(t, func() {
		for _, uri := range uris {
			srv.AddResourceTemplate(
				&mcp.ResourceTemplate{URITemplate: uri, Name: uri, MIMEType: "application/json"},
				func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) { return nil, nil },
			)
		}
	})
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
	_, err := s.buildSingleVariantServer("bogus", nil, nil, tools.Deps{})
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

// TestRegisterAllToolsCount pins allToolsCount against the tools that
// registerAllTools actually registers. Every variant Description string
// interpolates allToolsCount, so this test is the choke point that keeps
// the constant honest.
func TestRegisterAllToolsCount(t *testing.T) {
	s := testServer(t)
	srv := s.newMCPInstance()
	registerAllTools(srv, tools.Deps{
		Logs:           stubBackends{},
		Errors:         stubBackends{},
		Traces:         stubBackends{},
		Profiler:       stubBackends{},
		Registry:       metrics.NewRegistry(),
		DefaultProject: "test",
		Mode:           tools.ModeStandard,
	})

	tls := listToolsViaInMemory(t, srv)
	assert.Len(t, tls, allToolsCount,
		"registerAllTools registered %d tools; update allToolsCount if the change is intentional", len(tls))
}

// TestVariantDescriptionsInterpolateCounts guards the buildVariantSpecs
// fmt.Sprintf calls — a broken format string (missing %d, literal number
// left behind, %%d escaping regression) would silently ship a malformed
// description to clients.
func TestVariantDescriptionsInterpolateCounts(t *testing.T) {
	allCountStr := fmt.Sprintf("(%d)", allToolsCount)
	coreCountStr := fmt.Sprintf("(%d)", tools.CoreToolsCount)

	full, ok := findVariantSpec(string(VariantFull))
	require.True(t, ok)
	assert.Contains(t, full.description, allCountStr,
		"full variant description must interpolate allToolsCount")

	compact, ok := findVariantSpec(string(VariantCompact))
	require.True(t, ok)
	assert.Contains(t, compact.description, allCountStr,
		"compact variant description must interpolate allToolsCount")

	monitoring, ok := findVariantSpec(string(VariantMonitoring))
	require.True(t, ok)
	assert.Contains(t, monitoring.description, coreCountStr,
		"monitoring variant description must interpolate tools.CoreToolsCount")
}

// TestBuildVariantsServerHappyPath verifies the core feature of the variants
// PR: buildVariantsServer must construct a non-nil *variants.Server when given
// valid dependencies. Without this, the only buildVariantsServer coverage was
// the unknown-variant error path of buildSingleVariantServer.
func TestBuildVariantsServerHappyPath(t *testing.T) {
	s := testServer(t)
	client := gcpclient.NewForTesting(gcpclient.Config{DefaultProject: "test"})
	deps := tools.Deps{
		Logs:           stubBackends{},
		Errors:         stubBackends{},
		Traces:         stubBackends{},
		Profiler:       stubBackends{},
		Registry:       metrics.NewRegistry(),
		DefaultProject: "test",
	}
	vs, err := s.buildVariantsServer(client, metrics.NewRegistry(), deps)
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
		Logs:           stubBackends{},
		Errors:         stubBackends{},
		Traces:         stubBackends{},
		Profiler:       stubBackends{},
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
