package tools

import (
	"context"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

func RegisterTraceList(s *mcp.Server, d Deps) {
	requireTraces(d.Traces)
	mcp.AddTool(s, &mcp.Tool{
		Name: "trace_list",
		Description: applyMode(d.Mode, "Search for traces by criteria such as span name, latency, or time range. "+
			"Returns trace summaries with root span info — use trace_get with a returned trace_id for full span details. "+
			"Supports structured filters (root_name, span_name, min_latency) or raw Cloud Trace filter syntax. "+
			"Default time range is the last 1 hour. Requires Cloud Trace API to be enabled."),
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			OpenWorldHint:  new(true),
			IdempotentHint: true,
		},
		InputSchema: projectInputSchema[TraceListInput](d.Project,
			enumProp("order_by", traceOrderBys),
			enumProp("view", traceViews),
		),
		OutputSchema: outputSchemaFor[gcpdata.TraceListResult](),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in TraceListInput) (*mcp.CallToolResult, *gcpdata.TraceListResult, error) {
		project, err := d.Project.Resolve(in.ProjectID)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}

		// Build filter: raw filter takes precedence over structured params.
		filter := in.Filter
		if filter == "" {
			filter, err = gcpdata.BuildTraceFilter(in.RootName, in.SpanName, in.MinLatency)
			if err != nil {
				return errResult(err.Error()), nil, nil
			}
		}

		startTime, endTime, err := parseTimeRange(in.StartTime, in.EndTime, time.Hour)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}

		pageSize := clampLimit(in.Limit, 50, 200)

		sendProgress(ctx, req, 0, 1, "Listing traces...")

		result, err := d.Traces.ListTraces(ctx, project,
			filter, in.View, in.OrderBy, startTime, endTime, pageSize, in.PageToken)
		if err != nil {
			mcpLog(ctx, req, logLevelError, "trace_list", fmt.Sprintf("list traces failed: %v", err))
			return errResult(fmt.Sprintf("Failed to list traces: %v. Verify the project_id, filter syntax, and that Cloud Trace API is enabled.", err)), nil, nil
		}
		if result.Truncated && result.TruncationHint != "" {
			mcpLog(ctx, req, logLevelWarning, "trace_list", result.TruncationHint)
		}

		return nil, result, nil
	})
}
