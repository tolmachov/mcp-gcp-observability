package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

func TestRequestBodyLimitReturns413(t *testing.T) {
	called := false
	handler := limitRequestBody(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	for _, tc := range []struct {
		name    string
		chunked bool
	}{
		{name: "content length"},
		{name: "chunked", chunked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called = false
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(strings.Repeat("x", maxMCPRequestBytes+1)))
			if tc.chunked {
				req.ContentLength = -1
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
			assert.False(t, called)
		})
	}
}

func TestPropertyToolResultNeverEmitsNonFiniteJSON(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rapid.Check(t, func(t *rapid.T) {
		kind := rapid.IntRange(0, 3).Draw(t, "kind")
		value := rapid.Float64Range(-1e100, 1e100).Draw(t, "finite")
		switch kind {
		case 1:
			value = math.NaN()
		case 2:
			value = math.Inf(1)
		case 3:
			value = math.Inf(-1)
		}
		next := func(context.Context, string, mcp.Request) (mcp.Result, error) {
			return &mcp.CallToolResult{StructuredContent: map[string]any{"nested": []any{map[string]any{"value": value}}}}, nil
		}
		handler := toolLimitsMiddleware(make(chan struct{}, 4), make(chan struct{}, 2), logger)(next)
		result, err := handler(context.Background(), "tools/call", &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "property"}})
		if math.IsNaN(value) || math.IsInf(value, 0) {
			assert.Error(t, err)
			assert.Nil(t, result)
			return
		}
		assert.NoError(t, err)
		assert.NotNil(t, result)
	})
}

func TestToolResultBudgetReturnsActionableToolError(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	next := func(context.Context, string, mcp.Request) (mcp.Result, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: strings.Repeat("x", maxEncodedToolResultBytes)}}}, nil
	}
	handler := toolLimitsMiddleware(make(chan struct{}, 4), make(chan struct{}, 2), logger)(next)
	result, err := handler(context.Background(), "tools/call", &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "oversized"}})
	require.NoError(t, err)
	call, ok := result.(*mcp.CallToolResult)
	require.True(t, ok)
	assert.True(t, call.IsError)
	require.Len(t, call.Content, 1)
	text, ok := call.Content[0].(*mcp.TextContent)
	require.True(t, ok)
	assert.Contains(t, text.Text, "oversized result")
	assert.Contains(t, text.Text, "response budget")
	assert.Contains(t, text.Text, "Narrow the request")
	assert.Contains(t, logs.String(), "msg=response_budget_violation tool=oversized")
}

// TestToolResultBudgetErrorReachesClient pins that the SDK delivers the
// middleware's replacement result to a real client as a tool error.
func TestToolResultBudgetErrorReachesClient(t *testing.T) {
	s := testServer(t)
	s.profiler = make(chan struct{}, 2)
	srv := s.newMCPInstance(s.completer)
	srv.AddTool(&mcp.Tool{Name: "oversized", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: strings.Repeat("x", maxEncodedToolResultBytes)}}}, nil
		})
	res, err := connectInMemory(t, srv).CallTool(context.Background(), &mcp.CallToolParams{Name: "oversized"})
	require.NoError(t, err)
	assert.True(t, res.IsError)
	require.Len(t, res.Content, 1)
	text, ok := res.Content[0].(*mcp.TextContent)
	require.True(t, ok)
	assert.Contains(t, text.Text, "Narrow the request")
}

