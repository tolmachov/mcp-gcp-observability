package tools

import (
	"context"
	"encoding/json"
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
	return &LogsK8sHandler{client: client}
}

// Tool returns the MCP tool definition.
func (h *LogsK8sHandler) Tool() mcp.Tool {
	return mcp.NewTool("logs.k8s",
		mcp.WithDescription("Query Kubernetes container logs with convenient filters. Automatically builds Cloud Logging filter for resource.type=\"k8s_container\"."),
		mcp.WithReadOnlyHintAnnotation(true),
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
			mcp.Description("Minimum severity: DEFAULT, DEBUG, INFO, NOTICE, WARNING, ERROR, CRITICAL, ALERT, EMERGENCY"),
		),
		mcp.WithString("text_search",
			mcp.Description("Text to search for in log payloads"),
		),
		mcp.WithString("project_id",
			mcp.Description("GCP project ID (uses default if not specified)"),
		),
		mcp.WithNumber("limit",
			mcp.Description("Maximum number of entries to return (default 100)"),
		),
	)
}

// Handle processes the logs.k8s tool request.
func (h *LogsK8sHandler) Handle(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := request.GetString("project_id", h.client.Config.DefaultProject)
	limit := clampLimit(request.GetInt("limit", 100), 100, h.client.Config.LogsMaxLimit)

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

	result, err := gcpdata.QueryLogs(ctx, h.client.Logging, project, filter, limit, "desc", "")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to query K8s logs: %v", err)), nil
	}

	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to marshal result: %v", err)), nil
	}

	return mcp.NewToolResultText(string(data)), nil
}
