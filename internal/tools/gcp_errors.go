package tools

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
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

// causeGuidance is the advice for failures whose cause decides the advice
// whatever gRPC code they carry. It takes precedence over every per-code
// advice.
var causeGuidance = []struct {
	is     func(error) bool
	advice string
}{
	{isPanic, "This is a bug in the server, not a GCP failure; retrying will not help. Please report it."},
	{func(err error) bool { return errors.Is(err, gcpdata.ErrProfilerScanBudget) },
		"The profiler scan used up the per-call time budget, so retrying the same request will not help; " +
			"lower max_profiles, narrow the filters, or use a shorter time range."},
}

// codeInfo is what one gRPC code means for every GCP API.
type codeInfo struct {
	advice string
	// benign marks a failure there is nothing to report about: the client
	// canceled the request.
	benign bool
	// input marks a code caused by the request's own input. A tool's
	// fallback advice names the inputs to check, so it replaces advice.
	input bool
}

// sharedCodes is the advice for failures that mean the same thing for every
// GCP API. The credential hints cover both the shared HTTP server
// (re-connect to rerun the OAuth login) and local stdio use (ADC).
var sharedCodes = map[codes.Code]codeInfo{
	codes.Unauthenticated: {advice: "Hint: GCP rejected the credentials (expired or revoked token). " +
		"On the shared HTTP server, re-connect this MCP server so the OAuth login runs again; " +
		"locally, run `gcloud auth application-default login`."},
	codes.PermissionDenied: {advice: "Hint: your identity lacks IAM permission for this project or API. " +
		"Ask an administrator for the read-only observability roles " +
		"(logging.viewer, monitoring.viewer, cloudtrace.user, errorreporting.viewer, cloudprofiler.user)."},
	codes.DeadlineExceeded:  {advice: "The GCP API did not respond in time; retry the request or narrow the query."},
	codes.Canceled:          {advice: "The request was canceled before the GCP API returned a response.", benign: true},
	codes.Unavailable:       {advice: "The GCP API is temporarily unavailable or rate-limited; retry shortly."},
	codes.ResourceExhausted: {advice: "The GCP API is temporarily unavailable or rate-limited; retry shortly."},
	codes.NotFound:          {advice: "GCP found no such resource (check the project_id and the requested metric type, ID or name); retrying will not help.", input: true},
	codes.InvalidArgument:   {advice: "GCP rejected the request as invalid (check the filter and the other inputs); retrying will not help.", input: true},
}

// isBenign reports whether err is a failure there is nothing to report about.
func isBenign(err error) bool {
	return sharedCodes[errorCode(err)].benign
}

// codeGuidance is a tool's own advice for one gRPC code.
type codeGuidance struct {
	code     codes.Code
	guidance string
}

// gcpErrorResult reports a failed GCP call as a tool error: msg (which
// normally embeds err), then the guidance for err (see errorGuidance). Every
// handler fails the call on a GCP error through here or gcpErrorsResult.
func gcpErrorResult(msg string, err error, fallback string, byCode ...codeGuidance) *mcp.CallToolResult {
	return gcpErrorsResult(msg, []error{err}, fallback, byCode...)
}

// gcpErrorsResult reports several failed GCP calls as one tool error: msg,
// then the guidance for every distinct failure among errs.
func gcpErrorsResult(msg string, errs []error, fallback string, byCode ...codeGuidance) *mcp.CallToolResult {
	guidance := errorGuidance(errs, fallback, byCode...)
	if guidance == "" {
		return ErrorResult(msg)
	}
	return ErrorResult(msg + ". " + guidance)
}

// errorGuidance returns the advice for the non-nil errs, each distinct piece
// once, in order. Failures with different causes or codes each get their own
// advice rather than one error's advice standing in for all of them.
func errorGuidance(errs []error, fallback string, byCode ...codeGuidance) string {
	var out []string
	for _, err := range errs {
		if err == nil {
			continue
		}
		if g := errorAdvice(err, fallback, byCode); g != "" && !slices.Contains(out, g) {
			out = append(out, g)
		}
	}
	return strings.Join(out, " ")
}

// errorAdvice returns the advice for one failure: causeGuidance, then the
// tool's own byCode, then sharedCodes, then fallback. A tool's non-empty
// fallback replaces the shared advice of an input code.
func errorAdvice(err error, fallback string, byCode []codeGuidance) string {
	for _, c := range causeGuidance {
		if c.is(err) {
			return c.advice
		}
	}
	code := errorCode(err)
	for _, g := range byCode {
		if g.code == code {
			return g.guidance
		}
	}
	if shared, ok := sharedCodes[code]; ok && (!shared.input || fallback == "") {
		return shared.advice
	}
	return fallback
}
