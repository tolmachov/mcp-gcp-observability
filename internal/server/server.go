package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

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

// Server is the MCP server for GCP Observability.
type Server struct {
	mcpServer *mcp.Server
	completer *promptCompleter
	cfg       *gcpclient.Config
	logger    *slog.Logger
	stdin     io.Reader
	stdout    io.Writer
	errOut    io.Writer
}

// New creates a new MCP server.
func New(cfg *gcpclient.Config, version string, stdin io.Reader, stdout, errOut io.Writer) (*Server, error) {
	completer := &promptCompleter{}

	logger := slog.New(slog.NewTextHandler(errOut, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	mcpServer := mcp.NewServer(
		&mcp.Implementation{
			Name:    "mcp-gcp-observability",
			Version: version,
		},
		&mcp.ServerOptions{
			Instructions: "Recommended workflow: " +
				"1) logs_services — discover available services. " +
				"2) logs_summary — get severity distribution, top errors, and top services for initial triage. " +
				"3) errors_list — list error groups sorted by count. " +
				"4) logs_query or logs_k8s — investigate specific logs with filters. " +
				"5) logs_by_trace — follow a single request across services using a trace ID from logs_find_requests or logs_query results. " +
				"6) trace_list — search for traces by span name, latency, or time range without knowing trace IDs. " +
				"7) trace_get — get detailed span tree for a trace to understand request timing and dependencies. " +
				"Always prefer logs_k8s over logs_query when investigating Kubernetes workloads. " +
				"For metrics analysis: " +
				"1) metrics_list — discover available metrics. " +
				"2) metrics_snapshot — get semantic snapshot with baseline comparison, trend detection, and classification. " +
				"3) metrics_top_contributors — break down by label dimension to find which values contribute most to an anomaly. " +
				"4) metrics_related — check correlated signals configured in the registry. " +
				"5) metrics_compare — compare two arbitrary time windows (e.g. before/after deploy). " +
				"For profiling analysis: " +
				"1) profiler_list — discover available profiles by service and type. " +
				"2) profiler_top — see top functions by resource consumption. " +
				"3) profiler_peek — understand a hotspot's callers and callees. " +
				"4) profiler_flamegraph — view bounded subtree of the call graph. " +
				"5) profiler_compare — compare two profiles to find regressions (use diff_id with top/peek/flamegraph). " +
				"6) profiler_trends — track how function costs change over time across multiple profiles. " +
				"Use profiler_compare for point-in-time A/B comparison; use profiler_trends for historical cost evolution.",
			Logger:            logger,
			CompletionHandler: completer.Handle,
		},
	)

	// Recover from panics in tool handlers so that a single bad request does
	// not crash the server process. Panics inside goroutines spawned by tools
	// have their own recovery defers; this middleware covers the main handler
	// goroutine that the SDK runs for each incoming request.
	panicRecovery := func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (result mcp.Result, err error) {
			defer func() {
				if r := recover(); r != nil {
					stack := debug.Stack()
					logger.Error("panic in handler", "method", method, "panic", r, "stack", string(stack))
					err = fmt.Errorf("internal server error: panic in handler for %s: %v", method, r)
				}
			}()
			return next(ctx, method, req)
		}
	}
	mcpServer.AddReceivingMiddleware(panicRecovery)

	tools.SetNotifyLogger(logger)

	return &Server{
		mcpServer: mcpServer,
		completer: completer,
		cfg:       cfg,
		logger:    logger,
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
			s.logger.Warn("failed to close GCP client", "err", closeErr)
		}
	}()

	// LoadRegistry merges user overlay (if any) with embedded GCP defaults.
	registryPath := s.cfg.MetricsRegistryFile
	if registryPath == "" {
		cwd, cwdErr := os.Getwd()
		if cwdErr != nil {
			s.logger.Warn("could not determine working directory for registry auto-probe", "err", cwdErr)
		} else {
			candidate := filepath.Join(cwd, ".mcp", "metrics_registry.yaml")
			_, statErr := os.Stat(candidate)
			if statErr == nil {
				registryPath = candidate
				s.logger.Info("auto-loaded metrics registry overlay", "path", candidate)
			} else if !errors.Is(statErr, fs.ErrNotExist) {
				s.logger.Warn("could not stat registry candidate, skipping auto-probe", "path", candidate, "err", statErr)
			}
		}
	}
	reg, regErr := metrics.LoadRegistry(registryPath)
	if regErr != nil {
		return fmt.Errorf("loading metrics registry: %w", regErr)
	}

	s.completer.registry = reg

	querier := gcpdata.NewMonitoringQuerier(client.MonitoringClient())
	defaultProject := client.Config().DefaultProject

	// Register tools
	tools.RegisterLogsQuery(s.mcpServer, client)
	tools.RegisterLogsByTrace(s.mcpServer, client)
	tools.RegisterLogsByRequestID(s.mcpServer, client)
	tools.RegisterLogsFindRequests(s.mcpServer, client)
	tools.RegisterLogsK8s(s.mcpServer, client)
	tools.RegisterLogsServices(s.mcpServer, client)
	tools.RegisterLogsSummary(s.mcpServer, client)
	// Errors
	tools.RegisterErrorsList(s.mcpServer, client)
	tools.RegisterErrorsGet(s.mcpServer, client)
	// Traces
	tools.RegisterTraceGet(s.mcpServer, client)
	tools.RegisterTraceList(s.mcpServer, client)
	// Metrics
	tools.RegisterMetricsList(s.mcpServer, querier, reg, defaultProject)
	tools.RegisterMetricsSnapshot(s.mcpServer, querier, reg, defaultProject)
	tools.RegisterMetricsTop(s.mcpServer, querier, reg, defaultProject)
	tools.RegisterMetricsRelated(s.mcpServer, querier, reg, defaultProject)
	tools.RegisterMetricsCompare(s.mcpServer, querier, reg, defaultProject)
	// Profiler
	profileCache := gcpdata.NewProfileCache(10)
	tools.RegisterProfilerList(s.mcpServer, client)
	tools.RegisterProfilerTop(s.mcpServer, client, profileCache)
	tools.RegisterProfilerPeek(s.mcpServer, client, profileCache)
	tools.RegisterProfilerFlamegraph(s.mcpServer, client, profileCache)
	tools.RegisterProfilerCompare(s.mcpServer, client, profileCache)
	tools.RegisterProfilerTrends(s.mcpServer, client, profileCache)

	if err := s.registerResources(client, reg); err != nil {
		return err
	}
	s.registerPrompts()

	switch transport {
	case TransportHTTP:
		return s.runHTTP(ctx, httpAddr)
	case TransportStdio, "":
		return s.runStdio(ctx)
	default:
		return fmt.Errorf("unsupported transport %q: must be %q or %q", transport, TransportStdio, TransportHTTP)
	}
}

