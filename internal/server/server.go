package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpclient"
	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
	"github.com/tolmachov/mcp-gcp-observability/internal/tools"
)

// Transport defines the server transport mode.
type Transport string

const (
	// TransportStdio uses standard input/output (default, for Claude Desktop and Claude Code).
	TransportStdio Transport = "stdio"
	// TransportHTTP uses streamable HTTP (for remote deployments).
	TransportHTTP Transport = "http"
)

// Server represents the MCP server for GCP Observability.
type Server struct {
	mcpServer *server.MCPServer
	completer *promptCompleter
	cfg       *gcpclient.Config
	stdin     io.Reader
	stdout    io.Writer
	errOut    io.Writer
}

// New creates a new MCP server.
func New(cfg *gcpclient.Config, version string, stdin io.Reader, stdout, errOut io.Writer) (*Server, error) {
	completer := &promptCompleter{}
	mcpServer := server.NewMCPServer(
		"mcp-gcp-observability",
		version,
		server.WithToolCapabilities(true),
		server.WithRecovery(),
		server.WithLogging(),
		server.WithPromptCompletionProvider(completer),
		server.WithInstructions("Recommended workflow: "+
			"1) logs.services — discover available services. "+
			"2) logs.summary — get severity distribution, top errors, and top services for initial triage. "+
			"3) errors.list — list error groups sorted by count. "+
			"4) logs.query or logs.k8s — investigate specific logs with filters. "+
			"5) logs.by_trace — follow a single request across services using a trace ID from logs.find_requests or logs.query results. "+
			"6) trace.get — get detailed span tree for a trace to understand request timing and dependencies. "+
			"Always prefer logs.k8s over logs.query when investigating Kubernetes workloads. "+
			"For metrics analysis: "+
			"1) metrics.list — discover available metrics. "+
			"2) metrics.snapshot — get semantic snapshot with baseline comparison, trend detection, and classification. "+
			"3) metrics.top_contributors — break down by label dimension to find which values contribute most to an anomaly. "+
			"4) metrics.related — check correlated signals configured in the registry. "+
			"5) metrics.compare — compare two arbitrary time windows (e.g. before/after deploy)."),
	)

	return &Server{
		mcpServer: mcpServer,
		completer: completer,
		cfg:       cfg,
		stdin:     stdin,
		stdout:    stdout,
		errOut:    errOut,
	}, nil
}

// Run starts the MCP server using the specified transport.
func (s *Server) Run(ctx context.Context, transport Transport, httpAddr string) error {
	client, err := gcpclient.New(ctx, s.cfg)
	if err != nil {
		return fmt.Errorf("creating GCP client: %w", err)
	}
	defer func() {
		if closeErr := client.Close(); closeErr != nil {
			log.New(s.errOut, "[mcp-gcp-observability] ", log.LstdFlags).
				Printf("warning: failed to close GCP client: %v", closeErr)
		}
	}()

	var reg *metrics.Registry
	if s.cfg.MetricsRegistryFile != "" {
		var regErr error
		reg, regErr = metrics.LoadRegistry(s.cfg.MetricsRegistryFile)
		if regErr != nil {
			return fmt.Errorf("loading metrics registry: %w", regErr)
		}
	} else {
		reg = metrics.NewRegistry()
	}

	s.completer.registry = reg

	querier := gcpdata.NewMonitoringQuerier(client.MonitoringClient())
	defaultProject := client.Config().DefaultProject

	tools.RegisterTools(s.mcpServer, []tools.Handler{
		// Logs
		tools.NewLogsQueryHandler(client),
		tools.NewLogsByTraceHandler(client),
		tools.NewLogsByRequestIDHandler(client),
		tools.NewLogsFindRequestsHandler(client),
		tools.NewLogsK8sHandler(client),
		tools.NewLogsServicesHandler(client),
		tools.NewLogsSummaryHandler(client),
		// Errors
		tools.NewErrorsListHandler(client),
		tools.NewErrorsGetHandler(client),
		// Traces
		tools.NewTraceGetHandler(client),
		// Metrics
		tools.NewMetricsListHandler(querier, reg, defaultProject),
		tools.NewMetricsSnapshotHandler(querier, reg, defaultProject),
		tools.NewMetricsTopHandler(querier, reg, defaultProject),
		tools.NewMetricsRelatedHandler(querier, reg, defaultProject),
		tools.NewMetricsCompareHandler(querier, reg, defaultProject),
	})

	s.registerResources(client, reg)
	s.registerPrompts()

	errLogger := log.New(s.errOut, "[mcp-gcp-observability] ", log.LstdFlags)

	switch transport {
	case TransportHTTP:
		return s.runHTTP(ctx, httpAddr, errLogger)
	case TransportStdio, "":
		return s.runStdio(ctx, errLogger)
	default:
		return fmt.Errorf("unsupported transport %q: must be %q or %q", transport, TransportStdio, TransportHTTP)
	}
}

