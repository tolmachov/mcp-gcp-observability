package tools

import (
	"context"
	"errors"
	"slices"
	"strings"

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
// normally embeds err), then the guidance for err's gRPC code (see
// errorGuidance). Every handler fails the call on a GCP error through here or
// gcpErrorsResult.
func gcpErrorResult(msg string, err error, fallback string, byCode ...codeGuidance) *mcp.CallToolResult {
	return gcpErrorsResult(msg, []error{err}, fallback, byCode...)
}

// gcpErrorsResult reports several failed GCP calls as one tool error: msg,
// then the guidance for every distinct gRPC code among errs.
func gcpErrorsResult(msg string, errs []error, fallback string, byCode ...codeGuidance) *mcp.CallToolResult {
	guidance := errorGuidance(errs, fallback, byCode...)
	if guidance == "" {
		return errResult(msg)
	}
	return errResult(msg + ". " + guidance)
}

// errorGuidance returns the advice for the gRPC codes of the non-nil errs,
// each distinct piece once, in order. A code's advice is the tool's own in
// byCode first, then sharedCodeGuidance, then fallback. Failures with
// different codes each get their own advice rather than one error's advice
// standing in for all of them.
func errorGuidance(errs []error, fallback string, byCode ...codeGuidance) string {
	var out []string
	for _, err := range errs {
		if err == nil {
			continue
		}
		if g := codeAdvice(errorCode(err), fallback, byCode); g != "" && !slices.Contains(out, g) {
			out = append(out, g)
		}
	}
	return strings.Join(out, " ")
}

// codeAdvice returns the advice for one gRPC code: byCode, then
// sharedCodeGuidance, then fallback.
func codeAdvice(code codes.Code, fallback string, byCode []codeGuidance) string {
	for _, g := range byCode {
		if g.code == code {
			return g.guidance
		}
	}
	if shared, ok := sharedCodeGuidance[code]; ok {
		return shared
	}
	return fallback
}