// nopWriteCloser wraps an io.Writer with a no-op Close method.
type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

func (s *Server) runStdio(ctx context.Context) error {
	s.logger.Info("Starting stdio server")
	if err := s.mcpServer.Run(ctx, &mcp.IOTransport{
		Reader: io.NopCloser(s.stdin),
		Writer: nopWriteCloser{s.stdout},
	}); err != nil {
		return fmt.Errorf("stdio server: %w", err)
	}
	return nil
}

func (s *Server) runHTTP(ctx context.Context, addr string) error {
	handler := mcp.NewStreamableHTTPHandler(
		func(_ *http.Request) *mcp.Server { return s.mcpServer },
		nil,
	)
	s.logger.Info("Starting streamable HTTP server", "addr", addr)
	srv := &http.Server{
		Addr:    addr,
		Handler: handler,
	}
	shutdownDone := make(chan error, 1)
	serverExited := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			shutdownDone <- srv.Shutdown(shutdownCtx)
		case <-serverExited:
			// Server exited early before ctx.Done(); send nil to unblock receiver
			shutdownDone <- nil
		}
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		close(serverExited) // Signal goroutine to exit early
		return fmt.Errorf("http server: %w", err)
	}
	close(serverExited)
	// Wait for shutdown to complete and check for errors
	if shutdownErr := <-shutdownDone; shutdownErr != nil {
		s.logger.Error("HTTP server shutdown failed", "err", shutdownErr)
		return fmt.Errorf("http server shutdown: %w", shutdownErr)
	}
	return nil
}

