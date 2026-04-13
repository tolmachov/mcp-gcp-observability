package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpclient"
	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

func RegisterLogsFindRequests(s *mcp.Server, client *gcpclient.Client) {
	requireClient(client)
	mcp.AddTool(s, &mcp.Tool{
		Name: "logs.find_requests",
		Description: "Find examples of HTTP requests by URL pattern. " +
			"Returns trace_id and request_id for each request, enabling deeper investigation with logs.by_trace, logs.by_request_id, or trace.get.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			OpenWorldHint:  new(true),
			IdempotentHint: true,
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in LogsFindRequestsInput) (*mcp.CallToolResult, *gcpdata.RequestList, error) {
		if in.URLPattern == "" {
			return errResult("url_pattern is required"), nil, nil
		}
		if in.StatusCode != 0 && (in.StatusCode < 100 || in.StatusCode > 599) {
			return errResult(fmt.Sprintf("invalid status_code %d: must be in range [100, 599]", in.StatusCode)), nil, nil
		}
		validMethods := map[string]bool{"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true, "HEAD": true, "OPTIONS": true}
		if in.Method != "" && !validMethods[in.Method] {
			return errResult(fmt.Sprintf("invalid method %q: must be one of GET, POST, PUT, PATCH, DELETE, HEAD, OPTIONS", in.Method)), nil, nil
		}
		project, err := resolveProject(in.ProjectID, client.Config().DefaultProject)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		limit := clampLimit(in.Limit, 20, client.Config().LogsMaxLimit)

		timeFilter, err := buildTimeFilter(in.TimeFilterInput)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}

		sendProgress(ctx, req, 0, 1, "Finding requests...")

		result, err := gcpdata.FindRequests(ctx, client.LoggingClient(), project, in.URLPattern, in.Method, in.StatusCode, in.TracedOnly, timeFilter, limit)
		if err != nil {
			mcpLog(ctx, req, logLevelError, "logs.find_requests", fmt.Sprintf("find requests failed: %v", err))
			return errResult(fmt.Sprintf("Failed to find requests: %v. Verify the project_id and that the URL pattern is correct.", err)), nil, nil
		}

		return nil, result, nil
	})
}
