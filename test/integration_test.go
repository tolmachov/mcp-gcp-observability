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

	"github.com/joho/godotenv"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal"
)

func init() {
	if err := godotenv.Load("../.env"); err != nil && !errors.Is(err, os.ErrNotExist) {
		panic(fmt.Sprintf("failed to load .env file: %v", err))
	}
}

func setupClient(t *testing.T) (*client.Client, context.Context, func()) {
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
	}()

	stdioTransport := transport.NewIO(clientReader, clientWriter, stderrReader)
	c := client.NewClient(stdioTransport)

	cleanup := func() {
		if err := c.Close(); err != nil {
			t.Errorf("failed to close client: %v", err)
		}
		serverCancel()
		_ = clientWriter.Close()
		_ = serverWriter.Close()
		_ = stderrWriter.Close()

		select {
		case err := <-serverDone:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("server error: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("server did not stop in time")
		}

		cancel()
	}

	if err := c.Start(ctx); err != nil {
		cleanup()
		t.Fatalf("failed to start client: %v", err)
	}

	initRequest := mcp.InitializeRequest{}
	initRequest.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initRequest.Params.ClientInfo = mcp.Implementation{
		Name:    "mcp-gcp-observability-test",
		Version: "1.0.0",
	}
	initRequest.Params.Capabilities = mcp.ClientCapabilities{}

	serverInfo, err := c.Initialize(ctx, initRequest)
	if err != nil {
		cleanup()
		t.Fatalf("failed to initialize: %v", err)
	}

	t.Logf("Connected to server: %s (version %s)", serverInfo.ServerInfo.Name, serverInfo.ServerInfo.Version)

	return c, ctx, cleanup
}

func TestListTools(t *testing.T) {
	c, ctx, cleanup := setupClient(t)
	defer cleanup()

	toolsResult, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("failed to list tools: %v", err)
	}

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
		if !toolNames[expected] {
			t.Errorf("expected tool %q not found", expected)
		}
	}
}

func TestLogsQuery(t *testing.T) {
	c, ctx, cleanup := setupClient(t)
	defer cleanup()

	result, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "logs.query",
			Arguments: map[string]any{
				"filter": "severity>=ERROR",
				"limit":  5,
			},
		},
	})
	if err != nil {
		t.Fatalf("failed to call logs.query: %v", err)
	}

	if len(result.Content) == 0 {
		t.Fatal("expected non-empty result")
	}

	text := result.Content[0].(mcp.TextContent).Text
	t.Logf("logs.query result: %s", text[:min(len(text), 500)])
}

func TestLogsServices(t *testing.T) {
	c, ctx, cleanup := setupClient(t)
	defer cleanup()

	result, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "logs.services",
			Arguments: map[string]any{},
		},
	})
	if err != nil {
		t.Fatalf("failed to call logs.services: %v", err)
	}

	if len(result.Content) == 0 {
		t.Fatal("expected non-empty result")
	}

	text := result.Content[0].(mcp.TextContent).Text
	t.Logf("logs.services result: %s", text)

	var services struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal([]byte(text), &services); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}

	if services.Count == 0 {
		t.Log("warning: no services found")
	}
}

func TestLogsFindRequests(t *testing.T) {
	urlPattern := os.Getenv("TEST_URL_PATTERN")
	if urlPattern == "" {
		urlPattern = "/api"
	}

	c, ctx, cleanup := setupClient(t)
	defer cleanup()

	result, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "logs.find_requests",
			Arguments: map[string]any{
				"url_pattern": urlPattern,
				"limit":       5,
			},
		},
	})
	if err != nil {
		t.Fatalf("failed to call logs.find_requests: %v", err)
	}

	if len(result.Content) == 0 {
		t.Fatal("expected non-empty result")
	}

	text := result.Content[0].(mcp.TextContent).Text
	t.Logf("logs.find_requests result: %s", text[:min(len(text), 500)])
}

