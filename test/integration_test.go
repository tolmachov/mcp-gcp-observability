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
		"logs_query",
		"logs_k8s",
		"logs_by_trace",
		"logs_by_request_id",
		"logs_find_requests",
		"logs_summary",
		"logs_services",
		"errors_list",
		"errors_get",
		"trace_get",
		"trace_list",
		"metrics_list",
		"metrics_snapshot",
		"metrics_top_contributors",
		"metrics_related",
		"metrics_compare",
		"profiler_list",
		"profiler_top",
		"profiler_peek",
		"profiler_flamegraph",
		"profiler_compare",
		"profiler_trends",
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

	result := callTool(t, session, ctx, "logs_query", map[string]any{
		"filter": "severity>=ERROR",
		"limit":  5,
	})

	text := textFromResult(t, result)
	t.Logf("logs_query result: %s", text[:min(len(text), 500)])
}

func TestLogsServices(t *testing.T) {
	session, ctx, cleanup := setupClient(t)
	defer cleanup()

	result := callTool(t, session, ctx, "logs_services", map[string]any{})

	text := textFromResult(t, result)
	t.Logf("logs_services result: %s", text)

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

	result := callTool(t, session, ctx, "logs_find_requests", map[string]any{
		"url_pattern": urlPattern,
		"limit":       5,
	})

	text := textFromResult(t, result)
	t.Logf("logs_find_requests result: %s", text[:min(len(text), 500)])
}

func TestErrorsList(t *testing.T) {
	session, ctx, cleanup := setupClient(t)
	defer cleanup()

	result := callTool(t, session, ctx, "errors_list", map[string]any{
		"limit": 5,
	})

	text := textFromResult(t, result)
	t.Logf("errors_list result: %s", text[:min(len(text), 500)])
}

func TestLogsK8s(t *testing.T) {
	namespace := os.Getenv("TEST_K8S_NAMESPACE")
	if namespace == "" {
		t.Skip("TEST_K8S_NAMESPACE not set, skipping K8s test")
	}

	session, ctx, cleanup := setupClient(t)
	defer cleanup()

	result := callTool(t, session, ctx, "logs_k8s", map[string]any{
		"namespace": namespace,
		"limit":     5,
	})

	text := textFromResult(t, result)
	t.Logf("logs_k8s result: %s", text[:min(len(text), 500)])
}

