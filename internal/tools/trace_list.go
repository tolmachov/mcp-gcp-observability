package tools

import (
	"context"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpclient"
	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

func RegisterTraceList(s *mcp.Server, client *gcpclient.Client) {
	requireClient(client)
	mcp.AddTool(s, &mcp.Tool{
		Name: "trace.list",
		Description: "Search for traces by criteria such as span name, latency, or time range. " +
			"Returns trace summaries with root span info — use trace.get with a returned trace_id for full span details. " +
			"Supports structured filters (root_name, span_name, min_latency) or raw Cloud Trace filter syntax. " +
			"Default time range is the last 1 hour. Requires Cloud Trace API to be enabled.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			OpenWorldHint:  new(true),
			IdempotentHint: true,
		},
		InputSchema: inputSchemaWithEnums[TraceListInput](
			enumPatch{"order_by", enumTraceOrderBy},
			enumPatch{"view", enumTraceView},
		),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in TraceListInput) (*mcp.CallToolResult, *gcpdata.TraceListResult, error) {
		project, err := resolveProject(in.ProjectID, client.Config().DefaultProject)
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

		startTime, endTime, err := parseTraceTimeRange(in.StartTime, in.EndTime)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}

		pageSize := clampLimit(in.Limit, 50, 200)

		sendProgress(ctx, req, 0, 1, "Listing traces...")

		result, err := gcpdata.ListTraces(ctx, client.TraceClient(), project,
			filter, in.View, in.OrderBy, startTime, endTime, pageSize, in.PageToken)
		if err != nil {
			mcpLog(ctx, req, logLevelError, "trace.list", fmt.Sprintf("list traces failed: %v", err))
			return errResult(fmt.Sprintf("Failed to list traces: %v. Verify the project_id, filter syntax, and that Cloud Trace API is enabled.", err)), nil, nil
		}
		if result.Truncated && result.TruncationHint != "" {
			mcpLog(ctx, req, logLevelWarning, "trace.list", result.TruncationHint)
		}

		return nil, result, nil
	})
}

// parseTraceTimeRange parses start/end from TimeFilterInput, defaulting to last 1 hour.
// Unlike buildTimeFilter (which returns a Cloud Logging filter string), this returns
// time.Time values needed by the Cloud Trace API's protobuf timestamps.
func parseTraceTimeRange(startTimeStr, endTimeStr string) (time.Time, time.Time, error) {
	now := time.Now().UTC()
	var startTime, endTime time.Time

	if endTimeStr != "" {
		var err error
		endTime, err = time.Parse(time.RFC3339, endTimeStr)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid end_time %q: must be RFC3339 format (e.g. 2025-01-15T23:59:59Z)", endTimeStr)
		}
	} else {
		endTime = now
	}

	if startTimeStr != "" {
		var err error
		startTime, err = time.Parse(time.RFC3339, startTimeStr)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid start_time %q: must be RFC3339 format (e.g. 2025-01-15T00:00:00Z)", startTimeStr)
		}
	} else {
		startTime = endTime.Add(-1 * time.Hour)
	}

	if !endTime.After(startTime) {
		return time.Time{}, time.Time{}, fmt.Errorf("end_time must be after start_time (got start=%s, end=%s)",
			startTime.Format(time.RFC3339), endTime.Format(time.RFC3339))
	}

	return startTime, endTime, nil
}