// registerResources adds MCP resources to the server.
func (s *Server) registerResources(client *gcpclient.Client, reg *metrics.Registry) error {
	cfg := client.Config()
	configJSON, err := json.Marshal(map[string]any{
		"default_project":        cfg.DefaultProject,
		"logs_max_limit":         cfg.LogsMaxLimit,
		"errors_max_limit":       cfg.ErrorsMaxLimit,
		"metrics_registry_file":  cfg.MetricsRegistryFile,
		"metrics_registry_count": reg.Count(),
	})
	if err != nil {
		return fmt.Errorf("failed to marshal config resource during startup: %w", err)
	}

	s.mcpServer.AddResource(
		&mcp.Resource{
			URI:         "config://project",
			Name:        "Project Configuration",
			Description: "Current GCP project configuration: default project ID and query limits",
			MIMEType:    "application/json",
		},
		func(_ context.Context, _ *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{
				Contents: []*mcp.ResourceContents{{
					URI:      "config://project",
					MIMEType: "application/json",
					Text:     string(configJSON),
				}},
			}, nil
		},
	)

	tools.RegisterMetricsChartStaticResource(s.mcpServer)
	return nil
}

// registerPrompts adds MCP prompts for common observability workflows.
func (s *Server) registerPrompts() {
	s.mcpServer.AddPrompt(&mcp.Prompt{
		Name:        "investigate-errors",
		Description: "Investigate top errors: list error groups, get details for the worst one, and find related logs",
		Arguments: []*mcp.PromptArgument{
			{Name: "service", Description: "Optional service name to filter errors"},
		},
	}, func(_ context.Context, request *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		service := request.Params.Arguments["service"]
		msg := "Investigate the top errors in the project:\n" +
			"1. Use errors_list to find the most frequent error groups"
		if service != "" {
			msg += fmt.Sprintf(" (filter by service: %s)", service)
		}
		msg += "\n2. Use errors_get on the top error group to see stack traces and individual events" +
			"\n3. Use logs_query or logs_k8s to find related logs around the same time" +
			"\n4. If trace IDs are available, use logs_by_trace to follow the request flow" +
			"\n5. Summarize the root cause and suggest next steps"
		return &mcp.GetPromptResult{
			Messages: []*mcp.PromptMessage{
				{Role: "user", Content: &mcp.TextContent{Text: msg}},
			},
		}, nil
	})

	s.mcpServer.AddPrompt(&mcp.Prompt{
		Name:        "trace-request",
		Description: "Trace a specific HTTP request end-to-end: find it by URL, follow its trace, and analyze spans",
		Arguments: []*mcp.PromptArgument{
			{Name: "url_pattern", Description: "URL pattern to search for (e.g. '/api/users')", Required: true},
		},
	}, func(_ context.Context, request *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		urlPattern := request.Params.Arguments["url_pattern"]
		msg := fmt.Sprintf("Trace a request matching URL pattern %q:\n", urlPattern) +
			"1. Use logs_find_requests to find matching HTTP requests with their trace IDs\n" +
			"2. Pick the most interesting request (e.g. slowest or with an error status)\n" +
			"3. Use trace_get to see the full span tree and identify slow spans\n" +
			"4. Use logs_by_trace to see all logs associated with that trace\n" +
			"5. Summarize the request flow, highlighting any issues or bottlenecks"
		return &mcp.GetPromptResult{
			Messages: []*mcp.PromptMessage{
				{Role: "user", Content: &mcp.TextContent{Text: msg}},
			},
		}, nil
	})

	s.mcpServer.AddPrompt(&mcp.Prompt{
		Name:        "investigate-metrics",
		Description: "Investigate a metric anomaly: discover metrics, get snapshot, drill down by dimension, check related signals",
		Arguments: []*mcp.PromptArgument{
			{Name: "metric_type", Description: "Metric type to investigate (e.g. 'compute.googleapis.com/instance/cpu/utilization')"},
			{Name: "service", Description: "Optional service or resource filter"},
		},
	}, func(_ context.Context, request *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		metricType := request.Params.Arguments["metric_type"]
		service := request.Params.Arguments["service"]
		msg := "Investigate a metric anomaly:\n"
		if metricType == "" {
			msg += "1. Use metrics_list to discover available metrics"
			if service != "" {
				msg += fmt.Sprintf(" (filter by '%s')", service)
			}
			msg += "\n2. Pick the most relevant metric\n"
		} else {
			msg += fmt.Sprintf("1. The metric to investigate is: %s\n", metricType)
		}
		msg += "3. Use metrics_snapshot to get a semantic snapshot with baseline comparison\n" +
			"4. If the classification shows a regression, use metrics_top_contributors to find which dimension contributes most\n" +
			"5. Use metrics_related to check correlated signals\n" +
			"6. Summarize the findings: what changed, when, likely cause, and recommended action"
		return &mcp.GetPromptResult{
			Messages: []*mcp.PromptMessage{
				{Role: "user", Content: &mcp.TextContent{Text: msg}},
			},
		}, nil
	})

	s.mcpServer.AddPrompt(&mcp.Prompt{
		Name:        "service-health",
		Description: "Check the health of services: discover services, summarize logs, and identify issues",
	}, func(_ context.Context, _ *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		msg := "Check the health of services in the project:\n" +
			"1. Use logs_services to discover all available services\n" +
			"2. Use logs_summary to get an overview of severity distribution and top errors\n" +
			"3. Use errors_list to see the most frequent error groups\n" +
			"4. For any concerning services, use logs_k8s or logs_query to investigate further\n" +
			"5. Provide a health summary with any issues found and recommended actions"
		return &mcp.GetPromptResult{
			Messages: []*mcp.PromptMessage{
				{Role: "user", Content: &mcp.TextContent{Text: msg}},
			},
		}, nil
	})

	s.mcpServer.AddPrompt(&mcp.Prompt{
		Name:        "investigate-profile",
		Description: "Investigate performance hotspots using Cloud Profiler: list profiles, find top functions, and drill into call paths",
		Arguments: []*mcp.PromptArgument{
			{Name: "service", Description: "Service/target name to investigate"},
			{Name: "profile_type", Description: "Profile type (CPU, HEAP, WALL, CONTENTION, etc.)"},
		},
	}, func(_ context.Context, request *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		service := request.Params.Arguments["service"]
		profileType := request.Params.Arguments["profile_type"]
		msg := "Investigate performance hotspots using Cloud Profiler:\n" +
			"1. Use profiler_list to discover available profiles"
		if service != "" {
			msg += fmt.Sprintf(" (filter by target: %s)", service)
		}
		if profileType != "" {
			msg += fmt.Sprintf(" (filter by type: %s)", profileType)
		}
		msg += "\n2. Use profiler_top on the most recent profile to identify the hottest functions" +
			"\n3. Use profiler_peek on the top hotspot to understand who calls it and what it calls" +
			"\n4. Use profiler_flamegraph to see the call subtree around the hotspot" +
			"\n5. Summarize the findings: which functions consume the most resources, potential optimizations"
		return &mcp.GetPromptResult{
			Messages: []*mcp.PromptMessage{
				{Role: "user", Content: &mcp.TextContent{Text: msg}},
			},
		}, nil
	})

	s.mcpServer.AddPrompt(&mcp.Prompt{
		Name:        "generate-metrics-registry",
		Description: "Scan a project for custom Prometheus/OTel metric definitions and generate a metrics registry overlay YAML for this MCP server",
		Arguments: []*mcp.PromptArgument{
			{Name: "project_path", Description: "Path to the target project root (defaults to current working directory)"},
			{Name: "output_path", Description: "Where to write the overlay YAML (defaults to .mcp/metrics_registry.yaml in the target project)"},
		},
	}, func(_ context.Context, request *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		projectPath := request.Params.Arguments["project_path"]
		if projectPath == "" {
			projectPath = "the current working directory"
		}
		outputPath := request.Params.Arguments["output_path"]
		if outputPath == "" {
			outputPath = ".mcp/metrics_registry.yaml"
		}
		serverBinary, execErr := os.Executable()
		if execErr != nil || serverBinary == "" {
			serverBinary = "mcp-gcp-observability"
		}
		msg := fmt.Sprintf(`Generate a metrics registry overlay for the mcp-gcp-observability MCP server.

TARGET PROJECT: %s
OUTPUT FILE:    %s

STEP 1 — Discover custom metric definitions in the target project.
Search the codebase for metric client-library calls. Cover multiple languages:
  - Go:     promauto.NewCounter/Gauge/Histogram/Summary, prometheus.NewCounter/Gauge/Histogram/Summary, *Vec variants, otel metric.Meter.Int64Counter/Float64Histogram/...
  - JS/TS:  new client.Counter/Gauge/Histogram/Summary from 'prom-client'
  - Python: Counter/Gauge/Histogram/Summary from prometheus_client
  - Java:   Micrometer Counter/Timer/Gauge, io.prometheus.client.*
  - Rust:   prometheus or metrics crate register_counter!/histogram!/...
For each hit record: metric name, type (counter/gauge/histogram/summary), label names, help text, unit, and the code context.

STEP 2 — Map each metric to how it ACTUALLY appears in GCP using metrics_list.

STEP 3 — Produce a YAML overlay. Required fields: kind, better_direction. Optional: unit, slo_threshold, saturation_cap, related_metrics, keywords, thresholds, aggregation.

Aggregation: optional; declares how to collapse the metric's time series. Only add when the per-kind default is wrong:
  (a) Ratio/hit-ratio gauges classified as business_kpi — use "across_groups: mean".
  (b) Peak/worst-case gauges — use "across_groups: max".
  (c) Per-entity gauges with entity labels (e.g. game_id, tenant_id) — use two-stage:
      aggregation:
        group_by: [metric.labels.entity_label]
        within_group: max
        across_groups: sum

Example:
  prometheus.googleapis.com/myservice_online_users_count/gauge:
    kind: business_kpi
    better_direction: up
    unit: users
    aggregation:
      group_by: [metric.labels.tenant_id]
      within_group: max
      across_groups: sum

STEP 4 — Save the file to: %s

STEP 5 — Validate: %s validate-registry <path>

STEP 6 — Report results.`, projectPath, outputPath, outputPath, serverBinary)
		return &mcp.GetPromptResult{
			Messages: []*mcp.PromptMessage{
				{Role: "user", Content: &mcp.TextContent{Text: msg}},
			},
		}, nil
	})
}

// promptCompleter provides autocomplete for prompt arguments.
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

func (p *promptCompleter) Handle(_ context.Context, req *mcp.CompleteRequest) (*mcp.CompleteResult, error) {
	prefix := strings.ToLower(req.Params.Argument.Value)
	var values []string

	// Only complete metric_type for the investigate-metrics prompt.
	if req.Params.Ref != nil && req.Params.Ref.Type == "ref/prompt" && req.Params.Ref.Name == "investigate-metrics" && req.Params.Argument.Name == "metric_type" {
		candidates := p.metricCandidates()
		for _, c := range candidates {
			if prefix == "" || strings.Contains(strings.ToLower(c), prefix) {
				values = append(values, c)
			}
		}
	}

	return &mcp.CompleteResult{
		Completion: mcp.CompletionResultDetails{
			Values:  values,
			HasMore: len(values) > 100,
		},
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