func (s *Server) runStdio(ctx context.Context, errLogger *log.Logger) error {
	stdioServer := server.NewStdioServer(s.mcpServer)
	stdioServer.SetErrorLogger(errLogger)
	if err := stdioServer.Listen(ctx, s.stdin, s.stdout); err != nil {
		return fmt.Errorf("stdio server: %w", err)
	}
	return nil
}

func (s *Server) runHTTP(ctx context.Context, addr string, errLogger *log.Logger) error {
	httpServer := server.NewStreamableHTTPServer(s.mcpServer)
	errLogger.Printf("Starting streamable HTTP server on %s", addr)
	srv := &http.Server{
		Addr:    addr,
		Handler: httpServer,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			errLogger.Printf("HTTP server shutdown error: %v", err)
		}
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http server: %w", err)
	}
	return nil
}

// registerResources adds MCP resources to the server.
func (s *Server) registerResources(client *gcpclient.Client, reg *metrics.Registry) {
	cfg := client.Config()
	configJSON, err := json.Marshal(map[string]any{
		"default_project":        cfg.DefaultProject,
		"logs_max_limit":         cfg.LogsMaxLimit,
		"errors_max_limit":       cfg.ErrorsMaxLimit,
		"metrics_registry_file":  cfg.MetricsRegistryFile,
		"metrics_registry_count": reg.Count(),
	})
	if err != nil {
		configJSON = []byte(fmt.Sprintf(`{"error": "failed to marshal config: %v"}`, err))
	}

	s.mcpServer.AddResource(
		mcp.NewResource(
			"config://project",
			"Project Configuration",
			mcp.WithResourceDescription("Current GCP project configuration: default project ID and query limits"),
			mcp.WithMIMEType("application/json"),
		),
		func(_ context.Context, _ mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
			return []mcp.ResourceContents{
				mcp.TextResourceContents{
					URI:      "config://project",
					MIMEType: "application/json",
					Text:     string(configJSON),
				},
			}, nil
		},
	)
}