func TestToolSlotWaitTimeoutNamesTheTool(t *testing.T) {
	const wait = 20 * time.Millisecond
	// callTool returns the text of the tool error result, or "" when the call
	// ran.
	callTool := func(t *testing.T, userCalls, profilerCalls chan struct{}, tool string) (called bool, logs *bytes.Buffer, toolErr string) {
		t.Helper()
		logs = &bytes.Buffer{}
		logger := slog.New(slog.NewTextHandler(logs, nil))
		next := func(context.Context, string, mcp.Request) (mcp.Result, error) {
			called = true
			return &mcp.CallToolResult{}, nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), wait)
		defer cancel()
		res, err := toolLimitsMiddleware(userCalls, profilerCalls, logger)(next)(
			ctx, "tools/call", &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: tool}})
		require.NoError(t, err)
		call, ok := res.(*mcp.CallToolResult)
		require.True(t, ok)
		if !call.IsError {
			return called, logs, ""
		}
		require.Len(t, call.Content, 1)
		text, ok := call.Content[0].(*mcp.TextContent)
		require.True(t, ok)
		return called, logs, text.Text
	}

	t.Run("per-user limit", func(t *testing.T) {
		userCalls := make(chan struct{}, 1)
		userCalls <- struct{}{}
		called, logs, toolErr := callTool(t, userCalls, make(chan struct{}, 2), "logs_query")
		assert.Contains(t, toolErr, "tool logs_query never got a concurrent-call slot (limit 1) after waiting")
		assert.Contains(t, toolErr, context.DeadlineExceeded.Error())
		assert.False(t, called)
		assert.Contains(t, logs.String(), "msg=tool_slot_wait_aborted tool=logs_query slot=concurrent-call limit=1 waited=")
	})
	t.Run("profiler limit", func(t *testing.T) {
		userCalls := make(chan struct{}, 4)
		profilerCalls := make(chan struct{}, 1)
		profilerCalls <- struct{}{}
		called, logs, toolErr := callTool(t, userCalls, profilerCalls, "profiler_top")
		assert.Contains(t, toolErr, "tool profiler_top never got a profiler slot (limit 1) after waiting")
		assert.Contains(t, toolErr, context.DeadlineExceeded.Error())
		assert.False(t, called)
		assert.Contains(t, logs.String(), "msg=profiler_saturation limit=1 tool=profiler_top")
		assert.Contains(t, logs.String(), "msg=tool_slot_wait_aborted tool=profiler_top slot=profiler limit=1 waited=")
		assert.Empty(t, userCalls, "the per-user slot must be released")
	})
	t.Run("full profiler limit does not block other tools", func(t *testing.T) {
		profilerCalls := make(chan struct{}, 1)
		profilerCalls <- struct{}{}
		called, _, toolErr := callTool(t, make(chan struct{}, 4), profilerCalls, "logs_query")
		assert.Empty(t, toolErr)
		assert.True(t, called)
	})
}

// TestProfileScanToolsRunUnderProfilerLimits pins which calls take a profiler
// slot and the profiler deadline, keyed on profileScanTools.
func TestProfileScanToolsRunUnderProfilerLimits(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	for _, spec := range toolSpecs {
		t.Run(spec.name, func(t *testing.T) {
			userCalls, profilerCalls := make(chan struct{}, 4), make(chan struct{}, 2)
			var heldProfilerSlot, hasDeadline bool
			next := func(ctx context.Context, _ string, _ mcp.Request) (mcp.Result, error) {
				heldProfilerSlot = len(profilerCalls) == 1
				deadline, ok := ctx.Deadline()
				hasDeadline = ok && time.Until(deadline) <= gcpdata.ProfilerScanTimeout
				return &mcp.CallToolResult{}, nil
			}
			_, err := toolLimitsMiddleware(userCalls, profilerCalls, logger)(next)(
				context.Background(), "tools/call", &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: spec.name}})
			require.NoError(t, err)
			assert.Equal(t, spec.profileScan, heldProfilerSlot)
			assert.Equal(t, spec.profileScan, hasDeadline)
			assert.Empty(t, userCalls)
			assert.Empty(t, profilerCalls)
		})
	}
}

func TestToolResultBudgetCompactsAutomaticStructuredContentDuplication(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	structured := json.RawMessage(`{"entries":["` + strings.Repeat("x", 1300<<10) + `"]}`)
	next := func(context.Context, string, mcp.Request) (mcp.Result, error) {
		return &mcp.CallToolResult{
			Content:           []mcp.Content{&mcp.TextContent{Text: string(structured)}},
			StructuredContent: structured,
		}, nil
	}
	handler := toolLimitsMiddleware(make(chan struct{}, 4), make(chan struct{}, 2), logger)(next)
	result, err := handler(context.Background(), "tools/call", &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "large-structured"}})
	require.NoError(t, err)
	call, ok := result.(*mcp.CallToolResult)
	require.True(t, ok)
	require.Len(t, call.Content, 1)
	textContent, ok := call.Content[0].(*mcp.TextContent)
	require.True(t, ok)
	assert.Equal(t, structuredResultContentNotice, textContent.Text)
	encoded, err := json.Marshal(result)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(encoded), maxEncodedToolResultBytes)
}
