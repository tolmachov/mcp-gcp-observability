package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
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

func TestToolResultBudgetRejectsOversizedEncoding(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	next := func(context.Context, string, mcp.Request) (mcp.Result, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: strings.Repeat("x", maxEncodedToolResultBytes)}}}, nil
	}
	handler := toolLimitsMiddleware(make(chan struct{}, 4), make(chan struct{}, 2), logger)(next)
	result, err := handler(context.Background(), "tools/call", &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "oversized"}})
	require.Error(t, err)
	assert.Nil(t, result)
	assert.ErrorContains(t, err, "response budget")
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