// registerPrompts adds MCP prompts for common observability workflows.
func (s *Server) registerPrompts() {
	s.mcpServer.AddPrompt(mcp.NewPrompt("investigate-errors",
		mcp.WithPromptDescription("Investigate top errors: list error groups, get details for the worst one, and find related logs"),
		mcp.WithArgument("service",
			mcp.ArgumentDescription("Optional service name to filter errors"),
		),
	), func(_ context.Context, request mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		service := request.Params.Arguments["service"]
		msg := "Investigate the top errors in the project:\n" +
			"1. Use errors.list to find the most frequent error groups"
		if service != "" {
			msg += fmt.Sprintf(" (filter by service: %s)", service)
		}
		msg += "\n2. Use errors.get on the top error group to see stack traces and individual events" +
			"\n3. Use logs.query or logs.k8s to find related logs around the same time" +
			"\n4. If trace IDs are available, use logs.by_trace to follow the request flow" +
			"\n5. Summarize the root cause and suggest next steps"
		return &mcp.GetPromptResult{
			Messages: []mcp.PromptMessage{
				{Role: mcp.RoleUser, Content: mcp.TextContent{Text: msg}},
			},
		}, nil
	})

	s.mcpServer.AddPrompt(mcp.NewPrompt("trace-request",
		mcp.WithPromptDescription("Trace a specific HTTP request end-to-end: find it by URL, follow its trace, and analyze spans"),
		mcp.WithArgument("url_pattern",
			mcp.ArgumentDescription("URL pattern to search for (e.g. '/api/users')"),
			mcp.RequiredArgument(),
		),
	), func(_ context.Context, request mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		urlPattern := request.Params.Arguments["url_pattern"]
		msg := fmt.Sprintf("Trace a request matching URL pattern %q:\n", urlPattern) +
			"1. Use logs.find_requests to find matching HTTP requests with their trace IDs\n" +
			"2. Pick the most interesting request (e.g. slowest or with an error status)\n" +
			"3. Use trace.get to see the full span tree and identify slow spans\n" +
			"4. Use logs.by_trace to see all logs associated with that trace\n" +
			"5. Summarize the request flow, highlighting any issues or bottlenecks"
		return &mcp.GetPromptResult{
			Messages: []mcp.PromptMessage{
				{Role: mcp.RoleUser, Content: mcp.TextContent{Text: msg}},
			},
		}, nil
	})

	s.mcpServer.AddPrompt(mcp.NewPrompt("investigate-metrics",
		mcp.WithPromptDescription("Investigate a metric anomaly: discover metrics, get snapshot, drill down by dimension, check related signals"),
		mcp.WithArgument("metric_type",
			mcp.ArgumentDescription("Metric type to investigate (e.g. 'compute.googleapis.com/instance/cpu/utilization')"),
		),
		mcp.WithArgument("service",
			mcp.ArgumentDescription("Optional service or resource filter"),
		),
	), func(_ context.Context, request mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		metricType := request.Params.Arguments["metric_type"]
		service := request.Params.Arguments["service"]
		msg := "Investigate a metric anomaly:\n"
		if metricType == "" {
			msg += "1. Use metrics.list to discover available metrics"
			if service != "" {
				msg += fmt.Sprintf(" (filter by '%s')", service)
			}
			msg += "\n2. Pick the most relevant metric\n"
		} else {
			msg += fmt.Sprintf("1. The metric to investigate is: %s\n", metricType)
		}
		msg += "3. Use metrics.snapshot to get a semantic snapshot with baseline comparison\n" +
			"4. If the classification shows a regression, use metrics.top_contributors to find which dimension contributes most\n" +
			"5. Use metrics.related to check correlated signals\n" +
			"6. Summarize the findings: what changed, when, likely cause, and recommended action"
		return &mcp.GetPromptResult{
			Messages: []mcp.PromptMessage{
				{Role: mcp.RoleUser, Content: mcp.TextContent{Text: msg}},
			},
		}, nil
	})

	s.mcpServer.AddPrompt(mcp.NewPrompt("service-health",
		mcp.WithPromptDescription("Check the health of services: discover services, summarize logs, and identify issues"),
	), func(_ context.Context, _ mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		msg := "Check the health of services in the project:\n" +
			"1. Use logs.services to discover all available services\n" +
			"2. Use logs.summary to get an overview of severity distribution and top errors\n" +
			"3. Use errors.list to see the most frequent error groups\n" +
			"4. For any concerning services, use logs.k8s or logs.query to investigate further\n" +
			"5. Provide a health summary with any issues found and recommended actions"
		return &mcp.GetPromptResult{
			Messages: []mcp.PromptMessage{
				{Role: mcp.RoleUser, Content: mcp.TextContent{Text: msg}},
			},
		}, nil
	})
}

// promptCompleter provides autocomplete for prompt arguments.
// It uses the metrics registry when available, falling back to common GCP metrics.
type promptCompleter struct {
	registry *metrics.Registry
}

// defaultMetricCandidates are common GCP metric types shown when the registry is empty.
var defaultMetricCandidates = []string{
	"compute.googleapis.com/instance/cpu/utilization",
	"compute.googleapis.com/instance/disk/read_bytes_count",
	"compute.googleapis.com/instance/network/received_bytes_count",
	"loadbalancing.googleapis.com/https/request_count",
	"loadbalancing.googleapis.com/https/total_latencies",
	"run.googleapis.com/request_count",
	"run.googleapis.com/request_latencies",
	"cloudsql.googleapis.com/database/cpu/utilization",
	"cloudsql.googleapis.com/database/memory/utilization",
	"storage.googleapis.com/api/request_count",
	"pubsub.googleapis.com/topic/send_request_count",
	"appengine.googleapis.com/http/server/response_latencies",
}

func (p *promptCompleter) CompletePromptArgument(_ context.Context, promptName string, argument mcp.CompleteArgument, _ mcp.CompleteContext) (*mcp.Completion, error) {
	prefix := strings.ToLower(argument.Value)
	var values []string

	switch {
	case promptName == "investigate-metrics" && argument.Name == "metric_type":
		candidates := p.metricCandidates()
		for _, c := range candidates {
			if prefix == "" || strings.Contains(strings.ToLower(c), prefix) {
				values = append(values, c)
			}
		}
	}

	return &mcp.Completion{
		Values:  values,
		HasMore: len(values) > 100,
	}, nil
}

func (p *promptCompleter) metricCandidates() []string {
	if p.registry != nil && p.registry.Count() > 0 {
		entries := p.registry.List("", metrics.MetricKind(""))
		candidates := make([]string, 0, len(entries))
		for _, e := range entries {
			candidates = append(candidates, e.MetricType)
		}
		return candidates
	}
	return defaultMetricCandidates
}
