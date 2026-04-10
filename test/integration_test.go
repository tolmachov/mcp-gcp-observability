//go:build integration

package test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/joho/godotenv"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal"
)

func init() {
	if err := godotenv.Load("../.env"); err != nil && !errors.Is(err, os.ErrNotExist) {
		panic(fmt.Sprintf("failed to load .env file: %v", err))
	}
}

func setupClient(t *testing.T) (*mcp.ClientSession, context.Context, func()) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)

	clientReader, serverWriter := io.Pipe()
	serverReader, clientWriter := io.Pipe()
	stderrReader, stderrWriter := io.Pipe()

	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stderrReader.Read(buf)
			if n > 0 {
				t.Logf("[server stderr] %s", string(buf[:n]))
			}
			if err != nil {
				return
			}
		}
	}()

	serverCtx, serverCancel := context.WithCancel(ctx)
	serverDone := make(chan error, 1)

	go func() {
		app := internal.New(serverReader, serverWriter, stderrWriter)
		err := app.Run(serverCtx, []string{"mcp-gcp-observability", "run"})
		serverDone <- err
		_ = serverReader.CloseWithError(fmt.Errorf("server exited: %v", err))
		_ = serverWriter.CloseWithError(fmt.Errorf("server exited: %v", err))
	}()

	transport := &mcp.IOTransport{
		Reader: io.NopCloser(clientReader),
		Writer: nopWriteCloser{clientWriter},
	}
	client := mcp.NewClient(
		&mcp.Implementation{Name: "mcp-gcp-observability-test", Version: "1.0.0"},
		nil,
	)

	session, err := client.Connect(ctx, transport, nil)
	require.NoError(t, err)

	cleanup := func() {
		serverCancel()
		_ = clientWriter.Close()
		_ = serverWriter.Close()
		_ = stderrWriter.Close()

		select {
		case err := <-serverDone:
			if err != nil && !errors.Is(err, context.Canceled) {
				assert.Failf(t, "server error", "%v", err)
			}
		case <-time.After(5 * time.Second):
			assert.Fail(t, "server did not stop in time")
		}

		cancel()
	}

	t.Logf("Connected to server")

	return session, ctx, cleanup
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

func callTool(t *testing.T, session *mcp.ClientSession, ctx context.Context, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      name,
		Arguments: args,
	})
	require.NoErrorf(t, err, "failed to call %s", name)
	return result
}

func textFromResult(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	require.NotEmpty(t, result.Content, "expected non-empty result content")
	tc, ok := result.Content[0].(*mcp.TextContent)
	require.True(t, ok, "expected TextContent, got %T", result.Content[0])
	return tc.Text
}

func TestListTools(t *testing.T) {
	session, ctx, cleanup := setupClient(t)
	defer cleanup()

	toolsResult, err := session.ListTools(ctx, nil)
	require.NoError(t, err)

	t.Logf("Available tools: %d", len(toolsResult.Tools))
	for _, tool := range toolsResult.Tools {
		t.Logf("  - %s: %s", tool.Name, tool.Description)
	}

	expectedTools := []string{
		"logs.query",
		"logs.k8s",
		"logs.by_trace",
		"logs.by_request_id",
		"logs.find_requests",
		"logs.summary",
		"logs.services",
		"errors.list",
		"errors.get",
		"trace.get",
	}

	toolNames := make(map[string]bool)
	for _, tool := range toolsResult.Tools {
		toolNames[tool.Name] = true
	}

	for _, expected := range expectedTools {
		assert.True(t, toolNames[expected], "expected tool %q not found", expected)
	}
}

func TestLogsQuery(t *testing.T) {
	session, ctx, cleanup := setupClient(t)
	defer cleanup()

	result := callTool(t, session, ctx, "logs.query", map[string]any{
		"filter": "severity>=ERROR",
		"limit":  5,
	})

	text := textFromResult(t, result)
	t.Logf("logs.query result: %s", text[:min(len(text), 500)])
}

