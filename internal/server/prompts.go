package server

import (
	"context"
	"fmt"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerPrompts adds MCP prompts for common observability workflows to srv.
func (s *Server) registerPrompts(srv *mcp.Server) {
	srv.AddPrompt(&mcp.Prompt{
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

	srv.AddPrompt(&mcp.Prompt{
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

	srv.AddPrompt(&mcp.Prompt{
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

	srv.AddPrompt(&mcp.Prompt{
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

	srv.AddPrompt(&mcp.Prompt{
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

	srv.AddPrompt(&mcp.Prompt{
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
			// Surface the uncertainty in the prompt itself: the user invoking
			// the prompt does not see s.logger output, so a silent "mcp-gcp-
			// observability" fallback would have them copy-paste a command
			// that may not exist on their PATH.
			s.logger.Warn("could not determine server binary path", "err", execErr)
			serverBinary = "<path-to-mcp-gcp-observability>"
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
