package tools

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
)

// testToolServer wraps an mcp.Server for testing tool handlers.
// It connects via InMemoryTransport so tools can be called through
// the full MCP protocol path.
type testToolServer struct {
	server  *mcp.Server
	session *mcp.ClientSession
	t       *testing.T
	cancel  context.CancelFunc
}

func newTestToolServer(t *testing.T) *testToolServer {
	t.Helper()
	s := mcp.NewServer(
		&mcp.Implementation{Name: "test", Version: "0.0.0"},
		nil,
	)
	return &testToolServer{server: s, t: t}
}

// metricsTestDeps builds a Deps suitable for metrics-tool registration: the
// querier/registry/project are set, ProfileCache and Client are nil (unused
// by metrics tools), and Mode defaults to Standard.
func metricsTestDeps(querier gcpdata.MetricsQuerier, registry *metrics.Registry, defaultProject string) Deps {
	return Deps{
		Querier:        querier,
		Registry:       registry,
		DefaultProject: defaultProject,
	}
}

func (ts *testToolServer) registerMetricsSnapshot(querier gcpdata.MetricsQuerier, registry *metrics.Registry, defaultProject string) {
	RegisterMetricsSnapshot(ts.server, metricsTestDeps(querier, registry, defaultProject))
}

func (ts *testToolServer) registerMetricsTop(querier gcpdata.MetricsQuerier, registry *metrics.Registry, defaultProject string) {
	RegisterMetricsTop(ts.server, metricsTestDeps(querier, registry, defaultProject))
}

func (ts *testToolServer) registerMetricsRelated(querier gcpdata.MetricsQuerier, registry *metrics.Registry, defaultProject string) {
	RegisterMetricsRelated(ts.server, metricsTestDeps(querier, registry, defaultProject))
}

func (ts *testToolServer) registerMetricsCompare(querier gcpdata.MetricsQuerier, registry *metrics.Registry, defaultProject string) {
	RegisterMetricsCompare(ts.server, metricsTestDeps(querier, registry, defaultProject))
}

func (ts *testToolServer) registerMetricsList(querier gcpdata.MetricsQuerier, registry *metrics.Registry, defaultProject string) {
	RegisterMetricsList(ts.server, metricsTestDeps(querier, registry, defaultProject))
}

// connect starts the server and connects a client via InMemoryTransport.
// Must be called after all tools are registered. Call close() when done.
func (ts *testToolServer) connect(ctx context.Context) {
	ts.t.Helper()
	ct, st := mcp.NewInMemoryTransports()

	serverCtx, cancel := context.WithCancel(ctx)
	ts.cancel = cancel

	go func() {
		_ = ts.server.Run(serverCtx, st)
	}()

	client := mcp.NewClient(
		&mcp.Implementation{Name: "test-client", Version: "0.0.0"},
		nil,
	)
	session, err := client.Connect(ctx, ct, nil)
	require.NoError(ts.t, err)
	ts.session = session
}

func (ts *testToolServer) close() {
	if ts.cancel != nil {
		ts.cancel()
	}
}

// callTool dispatches a tool call through the connected client session.
func (ts *testToolServer) callTool(ctx context.Context, toolName string, args map[string]any) (*mcp.CallToolResult, error) {
	ts.t.Helper()
	return ts.session.CallTool(ctx, &mcp.CallToolParams{
		Name:      toolName,
		Arguments: args,
	})
}
