package tools

import (
	"context"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// errorCode returns the gRPC code of err, mapping context cancellation and
// deadline errors (which carry no gRPC status) to their gRPC equivalents.
// A nil err is codes.OK; an error without a status is codes.Unknown.
func errorCode(err error) codes.Code {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return codes.DeadlineExceeded
	case errors.Is(err, context.Canceled):
		return codes.Canceled
	}
	return status.Code(err)
}

// sharedCodeGuidance is the advice for failures that mean the same thing for
// every GCP API. The credential hints cover both the shared HTTP server
// (re-connect to rerun the OAuth login) and local stdio use (ADC).
var sharedCodeGuidance = map[codes.Code]string{
	codes.Unauthenticated: "Hint: GCP rejected the credentials (expired or revoked token). " +
		"On the shared HTTP server, re-connect this MCP server so the OAuth login runs again; " +
		"locally, run `gcloud auth application-default login`.",
	codes.PermissionDenied: "Hint: your identity lacks IAM permission for this project or API. " +
		"Ask an administrator for the read-only observability roles " +
		"(logging.viewer, monitoring.viewer, cloudtrace.user, errorreporting.viewer, cloudprofiler.user).",
	codes.DeadlineExceeded:  "The GCP API did not respond in time; retry the request or narrow the query.",
	codes.Canceled:          "The request was canceled before the GCP API returned a response.",
	codes.Unavailable:       "The GCP API is temporarily unavailable or rate-limited; retry shortly.",
	codes.ResourceExhausted: "The GCP API is temporarily unavailable or rate-limited; retry shortly.",
}

// codeGuidance is a tool's own advice for one gRPC code.
type codeGuidance struct {
	code     codes.Code
	guidance string
}

// gcpErrorResult reports a failed GCP call as a tool error: msg (which
// normally embeds err), then guidance chosen once from err's gRPC code — the
// tool's own advice in byCode first, then sharedCodeGuidance, then fallback.
// Every handler that surfaces a GCP error goes through here.
func gcpErrorResult(msg string, err error, fallback string, byCode ...codeGuidance) *mcp.CallToolResult {
	code := errorCode(err)
	guidance := fallback
	if shared, ok := sharedCodeGuidance[code]; ok {
		guidance = shared
	}
	for _, g := range byCode {
		if g.code == code {
			guidance = g.guidance
		}
	}
	if guidance == "" {
		return errResult(msg)
	}
	return errResult(msg + ". " + guidance)
}