func TestLogsServices(t *testing.T) {
	session, ctx, cleanup := setupClient(t)
	defer cleanup()

	result := callTool(t, session, ctx, "logs.services", map[string]any{})

	text := textFromResult(t, result)
	t.Logf("logs.services result: %s", text)

	var services struct {
		Count int `json:"count"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &services))

	if services.Count == 0 {
		t.Log("warning: no services found")
	}
}

func TestLogsFindRequests(t *testing.T) {
	urlPattern := os.Getenv("TEST_URL_PATTERN")
	if urlPattern == "" {
		urlPattern = "/api"
	}

	session, ctx, cleanup := setupClient(t)
	defer cleanup()

	result := callTool(t, session, ctx, "logs.find_requests", map[string]any{
		"url_pattern": urlPattern,
		"limit":       5,
	})

	text := textFromResult(t, result)
	t.Logf("logs.find_requests result: %s", text[:min(len(text), 500)])
}

func TestErrorsList(t *testing.T) {
	session, ctx, cleanup := setupClient(t)
	defer cleanup()

	result := callTool(t, session, ctx, "errors.list", map[string]any{
		"limit": 5,
	})

	text := textFromResult(t, result)
	t.Logf("errors.list result: %s", text[:min(len(text), 500)])
}

func TestLogsK8s(t *testing.T) {
	namespace := os.Getenv("TEST_K8S_NAMESPACE")
	if namespace == "" {
		t.Skip("TEST_K8S_NAMESPACE not set, skipping K8s test")
	}

	session, ctx, cleanup := setupClient(t)
	defer cleanup()

	result := callTool(t, session, ctx, "logs.k8s", map[string]any{
		"namespace": namespace,
		"limit":     5,
	})

	text := textFromResult(t, result)
	t.Logf("logs.k8s result: %s", text[:min(len(text), 500)])
}

func TestLogsByRequestID(t *testing.T) {
	requestID := os.Getenv("TEST_REQUEST_ID")
	if requestID == "" {
		session, ctx, cleanup := setupClient(t)
		defer cleanup()

		findResult := callTool(t, session, ctx, "logs.find_requests", map[string]any{
			"url_pattern": "/",
			"limit":       5,
		})

		text := textFromResult(t, findResult)

		var requests struct {
			Requests []struct {
				RequestID string `json:"request_id"`
			} `json:"requests"`
		}
		require.NoError(t, json.Unmarshal([]byte(text), &requests))

		for _, r := range requests.Requests {
			if r.RequestID != "" {
				requestID = r.RequestID
				break
			}
		}
		if requestID == "" {
			t.Skip("no requests with request_id found, skipping logs.by_request_id test")
		}

		result := callTool(t, session, ctx, "logs.by_request_id", map[string]any{
			"request_id": requestID,
			"limit":      10,
		})

		resultText := textFromResult(t, result)
		t.Logf("logs.by_request_id result (request_id=%s): %s", requestID, resultText[:min(len(resultText), 500)])

		var logResult struct {
			Count int `json:"count"`
		}
		require.NoError(t, json.Unmarshal([]byte(resultText), &logResult))

		assert.NotZero(t, logResult.Count, "expected at least one log entry for the request_id")
		return
	}

	session, ctx, cleanup := setupClient(t)
	defer cleanup()

	result := callTool(t, session, ctx, "logs.by_request_id", map[string]any{
		"request_id": requestID,
		"limit":      10,
	})

	text := textFromResult(t, result)
	t.Logf("logs.by_request_id result: %s", text[:min(len(text), 500)])
}

func TestLogsByTrace(t *testing.T) {
	traceID := os.Getenv("TEST_TRACE_ID")
	if traceID == "" {
		session, ctx, cleanup := setupClient(t)
		defer cleanup()

		findResult := callTool(t, session, ctx, "logs.find_requests", map[string]any{
			"traced_only": true,
			"limit":       1,
		})

		text := textFromResult(t, findResult)

		var requests struct {
			Requests []struct {
				TraceID string `json:"trace_id"`
			} `json:"requests"`
		}
		require.NoError(t, json.Unmarshal([]byte(text), &requests))

		if len(requests.Requests) == 0 || requests.Requests[0].TraceID == "" {
			t.Skip("no traced requests found, skipping logs.by_trace test")
		}
		traceID = requests.Requests[0].TraceID

		result := callTool(t, session, ctx, "logs.by_trace", map[string]any{
			"trace_id": traceID,
			"limit":    10,
		})

		traceText := textFromResult(t, result)
		t.Logf("logs.by_trace result (trace_id=%s): %s", traceID, traceText[:min(len(traceText), 500)])
		return
	}

	session, ctx, cleanup := setupClient(t)
	defer cleanup()

	result := callTool(t, session, ctx, "logs.by_trace", map[string]any{
		"trace_id": traceID,
		"limit":    10,
	})

	text := textFromResult(t, result)
	t.Logf("logs.by_trace result: %s", text[:min(len(text), 500)])
}

func TestErrorsGet(t *testing.T) {
	groupID := os.Getenv("TEST_ERROR_GROUP_ID")
	if groupID == "" {
		session, ctx, cleanup := setupClient(t)
		defer cleanup()

		listResult := callTool(t, session, ctx, "errors.list", map[string]any{
			"limit": 1,
		})

		text := textFromResult(t, listResult)

		var errorList struct {
			Groups []struct {
				GroupID string `json:"group_id"`
			} `json:"groups"`
		}
		require.NoError(t, json.Unmarshal([]byte(text), &errorList))

		if len(errorList.Groups) == 0 || errorList.Groups[0].GroupID == "" {
			t.Skip("no error groups found, skipping errors.get test")
		}
		groupID = errorList.Groups[0].GroupID

		result := callTool(t, session, ctx, "errors.get", map[string]any{
			"group_id": groupID,
			"limit":    5,
		})

		getText := textFromResult(t, result)
		t.Logf("errors.get result (group_id=%s): %s", groupID, getText[:min(len(getText), 500)])
		return
	}

	session, ctx, cleanup := setupClient(t)
	defer cleanup()

	result := callTool(t, session, ctx, "errors.get", map[string]any{
		"group_id": groupID,
		"limit":    5,
	})

	text := textFromResult(t, result)
	t.Logf("errors.get result: %s", text[:min(len(text), 500)])
}

func TestTraceGet(t *testing.T) {
	traceID := os.Getenv("TEST_TRACE_ID")
	if traceID == "" {
		session, ctx, cleanup := setupClient(t)
		defer cleanup()

		findResult := callTool(t, session, ctx, "logs.find_requests", map[string]any{
			"traced_only": true,
			"limit":       1,
		})

		text := textFromResult(t, findResult)

		var requests struct {
			Requests []struct {
				TraceID string `json:"trace_id"`
			} `json:"requests"`
		}
		require.NoError(t, json.Unmarshal([]byte(text), &requests))

		if len(requests.Requests) == 0 || requests.Requests[0].TraceID == "" {
			t.Skip("no traced requests found, skipping trace.get test")
		}
		traceID = requests.Requests[0].TraceID

		result := callTool(t, session, ctx, "trace.get", map[string]any{
			"trace_id": traceID,
		})

		traceText := textFromResult(t, result)
		t.Logf("trace.get result (trace_id=%s): %s", traceID, traceText[:min(len(traceText), 500)])

		var traceDetail struct {
			TraceID   string `json:"trace_id"`
			SpanCount int    `json:"span_count"`
		}
		require.NoError(t, json.Unmarshal([]byte(traceText), &traceDetail))

		assert.NotEmpty(t, traceDetail.TraceID, "expected non-empty trace_id in response")
		if traceDetail.SpanCount == 0 {
			t.Log("warning: trace has no spans")
		}
		return
	}

	session, ctx, cleanup := setupClient(t)
	defer cleanup()

	result := callTool(t, session, ctx, "trace.get", map[string]any{
		"trace_id": traceID,
	})

	text := textFromResult(t, result)
	t.Logf("trace.get result: %s", text[:min(len(text), 500)])
}

func TestLogsSummary(t *testing.T) {
	session, ctx, cleanup := setupClient(t)
	defer cleanup()

	result := callTool(t, session, ctx, "logs.summary", map[string]any{})

	text := textFromResult(t, result)
	t.Logf("logs.summary result: %s", text[:min(len(text), 500)])
}
