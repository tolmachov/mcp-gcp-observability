package server

import (
	"context"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Re-auth guidance appended to tool errors caused by GCP rejecting the
// caller's credentials. Tool handlers embed the gRPC error text in their
// error results, so the stable gRPC code names are the detection signal.
const (
	unauthenticatedHint = " Hint: GCP rejected the credentials (expired or revoked token). " +
		"On the shared HTTP server, re-connect this MCP server so the OAuth login runs again; " +
		"locally, run `gcloud auth application-default login`."
	permissionDeniedHint = " Hint: your identity lacks IAM permission for this project or API. " +
		"Ask an administrator for the read-only observability roles " +
		"(logging.viewer, monitoring.viewer, cloudtrace.user, errorreporting.viewer, cloudprofiler.user)."
)

// authErrorHintMiddleware appends actionable guidance to tool errors caused
// by GCP authentication or authorization failures. It inspects error results
// (never successful ones) for the stable gRPC code names. It is attached to
// every server instance — including stdio — deliberately: the hint text
// covers both the shared-HTTP re-connect case and the local ADC case.
func authErrorHintMiddleware() func(mcp.MethodHandler) mcp.MethodHandler {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			if err != nil || method != "tools/call" {
				return res, err
			}
			result, ok := res.(*mcp.CallToolResult)
			if !ok || !result.IsError {
				return res, err
			}
			for _, c := range result.Content {
				tc, ok := c.(*mcp.TextContent)
				if !ok {
					continue
				}
				if hint := authHintFor(tc.Text); hint != "" && !strings.Contains(tc.Text, " Hint: ") {
					tc.Text += hint
				}
			}
			return res, err
		}
	}
}

// authHintFor maps a tool error message to a re-auth hint, or "".
func authHintFor(msg string) string {
	switch {
	case strings.Contains(msg, "Unauthenticated"):
		return unauthenticatedHint
	case strings.Contains(msg, "PermissionDenied"):
		return permissionDeniedHint
	}
	return ""
}