func TestErrorsList(t *testing.T) {
	c, ctx, cleanup := setupClient(t)
	defer cleanup()

	result, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "errors.list",
			Arguments: map[string]any{
				"limit": 5,
			},
		},
	})
	if err != nil {
		t.Fatalf("failed to call errors.list: %v", err)
	}

	if len(result.Content) == 0 {
		t.Fatal("expected non-empty result")
	}

	text := result.Content[0].(mcp.TextContent).Text
	t.Logf("errors.list result: %s", text[:min(len(text), 500)])
}

func TestLogsK8s(t *testing.T) {
	namespace := os.Getenv("TEST_K8S_NAMESPACE")
	if namespace == "" {
		t.Skip("TEST_K8S_NAMESPACE not set, skipping K8s test")
	}

	c, ctx, cleanup := setupClient(t)
	defer cleanup()

	result, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "logs.k8s",
			Arguments: map[string]any{
				"namespace": namespace,
				"limit":     5,
			},
		},
	})
	if err != nil {
		t.Fatalf("failed to call logs.k8s: %v", err)
	}

	if len(result.Content) == 0 {
		t.Fatal("expected non-empty result")
	}

	text := result.Content[0].(mcp.TextContent).Text
	t.Logf("logs.k8s result: %s", text[:min(len(text), 500)])
}

func TestLogsByRequestID(t *testing.T) {
	requestID := os.Getenv("TEST_REQUEST_ID")
	if requestID == "" {
		// Find a request with request_id
		c, ctx, cleanup := setupClient(t)
		defer cleanup()

		findResult, err := c.CallTool(ctx, mcp.CallToolRequest{
			Params: mcp.CallToolParams{
				Name: "logs.find_requests",
				Arguments: map[string]any{
					"url_pattern": "/",
					"limit":       5,
				},
			},
		})
		if err != nil {
			t.Fatalf("failed to find requests: %v", err)
		}

		text := findResult.Content[0].(mcp.TextContent).Text

		var requests struct {
			Requests []struct {
				RequestID string `json:"request_id"`
			} `json:"requests"`
		}
		if err := json.Unmarshal([]byte(text), &requests); err != nil {
			t.Fatalf("failed to parse find_requests result: %v", err)
		}

		for _, r := range requests.Requests {
			if r.RequestID != "" {
				requestID = r.RequestID
				break
			}
		}
		if requestID == "" {
			t.Skip("no requests with request_id found, skipping logs.by_request_id test")
		}

		result, err := c.CallTool(ctx, mcp.CallToolRequest{
			Params: mcp.CallToolParams{
				Name: "logs.by_request_id",
				Arguments: map[string]any{
					"request_id": requestID,
					"limit":      10,
				},
			},
		})
		if err != nil {
			t.Fatalf("failed to call logs.by_request_id: %v", err)
		}

		if len(result.Content) == 0 {
			t.Fatal("expected non-empty result")
		}

		resultText := result.Content[0].(mcp.TextContent).Text
		t.Logf("logs.by_request_id result (request_id=%s): %s", requestID, resultText[:min(len(resultText), 500)])

		var logResult struct {
			Count int `json:"count"`
		}
		if err := json.Unmarshal([]byte(resultText), &logResult); err != nil {
			t.Fatalf("failed to parse result: %v", err)
		}

		if logResult.Count == 0 {
			t.Error("expected at least one log entry for the request_id")
		}
		return
	}

	// Use provided request ID
	c, ctx, cleanup := setupClient(t)
	defer cleanup()

	result, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "logs.by_request_id",
			Arguments: map[string]any{
				"request_id": requestID,
				"limit":      10,
			},
		},
	})
	if err != nil {
		t.Fatalf("failed to call logs.by_request_id: %v", err)
	}

	if len(result.Content) == 0 {
		t.Fatal("expected non-empty result")
	}

	text := result.Content[0].(mcp.TextContent).Text
	t.Logf("logs.by_request_id result: %s", text[:min(len(text), 500)])
}

