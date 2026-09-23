package server

import (
	"context"
	"fmt"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// promptArg declares one prompt argument. complete, when set, is the
// argument's completion source.
type promptArg struct {
	name, description string
	required          bool
	complete          func(context.Context, *promptCompleter) completionCandidates
}

// promptSpec declares one prompt. Project-scoped prompts accept project_id
// when the server is not pinned and start with the resolved project header.
// render builds the message body from the declared arguments (absent ones
// read as "").
type promptSpec struct {
	name, description string
	projectScoped     bool
	args              []promptArg
	render            func(s *Server, args map[string]string) string
}

// Completion sources for prompt arguments.
func completeMetricTypes(_ context.Context, p *promptCompleter) completionCandidates {
	return p.metricTypes
}

func completeServices(ctx context.Context, p *promptCompleter) completionCandidates {
	if p.loadServices == nil {
		return completionCandidates{}
	}
	return newCompletionCandidates(p.loadServices(ctx))
}

func completeProfileTypes(context.Context, *promptCompleter) completionCandidates {
	return profileTypeCandidates
}

// projectIDPromptArg is prepended to project-scoped prompts on unpinned servers.
var projectIDPromptArg = promptArg{
	name:        "project_id",
	description: "GCP project ID to use for every project-scoped tool call",
	required:    true,
}

// promptSpecs is the single table of prompts: it drives registration, the
// per-prompt argument allowlist and argument completion.
var promptSpecs = []promptSpec{
	{
		name:          "investigate-errors",
		description:   "Investigate top errors: list error groups, get details for the worst one, and find related logs",
		projectScoped: true,
		args: []promptArg{
			{name: "service", description: "Optional service name to filter errors", complete: completeServices},
		},
		render: func(_ *Server, args map[string]string) string {
			msg := "Investigate the top errors in the project:\n" +
				"1. Use errors_list to find the most frequent error groups"
			if service := args["service"]; service != "" {
				msg += fmt.Sprintf(" (filter by service: %s)", service)
			}
			return msg + "\n2. Use errors_get on the top error group to see stack traces and individual events" +
				"\n3. Use logs_query or logs_k8s to find related logs around the same time" +
				"\n4. If trace IDs are available, use logs_by_trace to follow the request flow" +
				"\n5. Summarize the root cause and suggest next steps"
		},
	},
	{
		name:          "trace-request",
		description:   "Trace a specific HTTP request end-to-end: find it by URL, follow its trace, and analyze spans",
		projectScoped: true,
		args: []promptArg{
			{name: "url_pattern", description: "URL pattern to search for (e.g. '/api/users')", required: true},
		},
		render: func(_ *Server, args map[string]string) string {
			return fmt.Sprintf("Trace a request matching URL pattern %q:\n", args["url_pattern"]) +
				"1. Use logs_find_requests to find matching HTTP requests with their trace IDs\n" +
				"2. Pick the most interesting request (e.g. slowest or with an error status)\n" +
				"3. Use trace_get to see the full span tree and identify slow spans\n" +
				"4. Use logs_by_trace to see all logs associated with that trace\n" +
				"5. Summarize the request flow, highlighting any issues or bottlenecks"
		},
	},
	{
		name:          "investigate-metrics",
		description:   "Investigate a metric anomaly: discover metrics, get snapshot, drill down by dimension, check related signals",
		projectScoped: true,
		args: []promptArg{
			{name: "metric_type", description: "Metric type to investigate (e.g. 'compute.googleapis.com/instance/cpu/utilization')", complete: completeMetricTypes},
			{name: "service", description: "Optional service or resource filter", complete: completeServices},
		},
		render: func(_ *Server, args map[string]string) string {
			msg := "Investigate a metric anomaly:\n"
			if metricType := args["metric_type"]; metricType != "" {
				msg += fmt.Sprintf("1. The metric to investigate is: %s\n", metricType)
			} else {
				msg += "1. Use metrics_list to discover available metrics"
				if service := args["service"]; service != "" {
					msg += fmt.Sprintf(" (filter by '%s')", service)
				}
				msg += "\n2. Pick the most relevant metric\n"
			}
			return msg + "3. Use metrics_snapshot to get a semantic snapshot with baseline comparison\n" +
				"4. If the classification shows a regression, use metrics_top_contributors to find which dimension contributes most\n" +
				"5. Use metrics_related to check correlated signals\n" +
				"6. Summarize the findings: what changed, when, likely cause, and recommended action"
		},
	},
	{
		name:          "service-health",
		description:   "Check the health of services: discover services, summarize logs, and identify issues",
		projectScoped: true,
		render: func(*Server, map[string]string) string {
			return "Check the health of services in the project:\n" +
				"1. Use logs_services to discover all available services\n" +
				"2. Use logs_summary to get an overview of severity distribution and top errors\n" +
				"3. Use errors_list to see the most frequent error groups\n" +
				"4. For any concerning services, use logs_k8s or logs_query to investigate further\n" +
				"5. Provide a health summary with any issues found and recommended actions"
		},
	},
	{
		name:          "investigate-profile",
		description:   "Investigate performance hotspots using Cloud Profiler: list profiles, find top functions, and drill into call paths",
		projectScoped: true,
		args: []promptArg{
			{name: "service", description: "Service/target name to investigate", complete: completeServices},
			{name: "profile_type", description: "Profile type (CPU, HEAP, WALL, CONTENTION, etc.)", complete: completeProfileTypes},
		},
		render: func(_ *Server, args map[string]string) string {
			msg := "Investigate performance hotspots using Cloud Profiler:\n" +
				"1. Use profiler_list to discover available profiles"
			if service := args["service"]; service != "" {
				msg += fmt.Sprintf(" (filter by target: %s)", service)
			}
			if profileType := args["profile_type"]; profileType != "" {
				msg += fmt.Sprintf(" (filter by type: %s)", profileType)
			}
			return msg + "\n2. Use profiler_top on the most recent profile to identify the hottest functions" +
				"\n3. Use profiler_peek on the top hotspot to understand who calls it and what it calls" +
				"\n4. Use profiler_flamegraph to see the call subtree around the hotspot" +
				"\n5. Summarize the findings: which functions consume the most resources, potential optimizations"
		},
	},
	{
		name:        "generate-metrics-registry",
		description: "Scan a project for custom Prometheus/OTel metric definitions and generate a metrics registry overlay YAML for this MCP server",
		args: []promptArg{
			{name: "project_path", description: "Path to the target project root (defaults to current working directory)"},
			{name: "output_path", description: "Where to write the overlay YAML (defaults to .mcp/metrics_registry.yaml in the target project)"},
		},
		render: func(s *Server, args map[string]string) string {
			projectPath := args["project_path"]
			if projectPath == "" {
				projectPath = "the current working directory"
			}
			outputPath := args["output_path"]
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
			return fmt.Sprintf(`Generate a metrics registry overlay for the mcp-gcp-observability MCP server.

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
		},
	},
}

// findPromptSpec returns the spec named name, or false if unknown.
func findPromptSpec(name string) (promptSpec, bool) {
	for _, p := range promptSpecs {
		if p.name == name {
			return p, true
		}
	}
	return promptSpec{}, false
}

// arguments returns the spec's declared arguments, led by project_id for
// project-scoped prompts on unpinned servers.
func (p promptSpec) arguments(pinned bool) []promptArg {
	if p.projectScoped && !pinned {
		return append([]promptArg{projectIDPromptArg}, p.args...)
	}
	return p.args
}

// userPrompt wraps msg as a single user message.
func userPrompt(msg string) *mcp.GetPromptResult {
	return &mcp.GetPromptResult{
		Messages: []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: msg}}},
	}
}

func (s *Server) promptProjectHeader(project string) string {
	if s.project.Pinned() {
		return fmt.Sprintf("GCP PROJECT: %s (server-pinned)\nDo not send project_id; pinned tool schemas reject it.\n\n", project)
	}
	return fmt.Sprintf("GCP PROJECT: %s\nPass this exact project_id to every project-scoped tool.\n\n", project)
}

// registerPrompts adds every prompt in promptSpecs to srv.
func (s *Server) registerPrompts(srv *mcp.Server) {
	for _, spec := range promptSpecs {
		declared := spec.arguments(s.project.Pinned())
		arguments := make([]*mcp.PromptArgument, len(declared))
		allowed := make(map[string]bool, len(declared))
		for i, a := range declared {
			arguments[i] = &mcp.PromptArgument{Name: a.name, Description: a.description, Required: a.required}
			allowed[a.name] = true
		}
		srv.AddPrompt(&mcp.Prompt{Name: spec.name, Description: spec.description, Arguments: arguments},
			func(_ context.Context, request *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
				var args map[string]string
				if request != nil && request.Params != nil {
					args = request.Params.Arguments
				}
				for name := range args {
					if !allowed[name] {
						return nil, fmt.Errorf("unknown prompt argument %q", name)
					}
				}
				if !spec.projectScoped {
					return userPrompt(spec.render(s, args)), nil
				}
				project, err := s.project.Resolve(args["project_id"])
				if err != nil {
					return nil, fmt.Errorf("project policy: %w", err)
				}
				return userPrompt(s.promptProjectHeader(project) + spec.render(s, args)), nil
			})
	}
}
