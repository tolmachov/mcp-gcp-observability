package tools

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpclient"
	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

// traceSpanSchema is a hand-written JSON schema for TraceSpan that uses
// $ref/$defs to express the recursive Children field. The go-sdk's
// jsonschema-go library cannot auto-generate schemas for recursive types
// (it panics with "cycle detected"), so we provide the output schema
// explicitly.
var traceDetailSchema = &jsonschema.Schema{
	Type: "object",
	Properties: map[string]*jsonschema.Schema{
		"trace_id":   {Type: "string"},
		"span_count": {Type: "integer"},
		"spans": {
			Types: []string{"null", "array"},
			Items: &jsonschema.Schema{Ref: "#/$defs/TraceSpan"},
		},
	},
	Required: []string{"trace_id", "span_count", "spans"},
	Defs: map[string]*jsonschema.Schema{
		"TraceSpan": {
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"span_id":    {Type: "string"},
				"name":       {Type: "string"},
				"kind":       {Type: "string"},
				"start_time": {Type: "string"},
				"end_time":   {Type: "string"},
				"duration":   {Type: "string"},
				"labels":     {Type: "object", AdditionalProperties: &jsonschema.Schema{Type: "string"}},
				"children": {
					Types: []string{"null", "array"},
					Items: &jsonschema.Schema{Ref: "#/$defs/TraceSpan"},
				},
			},
			Required: []string{"span_id", "name", "start_time", "end_time", "duration"},
		},
	},
}

func RegisterTraceGet(s *mcp.Server, client *gcpclient.Client) {
	requireClient(client)
	mcp.AddTool(s, &mcp.Tool{
		Name: "trace_get",
		Description: "Get trace details with all spans by trace ID. " +
			"Returns a span tree (parent-child hierarchy) sorted by start time, showing the full request execution flow. " +
			"Use trace IDs from logs_find_requests results or the trace field in logs_query output. " +
			"Requires Cloud Trace API to be enabled in the project.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			OpenWorldHint:  new(true),
			IdempotentHint: true,
		},
		OutputSchema: traceDetailSchema,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in TraceGetInput) (*mcp.CallToolResult, *gcpdata.TraceDetail, error) {
		if in.TraceID == "" {
			return errResult("trace_id is required"), nil, nil
		}
		project, err := resolveProject(in.ProjectID, client.Config().DefaultProject)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}

		sendProgress(ctx, req, 0, 1, "Fetching trace...")

		result, err := gcpdata.GetTrace(ctx, client.TraceClient(), project, in.TraceID)
		if err != nil {
			mcpLog(ctx, req, logLevelError, "trace_get", fmt.Sprintf("get trace %s failed: %v", in.TraceID, err))
			return errResult(formatTraceGetError(in.TraceID, err)), nil, nil
		}

		return nil, result, nil
	})
}

func formatTraceGetError(traceID string, err error) string {
	base := fmt.Sprintf("Failed to get trace %q: %v.", traceID, err)
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return base + " Cloud Trace did not respond in time; retry the request or narrow the surrounding investigation window."
	case errors.Is(err, context.Canceled):
		return base + " The request was canceled before Cloud Trace returned a response."
	}
	if st, ok := status.FromError(err); ok {
		switch st.Code() {
		case codes.InvalidArgument:
			return base + " Verify the trace_id is a valid 32-character hex string (not the full resource path). Use logs_find_requests to discover valid trace IDs."
		case codes.NotFound:
			return base + " The trace does not exist in this project, may have aged out of retention, or the trace_id/project_id pair is wrong."
		case codes.PermissionDenied, codes.Unauthenticated:
			return base + " Verify your credentials and that Cloud Trace API access is permitted for this project."
		case codes.Unavailable, codes.ResourceExhausted:
			return base + " Cloud Trace is temporarily unavailable or rate-limited; retry shortly."
		}
	}
	return base + " Verify the project_id, credentials, and that Cloud Trace API is enabled."
}