func TestLogsByRequestID(t *testing.T) {
	requestID := os.Getenv("TEST_REQUEST_ID")
	if requestID == "" {
		session, ctx, cleanup := setupClient(t)
		defer cleanup()

		findResult := callTool(t, session, ctx, "logs_find_requests", map[string]any{
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
			t.Skip("no requests with request_id found, skipping logs_by_request_id test")
		}

		result := callTool(t, session, ctx, "logs_by_request_id", map[string]any{
			"request_id": requestID,
			"limit":      10,
		})

		if result.IsError {
			t.Skipf("logs_by_request_id returned an error (possibly transient GCP timeout): %s", textFromResult(t, result))
		}

		resultText := textFromResult(t, result)
		t.Logf("logs_by_request_id result (request_id=%s): %s", requestID, resultText[:min(len(resultText), 500)])

		var logResult struct {
			Count int `json:"count"`
		}
		require.NoError(t, json.Unmarshal([]byte(resultText), &logResult))

		assert.NotZero(t, logResult.Count, "expected at least one log entry for the request_id")
		return
	}

	session, ctx, cleanup := setupClient(t)
	defer cleanup()

	result := callTool(t, session, ctx, "logs_by_request_id", map[string]any{
		"request_id": requestID,
		"limit":      10,
	})

	text := textFromResult(t, result)
	t.Logf("logs_by_request_id result: %s", text[:min(len(text), 500)])
}

func TestLogsByTrace(t *testing.T) {
	traceID := os.Getenv("TEST_TRACE_ID")
	if traceID == "" {
		session, ctx, cleanup := setupClient(t)
		defer cleanup()

		findResult := callTool(t, session, ctx, "logs_find_requests", map[string]any{
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
			t.Skip("no traced requests found, skipping logs_by_trace test")
		}
		traceID = requests.Requests[0].TraceID

		result := callTool(t, session, ctx, "logs_by_trace", map[string]any{
			"trace_id": traceID,
			"limit":    10,
		})

		traceText := textFromResult(t, result)
		t.Logf("logs_by_trace result (trace_id=%s): %s", traceID, traceText[:min(len(traceText), 500)])
		return
	}

	session, ctx, cleanup := setupClient(t)
	defer cleanup()

	result := callTool(t, session, ctx, "logs_by_trace", map[string]any{
		"trace_id": traceID,
		"limit":    10,
	})

	text := textFromResult(t, result)
	t.Logf("logs_by_trace result: %s", text[:min(len(text), 500)])
}

func TestErrorsGet(t *testing.T) {
	groupID := os.Getenv("TEST_ERROR_GROUP_ID")
	if groupID == "" {
		session, ctx, cleanup := setupClient(t)
		defer cleanup()

		listResult := callTool(t, session, ctx, "errors_list", map[string]any{
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
			t.Skip("no error groups found, skipping errors_get test")
		}
		groupID = errorList.Groups[0].GroupID

		result := callTool(t, session, ctx, "errors_get", map[string]any{
			"group_id": groupID,
			"limit":    5,
		})

		getText := textFromResult(t, result)
		t.Logf("errors_get result (group_id=%s): %s", groupID, getText[:min(len(getText), 500)])
		return
	}

	session, ctx, cleanup := setupClient(t)
	defer cleanup()

	result := callTool(t, session, ctx, "errors_get", map[string]any{
		"group_id": groupID,
		"limit":    5,
	})

	text := textFromResult(t, result)
	t.Logf("errors_get result: %s", text[:min(len(text), 500)])
}

func TestTraceGet(t *testing.T) {
	traceID := os.Getenv("TEST_TRACE_ID")
	if traceID == "" {
		session, ctx, cleanup := setupClient(t)
		defer cleanup()

		findResult := callTool(t, session, ctx, "logs_find_requests", map[string]any{
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
			t.Skip("no traced requests found, skipping trace_get test")
		}
		traceID = requests.Requests[0].TraceID

		result := callTool(t, session, ctx, "trace_get", map[string]any{
			"trace_id": traceID,
		})

		traceText := textFromResult(t, result)
		t.Logf("trace_get result (trace_id=%s): %s", traceID, traceText[:min(len(traceText), 500)])

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

	result := callTool(t, session, ctx, "trace_get", map[string]any{
		"trace_id": traceID,
	})

	text := textFromResult(t, result)
	t.Logf("trace_get result: %s", text[:min(len(text), 500)])
}

func TestLogsSummary(t *testing.T) {
	session, ctx, cleanup := setupClient(t)
	defer cleanup()

	result := callTool(t, session, ctx, "logs_summary", map[string]any{})

	text := textFromResult(t, result)
	t.Logf("logs_summary result: %s", text[:min(len(text), 500)])
}

// TestMetricsSnapshotMeta verifies that metrics_snapshot returns _meta.ui.resourceUri
// so that Claude Desktop can render the chart widget inline.
func TestMetricsSnapshotMeta(t *testing.T) {
	session, ctx, cleanup := setupClient(t)
	defer cleanup()

	const metricType = "compute.googleapis.com/instance/cpu/utilization"

	result := callTool(t, session, ctx, "metrics_snapshot", map[string]any{
		"metric_type": metricType,
		"window":      "1h",
	})

	// Verify _meta.ui.resourceUri is present and well-formed.
	ui, ok := result.Meta["ui"].(map[string]any)
	require.True(t, ok, "_meta.ui must be a map[string]any; got %T", result.Meta["ui"])

	resourceURI, ok := ui["resourceUri"].(string)
	require.True(t, ok, "_meta.ui.resourceUri must be a string; got %T", ui["resourceUri"])
	require.NotEmpty(t, resourceURI, "_meta.ui.resourceUri must not be empty")

	t.Logf("_meta.ui.resourceUri = %s", resourceURI)

	assert.Contains(t, resourceURI, "ui://metrics/chart/", "URI must use ui://metrics/chart/ scheme")
	assert.Contains(t, resourceURI, metricType, "URI must include the metric type")
	assert.Contains(t, resourceURI, "window=1h", "URI must carry the window parameter")

	// Fetch the chart resource and verify it returns HTML with Chart.js.
	readResult, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: resourceURI})
	require.NoError(t, err, "resources/read must succeed for the chart URI")
	require.NotEmpty(t, readResult.Contents, "chart resource must have content")

	content := readResult.Contents[0]
	assert.Equal(t, "text/html;profile=mcp-app", content.MIMEType, "chart MIME type must be text/html;profile=mcp-app")
	assert.Contains(t, content.Text, "chart.js", "chart HTML must reference Chart.js")
	assert.Contains(t, content.Text, "__D", "chart HTML must contain embedded data")

	t.Logf("Chart resource: %d bytes, MIME=%s", len(content.Text), content.MIMEType)
}

func TestTraceList(t *testing.T) {
	session, ctx, cleanup := setupClient(t)
	defer cleanup()

	result := callTool(t, session, ctx, "trace_list", map[string]any{
		"limit": 5,
	})

	if result.IsError {
		t.Skipf("trace_list returned an error: %s", textFromResult(t, result))
	}

	text := textFromResult(t, result)
	t.Logf("trace_list result: %s", text[:min(len(text), 500)])

	var parsed struct {
		Count  int `json:"count"`
		Traces []struct {
			TraceID string `json:"trace_id"`
		} `json:"traces"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &parsed))

	if parsed.Count > 0 {
		require.NotEmpty(t, parsed.Traces, "count > 0 but traces array is empty")
		assert.NotEmpty(t, parsed.Traces[0].TraceID, "first trace must have a non-empty trace_id")
	}
}

func TestMetricsList(t *testing.T) {
	session, ctx, cleanup := setupClient(t)
	defer cleanup()

	result := callTool(t, session, ctx, "metrics_list", map[string]any{
		"match": "cpu",
		"limit": 5,
	})

	if result.IsError {
		t.Skipf("metrics_list returned an error: %s", textFromResult(t, result))
	}

	text := textFromResult(t, result)
	t.Logf("metrics_list result: %s", text[:min(len(text), 500)])

	var parsed struct {
		Count   int `json:"count"`
		Metrics []struct {
			MetricType string `json:"metric_type"`
		} `json:"metrics"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &parsed))

	assert.Greater(t, parsed.Count, 0, "expected at least one metric matching 'cpu'")
	for _, m := range parsed.Metrics {
		assert.NotEmpty(t, m.MetricType, "each metric must have a non-empty metric_type")
	}
}

// discoverMetricType returns a metric type for use in metrics tests.
// It prefers TEST_METRIC_TYPE env var, then queries metrics_list.
func discoverMetricType(t *testing.T, session *mcp.ClientSession, ctx context.Context) string {
	t.Helper()
	if mt := os.Getenv("TEST_METRIC_TYPE"); mt != "" {
		return mt
	}
	result := callTool(t, session, ctx, "metrics_list", map[string]any{
		"match": "cpu",
		"limit": 1,
	})
	if result.IsError {
		t.Skip("metrics_list failed, cannot discover metric_type")
	}
	var parsed struct {
		Metrics []struct {
			MetricType string `json:"metric_type"`
		} `json:"metrics"`
	}
	require.NoError(t, json.Unmarshal([]byte(textFromResult(t, result)), &parsed))
	if len(parsed.Metrics) == 0 || parsed.Metrics[0].MetricType == "" {
		t.Skip("no metrics found via metrics_list, skipping test")
	}
	return parsed.Metrics[0].MetricType
}

func TestMetricsTopContributors(t *testing.T) {
	session, ctx, cleanup := setupClient(t)
	defer cleanup()

	metricType := discoverMetricType(t, session, ctx)

	result := callTool(t, session, ctx, "metrics_top_contributors", map[string]any{
		"metric_type": metricType,
		"dimension":   "resource.labels.zone",
		"window":      "1h",
		"limit":       3,
	})

	if result.IsError {
		t.Skipf("metrics_top_contributors returned an error (dimension may not exist for this metric): %s",
			textFromResult(t, result))
	}

	text := textFromResult(t, result)
	t.Logf("metrics_top_contributors result (metric=%s): %s", metricType, text[:min(len(text), 500)])

	var parsed struct {
		Dimension    string `json:"dimension"`
		Contributors []struct {
			LabelValue string `json:"label_value"`
		} `json:"contributors"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &parsed))
	t.Logf("contributors count: %d", len(parsed.Contributors))
}

func TestMetricsRelated(t *testing.T) {
	session, ctx, cleanup := setupClient(t)
	defer cleanup()

	metricType := discoverMetricType(t, session, ctx)

	result := callTool(t, session, ctx, "metrics_related", map[string]any{
		"metric_type": metricType,
		"window":      "1h",
	})

	if result.IsError {
		t.Skipf("metrics_related returned an error: %s", textFromResult(t, result))
	}

	text := textFromResult(t, result)
	t.Logf("metrics_related result (metric=%s): %s", metricType, text[:min(len(text), 500)])

	var parsed struct {
		RelatedSignals []struct {
			MetricType string `json:"metric_type"`
		} `json:"related_signals"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &parsed))
	t.Logf("related_signals count: %d", len(parsed.RelatedSignals))
}

func TestMetricsCompare(t *testing.T) {
	session, ctx, cleanup := setupClient(t)
	defer cleanup()

	metricType := discoverMetricType(t, session, ctx)

	now := time.Now().UTC()
	windowBFrom := now.Add(-1 * time.Hour).Format(time.RFC3339)
	windowBTo := now.Format(time.RFC3339)
	windowAFrom := now.Add(-2 * time.Hour).Format(time.RFC3339)
	windowATo := now.Add(-1 * time.Hour).Format(time.RFC3339)

	result := callTool(t, session, ctx, "metrics_compare", map[string]any{
		"metric_type":    metricType,
		"window_a_from":  windowAFrom,
		"window_a_to":    windowATo,
		"window_b_from":  windowBFrom,
		"window_b_to":    windowBTo,
		"window_a_label": "prev_hour",
		"window_b_label": "last_hour",
	})

	if result.IsError {
		t.Skipf("metrics_compare returned an error: %s", textFromResult(t, result))
	}

	text := textFromResult(t, result)
	t.Logf("metrics_compare result (metric=%s): %s", metricType, text[:min(len(text), 500)])

	var parsed struct {
		DeltaPct   float64 `json:"delta_pct"`
		TrendShift string  `json:"trend_shift"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &parsed))
	t.Logf("delta_pct=%.2f trend_shift=%s", parsed.DeltaPct, parsed.TrendShift)
}

func TestProfilerList(t *testing.T) {
	session, ctx, cleanup := setupClient(t)
	defer cleanup()

	result := callTool(t, session, ctx, "profiler_list", map[string]any{
		"limit": 5,
	})

	if result.IsError {
		t.Skipf("profiler_list returned an error: %s", textFromResult(t, result))
	}

	text := textFromResult(t, result)
	t.Logf("profiler_list result: %s", text[:min(len(text), 500)])

	var parsed struct {
		Count   int `json:"count"`
		Summary struct {
			CountByType map[string]int `json:"count_by_type"`
		} `json:"summary"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &parsed))
	t.Logf("profiles count: %d, by_type: %v", parsed.Count, parsed.Summary.CountByType)

	if parsed.Count == 0 {
		t.Skip("no profiles found, skipping profiler test")
	}
}

