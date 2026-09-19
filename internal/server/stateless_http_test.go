package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type statelessPingInput struct{}
type statelessPingOutput struct {
	Instance string `json:"instance"`
}

func statelessTestHandler(instance string) http.Handler {
	srv := mcp.NewServer(&mcp.Implementation{Name: "stateless-" + instance, Version: "1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "ping"}, func(context.Context, *mcp.CallToolRequest, statelessPingInput) (*mcp.CallToolResult, *statelessPingOutput, error) {
		return nil, &statelessPingOutput{Instance: instance}, nil
	})
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{Stateless: true})
}

func TestStatelessHTTPInitializeAndToolCallCanHitDifferentInstances(t *testing.T) {
	instanceA := statelessTestHandler("a")
	instanceB := statelessTestHandler("b")
	lb := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		var envelope struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &envelope)
		if envelope.Method == "initialize" {
			instanceA.ServeHTTP(w, r)
			return
		}
		instanceB.ServeHTTP(w, r)
	})
	ts := httptest.NewServer(lb)
	t.Cleanup(ts.Close)

	client := mcp.NewClient(&mcp.Implementation{Name: "stateless-client", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             ts.URL,
		DisableStandaloneSSE: true,
	}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "ping", Arguments: map[string]any{}})
	require.NoError(t, err)
	require.False(t, result.IsError)
	encoded, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err)
	var output statelessPingOutput
	require.NoError(t, json.Unmarshal(encoded, &output))
	assert.Equal(t, "b", output.Instance, "the tool call must succeed without affinity to the initialize instance")
}
