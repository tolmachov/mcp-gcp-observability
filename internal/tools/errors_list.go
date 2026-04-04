package tools

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpclient"
	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

// ErrorsListHandler handles the errors.list tool.
type ErrorsListHandler struct {
	client *gcpclient.Client
}

// NewErrorsListHandler creates a new ErrorsListHandler.
func NewErrorsListHandler(client *gcpclient.Client) *ErrorsListHandler {
	return &ErrorsListHandler{client: requireClient(client)}
}

// Tool returns the MCP tool definition.
func (h *ErrorsListHandler) Tool() mcp.Tool {
	return newToolWithTimeFilter("errors.list",
		mcp.WithDescription("List error groups from Google Cloud Error Reporting, sorted by occurrence count. "+
			"Returns aggregated errors with group IDs, not individual log entries. "+
			"Use errors.get with a group_id from these results to see individual error events and stack traces. "+
			"Time range can be specified via start_time/end_time (RFC3339, same as other tools) or time_range_hours. "+
			"If either start_time or end_time is provided, time_range_hours is ignored. "+
			"Note: Error Reporting supports fixed periods (1h, 6h, 1d, 1w, 30d) — your range is rounded up to the next available period."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithString("project_id",
			mcp.Description("GCP project ID (uses default if not specified)"),
		),
		mcp.WithNumber("time_range_hours",
			mcp.Description("Time range in hours to look back (default 24, max 720). Ignored if start_time/end_time are provided."),
			mcp.Min(1),
			mcp.Max(720),
		),
		mcp.WithNumber("limit",
			mcp.Description("Maximum number of error groups to return (default 50, server max applies)"),
			mcp.Min(1),
		),
		mcp.WithString("service_filter",
			mcp.Description("Filter by service name"),
		),
		mcp.WithString("version_filter",
			mcp.Description("Filter by service version"),
		),
	)
}

// Handle processes the errors.list tool request.
func (h *ErrorsListHandler) Handle(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project, errResult := requireProject(request, h.client.Config().DefaultProject)
	if errResult != nil {
		return errResult, nil
	}

	timeRangeHours, err := resolveErrorsTimeRange(request)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	limit := clampLimit(request.GetInt("limit", 50), 50, h.client.Config().ErrorsMaxLimit)
	serviceFilter := request.GetString("service_filter", "")
	versionFilter := request.GetString("version_filter", "")

	result, err := gcpdata.ListErrors(ctx, h.client.ErrorsClient(), project, timeRangeHours, limit, serviceFilter, versionFilter)
	if err != nil {
		mcpLog(ctx, mcp.LoggingLevelError, "errors.list", fmt.Sprintf("list errors failed for project %s: %v", project, err))
		return mcp.NewToolResultError(fmt.Sprintf("Failed to list errors: %v. Verify the project_id and that Error Reporting API is enabled.", err)), nil
	}

	return jsonResult(result)
}

// resolveErrorsTimeRange returns the time range in hours for Error Reporting.
// If either start_time or end_time is provided (RFC3339), compute hours from the delta (rounded up).
// When end_time is omitted, it defaults to now. When start_time is omitted, it defaults to 24h ago.
// Returns an error if end_time is before start_time or the range exceeds 720 hours.
// Otherwise falls back to time_range_hours parameter (default 24).
func resolveErrorsTimeRange(request mcp.CallToolRequest) (int, error) {
	startTime := request.GetString("start_time", "")
	endTime := request.GetString("end_time", "")

	if startTime != "" || endTime != "" {
		now := time.Now()
		start := now.Add(-24 * time.Hour)
		end := now

		if startTime != "" {
			parsed, err := time.Parse(time.RFC3339, startTime)
			if err != nil {
				return 0, fmt.Errorf("invalid start_time %q: must be RFC3339 format (e.g. 2025-01-15T00:00:00Z)", startTime)
			}
			start = parsed
		}
		if endTime != "" {
			parsed, err := time.Parse(time.RFC3339, endTime)
			if err != nil {
				return 0, fmt.Errorf("invalid end_time %q: must be RFC3339 format (e.g. 2025-01-15T23:59:59Z)", endTime)
			}
			end = parsed
		}

		if !end.After(start) {
			return 0, fmt.Errorf("end_time (%s) must be after start_time (%s)", end.Format(time.RFC3339), start.Format(time.RFC3339))
		}

		hours := int(math.Ceil(end.Sub(start).Hours()))
		if hours < 1 {
			hours = 1
		}
		if hours > 720 {
			return 0, fmt.Errorf("time range of %d hours exceeds maximum of 720 hours (30 days)", hours)
		}
		return hours, nil
	}

	hours := request.GetInt("time_range_hours", 24)
	if hours < 1 || hours > 720 {
		return 0, fmt.Errorf("time_range_hours must be between 1 and 720, got %d", hours)
	}
	return hours, nil
}