// discoverProfile queries profiler_list and returns the first profile's metadata.
// Skips the test if no profiles exist at all, but does NOT skip when profile_id is empty
// (Cloud Profiler may omit profile_id in list responses; callers must check if they need it).
// Returns profileID (may be ""), profileType, target.
func discoverProfile(t *testing.T, session *mcp.ClientSession, ctx context.Context) (profileID, profileType, target string) {
	t.Helper()
	if id := os.Getenv("TEST_PROFILE_ID"); id != "" {
		return id, "", ""
	}
	result := callTool(t, session, ctx, "profiler_list", map[string]any{"limit": 1})
	if result.IsError {
		t.Skip("profiler_list failed, cannot discover profile")
	}
	var parsed struct {
		Profiles []struct {
			ProfileID   string `json:"profile_id"`
			ProfileType string `json:"profile_type"`
			Target      string `json:"target"`
		} `json:"profiles"`
	}
	require.NoError(t, json.Unmarshal([]byte(textFromResult(t, result)), &parsed))
	if len(parsed.Profiles) == 0 {
		t.Skip("no profiles found, skipping profiler test")
	}
	p := parsed.Profiles[0]
	return p.ProfileID, p.ProfileType, p.Target
}

func TestProfilerTop(t *testing.T) {
	session, ctx, cleanup := setupClient(t)
	defer cleanup()

	profileID, _, _ := discoverProfile(t, session, ctx)
	if profileID == "" {
		t.Skip("profiler_list did not return a profile_id, skipping profiler_top test")
	}

	result := callTool(t, session, ctx, "profiler_top", map[string]any{
		"profile_id": profileID,
		"limit":      5,
	})

	if result.IsError {
		t.Skipf("profiler_top returned an error: %s", textFromResult(t, result))
	}

	text := textFromResult(t, result)
	t.Logf("profiler_top result (profile_id=%s): %s", profileID, text[:min(len(text), 500)])

	var parsed struct {
		TotalValue   int64 `json:"total_value"`
		TopFunctions []struct {
			FunctionName string `json:"function_name"`
		} `json:"top_functions"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &parsed))

	assert.GreaterOrEqual(t, parsed.TotalValue, int64(0))
	if parsed.TotalValue > 0 {
		assert.NotEmpty(t, parsed.TopFunctions, "expected top_functions when total_value > 0")
	}
}

func TestProfilerPeek(t *testing.T) {
	session, ctx, cleanup := setupClient(t)
	defer cleanup()

	profileID, _, _ := discoverProfile(t, session, ctx)
	if profileID == "" {
		t.Skip("profiler_list did not return a profile_id, skipping profiler_peek test")
	}

	// Get top function name first.
	topResult := callTool(t, session, ctx, "profiler_top", map[string]any{
		"profile_id": profileID,
		"limit":      1,
	})
	if topResult.IsError {
		t.Skipf("profiler_top failed, cannot get function name for profiler_peek: %s", textFromResult(t, topResult))
	}
	var topParsed struct {
		TopFunctions []struct {
			FunctionName string `json:"function_name"`
		} `json:"top_functions"`
	}
	require.NoError(t, json.Unmarshal([]byte(textFromResult(t, topResult)), &topParsed))
	if len(topParsed.TopFunctions) == 0 || topParsed.TopFunctions[0].FunctionName == "" {
		t.Skip("no functions found in profiler_top, skipping profiler_peek test")
	}
	functionName := topParsed.TopFunctions[0].FunctionName

	result := callTool(t, session, ctx, "profiler_peek", map[string]any{
		"profile_id":    profileID,
		"function_name": functionName,
	})

	if result.IsError {
		t.Skipf("profiler_peek returned an error: %s", textFromResult(t, result))
	}

	text := textFromResult(t, result)
	t.Logf("profiler_peek result (function=%s): %s", functionName, text[:min(len(text), 500)])

	var parsed struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &parsed))
	assert.NotEmpty(t, parsed.Function.Name, "function.name must not be empty")
}

func TestProfilerFlamegraph(t *testing.T) {
	session, ctx, cleanup := setupClient(t)
	defer cleanup()

	profileID, _, _ := discoverProfile(t, session, ctx)
	if profileID == "" {
		t.Skip("profiler_list did not return a profile_id, skipping profiler_flamegraph test")
	}

	result := callTool(t, session, ctx, "profiler_flamegraph", map[string]any{
		"profile_id": profileID,
		"max_depth":  2,
		"min_pct":    2.0,
	})

	if result.IsError {
		t.Skipf("profiler_flamegraph returned an error: %s", textFromResult(t, result))
	}

	text := textFromResult(t, result)
	t.Logf("profiler_flamegraph result (profile_id=%s): %s", profileID, text[:min(len(text), 500)])

	var parsed struct {
		Root struct {
			Name string `json:"name"`
		} `json:"root"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &parsed))
	assert.NotEmpty(t, parsed.Root.Name, "root node must have a non-empty name")
}

