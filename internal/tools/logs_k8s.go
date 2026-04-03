package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpclient"
	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

// LogsK8sHandler handles the logs.k8s tool.
type LogsK8sHandler struct {
	client *gcpclient.Client
}

// NewLogsK8sHandler creates a new LogsK8sHandler.
func NewLogsK8sHandler(client *gcpclient.Client) *LogsK8sHandler {
	return &LogsK8sHandler{client: requireClient(client)}
}

// Tool returns the MCP tool definition.
func (h *LogsK8sHandler) Tool() mcp.Tool {
	return newToolWithTimeFilter("logs.k8s",
		mcp.WithDescription("Query Kubernetes container logs with convenient filters. "+
			"Automatically builds Cloud Logging filter for resource.type=\"k8s_container\". "+
			"Preferred over logs.query for K8s workloads. Results default to newest-first (use order parameter to change)."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithString("namespace",
			mcp.Description("Kubernetes namespace name"),
		),
		mcp.WithString("pod_name",
			mcp.Description("Pod name (supports substring match)"),
		),
		mcp.WithString("container_name",
			mcp.Description("Container name"),
		),
		mcp.WithString("severity",
			mcp.Description("Minimum log severity level to return"),
			mcp.Enum("DEFAULT", "DEBUG", "INFO", "NOTICE", "WARNING", "ERROR", "CRITICAL", "ALERT", "EMERGENCY"),
		),
		mcp.WithString("text_search",
			mcp.Description("Text to search for in log payloads"),
		),
		mcp.WithString("project_id",
			mcp.Description("GCP project ID (uses default if not specified)"),
		),
		mcp.WithNumber("limit",
			mcp.Description("Maximum number of log entries to return (default 100, server max applies)"),
			mcp.Min(1),
		),
		mcp.WithString("order",
			mcp.Description("Sort order by timestamp (default 'desc')"),
			mcp.Enum("asc", "desc"),
		),
		mcp.WithString("page_token",
			mcp.Description("Page token for pagination (from previous response's next_page_token)"),
		),
	)
}

// Handle processes the logs.k8s tool request.
func (h *LogsK8sHandler) Handle(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project, errResult := requireProject(request, h.client.Config().DefaultProject)
	if errResult != nil {
		return errResult, nil
	}
	limit := clampLimit(request.GetInt("limit", 100), 100, h.client.Config().LogsMaxLimit)

	// Build K8s-specific filter
	parts := []string{`resource.type="k8s_container"`}

	if ns := request.GetString("namespace", ""); ns != "" {
		parts = append(parts, fmt.Sprintf(`resource.labels.namespace_name="%s"`, gcpdata.EscapeFilterValue(ns)))
	}
	if pod := request.GetString("pod_name", ""); pod != "" {
		parts = append(parts, fmt.Sprintf(`resource.labels.pod_name:"%s"`, gcpdata.EscapeFilterValue(pod)))
	}
	if container := request.GetString("container_name", ""); container != "" {
		parts = append(parts, fmt.Sprintf(`resource.labels.container_name="%s"`, gcpdata.EscapeFilterValue(container)))
	}
	if severity := request.GetString("severity", ""); severity != "" {
		severity = strings.ToUpper(severity)
		if !gcpdata.IsValidSeverity(severity) {
			return mcp.NewToolResultError(fmt.Sprintf("invalid severity %q: must be one of DEFAULT, DEBUG, INFO, NOTICE, WARNING, ERROR, CRITICAL, ALERT, EMERGENCY", severity)), nil
		}
		parts = append(parts, fmt.Sprintf(`severity>=%s`, severity))
	}
	if text := request.GetString("text_search", ""); text != "" {
		escaped := gcpdata.EscapeFilterValue(text)
		parts = append(parts, fmt.Sprintf(`(textPayload:"%s" OR jsonPayload.message:"%s")`, escaped, escaped))
	}

	filter := strings.Join(parts, "\n")

	timeFilter, err := buildTimeFilter(request)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	filter = gcpdata.AppendFilter(filter, timeFilter)

	order := request.GetString("order", "desc")
	if order != "asc" && order != "desc" {
		return mcp.NewToolResultError(fmt.Sprintf("invalid order %q: must be \"asc\" or \"desc\"", order)), nil
	}
	pageToken := request.GetString("page_token", "")

	result, err := gcpdata.QueryLogs(ctx, h.client.LoggingClient(), project, filter, limit, order, pageToken)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to query K8s logs: %v. Verify the project_id and that K8s logging is enabled.", err)), nil
	}

	return jsonResult(result)
}