func TestLogsByTrace(t *testing.T) {
	traceID := os.Getenv("TEST_TRACE_ID")
	if traceID == "" {
		// First find a traced request to get a real trace ID
		c, ctx, cleanup := setupClient(t)
		defer cleanup()

		findResult, err := c.CallTool(ctx, mcp.CallToolRequest{
			Params: mcp.CallToolParams{
				Name: "logs.find_requests",
				Arguments: map[string]any{
					"traced_only": true,
					"limit":       1,
				},
			},
		})
		if err != nil {
			t.Fatalf("failed to find traced request: %v", err)
		}

		text := findResult.Content[0].(mcp.TextContent).Text

		var requests struct {
			Requests []struct {
				TraceID string `json:"trace_id"`
			} `json:"requests"`
		}
		if err := json.Unmarshal([]byte(text), &requests); err != nil {
			t.Fatalf("failed to parse find_requests result: %v", err)
		}

		if len(requests.Requests) == 0 || requests.Requests[0].TraceID == "" {
			t.Skip("no traced requests found, skipping logs.by_trace test")
		}
		traceID = requests.Requests[0].TraceID

		// Now test by_trace with the found trace ID
		result, err := c.CallTool(ctx, mcp.CallToolRequest{
			Params: mcp.CallToolParams{
				Name: "logs.by_trace",
				Arguments: map[string]any{
					"trace_id": traceID,
					"limit":    10,
				},
			},
		})
		if err != nil {
			t.Fatalf("failed to call logs.by_trace: %v", err)
		}

		if len(result.Content) == 0 {
			t.Fatal("expected non-empty result")
		}

		traceText := result.Content[0].(mcp.TextContent).Text
		t.Logf("logs.by_trace result (trace_id=%s): %s", traceID, traceText[:min(len(traceText), 500)])
		return
	}

	// Use provided trace ID
	c, ctx, cleanup := setupClient(t)
	defer cleanup()

	result, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "logs.by_trace",
			Arguments: map[string]any{
				"trace_id": traceID,
				"limit":    10,
			},
		},
	})
	if err != nil {
		t.Fatalf("failed to call logs.by_trace: %v", err)
	}

	if len(result.Content) == 0 {
		t.Fatal("expected non-empty result")
	}

	text := result.Content[0].(mcp.TextContent).Text
	t.Logf("logs.by_trace result: %s", text[:min(len(text), 500)])
}

func TestErrorsGet(t *testing.T) {
	groupID := os.Getenv("TEST_ERROR_GROUP_ID")
	if groupID == "" {
		// First get an error group ID from errors.list
		c, ctx, cleanup := setupClient(t)
		defer cleanup()

		listResult, err := c.CallTool(ctx, mcp.CallToolRequest{
			Params: mcp.CallToolParams{
				Name: "errors.list",
				Arguments: map[string]any{
					"limit": 1,
				},
			},
		})
		if err != nil {
			t.Fatalf("failed to list errors: %v", err)
		}

		text := listResult.Content[0].(mcp.TextContent).Text

		var errorList struct {
			Groups []struct {
				GroupID string `json:"group_id"`
			} `json:"groups"`
		}
		if err := json.Unmarshal([]byte(text), &errorList); err != nil {
			t.Fatalf("failed to parse errors.list result: %v", err)
		}

		if len(errorList.Groups) == 0 || errorList.Groups[0].GroupID == "" {
			t.Skip("no error groups found, skipping errors.get test")
		}
		groupID = errorList.Groups[0].GroupID

		// Now test errors.get with the found group ID
		result, err := c.CallTool(ctx, mcp.CallToolRequest{
			Params: mcp.CallToolParams{
				Name: "errors.get",
				Arguments: map[string]any{
					"group_id": groupID,
					"limit":    5,
				},
			},
		})
		if err != nil {
			t.Fatalf("failed to call errors.get: %v", err)
		}

		if len(result.Content) == 0 {
			t.Fatal("expected non-empty result")
		}

		getText := result.Content[0].(mcp.TextContent).Text
		t.Logf("errors.get result (group_id=%s): %s", groupID, getText[:min(len(getText), 500)])
		return
	}

	// Use provided group ID
	c, ctx, cleanup := setupClient(t)
	defer cleanup()

	result, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "errors.get",
			Arguments: map[string]any{
				"group_id": groupID,
				"limit":    5,
			},
		},
	})
	if err != nil {
		t.Fatalf("failed to call errors.get: %v", err)
	}

	if len(result.Content) == 0 {
		t.Fatal("expected non-empty result")
	}

	text := result.Content[0].(mcp.TextContent).Text
	t.Logf("errors.get result: %s", text[:min(len(text), 500)])
}