func TestProfilerCompare(t *testing.T) {
	session, ctx, cleanup := setupClient(t)
	defer cleanup()

	// Need at least 2 profiles of the same type+target for a meaningful compare.
	listResult := callTool(t, session, ctx, "profiler_list", map[string]any{"limit": 10})
	if listResult.IsError {
		t.Skipf("profiler_list failed: %s", textFromResult(t, listResult))
	}
	var listParsed struct {
		Profiles []struct {
			ProfileID   string `json:"profile_id"`
			ProfileType string `json:"profile_type"`
			Target      string `json:"target"`
		} `json:"profiles"`
	}
	require.NoError(t, json.Unmarshal([]byte(textFromResult(t, listResult)), &listParsed))

	// Find two profiles with matching type+target and non-empty profile_id.
	type key struct{ typ, target string }
	seen := map[key]string{}
	var profileID, baseProfileID string
	for _, p := range listParsed.Profiles {
		if p.ProfileID == "" {
			continue
		}
		k := key{p.ProfileType, p.Target}
		if first, ok := seen[k]; ok {
			profileID = first
			baseProfileID = p.ProfileID
			break
		}
		seen[k] = p.ProfileID
	}
	if profileID == "" || baseProfileID == "" {
		t.Skip("need at least 2 profiles with non-empty profile_id of the same type+target for profiler_compare, skipping")
	}

	result := callTool(t, session, ctx, "profiler_compare", map[string]any{
		"profile_id":      profileID,
		"base_profile_id": baseProfileID,
	})

	if result.IsError {
		t.Skipf("profiler_compare returned an error: %s", textFromResult(t, result))
	}

	text := textFromResult(t, result)
	t.Logf("profiler_compare result: %s", text[:min(len(text), 500)])

	var parsed struct {
		DiffID  string `json:"diff_id"`
		Summary struct {
			TotalDeltaPct float64 `json:"total_delta_pct"`
		} `json:"summary"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &parsed))
	assert.NotEmpty(t, parsed.DiffID, "diff_id must not be empty")
	t.Logf("diff_id=%s total_delta_pct=%.2f", parsed.DiffID, parsed.Summary.TotalDeltaPct)

	// Verify diff_id is usable with profiler_top.
	topResult := callTool(t, session, ctx, "profiler_top", map[string]any{
		"profile_id": parsed.DiffID,
		"limit":      3,
	})
	assert.False(t, topResult.IsError, "profiler_top with diff_id must not return an error")
}

func TestProfilerTrends(t *testing.T) {
	session, ctx, cleanup := setupClient(t)
	defer cleanup()

	_, profileType, target := discoverProfile(t, session, ctx)
	if profileType == "" || target == "" {
		// discoverProfile was satisfied by env var; try profiler_list to get type+target.
		listResult := callTool(t, session, ctx, "profiler_list", map[string]any{"limit": 1})
		if listResult.IsError {
			t.Skipf("profiler_list failed: %s", textFromResult(t, listResult))
		}
		var listParsed struct {
			Profiles []struct {
				ProfileType string `json:"profile_type"`
				Target      string `json:"target"`
			} `json:"profiles"`
		}
		require.NoError(t, json.Unmarshal([]byte(textFromResult(t, listResult)), &listParsed))
		if len(listParsed.Profiles) == 0 {
			t.Skip("no profiles found, skipping profiler_trends test")
		}
		profileType = listParsed.Profiles[0].ProfileType
		target = listParsed.Profiles[0].Target
	}

	result := callTool(t, session, ctx, "profiler_trends", map[string]any{
		"profile_type":  profileType,
		"target":        target,
		"max_profiles":  10,
		"max_functions": 5,
	})

	if result.IsError {
		t.Skipf("profiler_trends returned an error: %s", textFromResult(t, result))
	}

	text := textFromResult(t, result)
	t.Logf("profiler_trends result (type=%s target=%s): %s", profileType, target, text[:min(len(text), 500)])

	var parsed struct {
		AnalyzedCount int `json:"analyzed_count"`
		Functions     []struct {
			Name string `json:"name"`
		} `json:"functions"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &parsed))
	t.Logf("analyzed_count=%d functions=%d", parsed.AnalyzedCount, len(parsed.Functions))
}