func TestTraceGet(t *testing.T) {
	traceID := os.Getenv("TEST_TRACE_ID")
	if traceID == "" {
		// Find a traced request to get a real trace ID
		c, ctx, cleanup := setupClient(t)
		defer cleanup()

		findResult, err := c.CallTool(ctx, mcp.CallToolRequest{
			Params: mcp.CallToolParams{
				Name: "logs.find_requests",
				Arguments: map[string]any{
					"traced_only": true,
					"limit":       1,
				},
			},
		})
		if err != nil {
			t.Fatalf("failed to find traced request: %v", err)
		}

		text := findResult.Content[0].(mcp.TextContent).Text

		var requests struct {
			Requests []struct {
				TraceID string `json:"trace_id"`
			} `json:"requests"`
		}
		if err := json.Unmarshal([]byte(text), &requests); err != nil {
			t.Fatalf("failed to parse find_requests result: %v", err)
		}

		if len(requests.Requests) == 0 || requests.Requests[0].TraceID == "" {
			t.Skip("no traced requests found, skipping trace.get test")
		}
		traceID = requests.Requests[0].TraceID

		result, err := c.CallTool(ctx, mcp.CallToolRequest{
			Params: mcp.CallToolParams{
				Name: "trace.get",
				Arguments: map[string]any{
					"trace_id": traceID,
				},
			},
		})
		if err != nil {
			t.Fatalf("failed to call trace.get: %v", err)
		}

		if len(result.Content) == 0 {
			t.Fatal("expected non-empty result")
		}

		traceText := result.Content[0].(mcp.TextContent).Text
		t.Logf("trace.get result (trace_id=%s): %s", traceID, traceText[:min(len(traceText), 500)])

		var traceDetail struct {
			TraceID   string `json:"trace_id"`
			SpanCount int    `json:"span_count"`
		}
		if err := json.Unmarshal([]byte(traceText), &traceDetail); err != nil {
			t.Fatalf("failed to parse trace.get result: %v", err)
		}

		if traceDetail.TraceID == "" {
			t.Error("expected non-empty trace_id in response")
		}
		if traceDetail.SpanCount == 0 {
			t.Log("warning: trace has no spans")
		}
		return
	}

	// Use provided trace ID
	c, ctx, cleanup := setupClient(t)
	defer cleanup()

	result, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "trace.get",
			Arguments: map[string]any{
				"trace_id": traceID,
			},
		},
	})
	if err != nil {
		t.Fatalf("failed to call trace.get: %v", err)
	}

	if len(result.Content) == 0 {
		t.Fatal("expected non-empty result")
	}

	text := result.Content[0].(mcp.TextContent).Text
	t.Logf("trace.get result: %s", text[:min(len(text), 500)])
}

func TestLogsSummary(t *testing.T) {
	c, ctx, cleanup := setupClient(t)
	defer cleanup()

	result, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "logs.summary",
			Arguments: map[string]any{},
		},
	})
	if err != nil {
		t.Fatalf("failed to call logs.summary: %v", err)
	}

	if len(result.Content) == 0 {
		t.Fatal("expected non-empty result")
	}

	text := result.Content[0].(mcp.TextContent).Text
	t.Logf("logs.summary result: %s", text[:min(len(text), 500)])
}
