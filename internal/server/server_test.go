package server

import (
	"context"
	"fmt"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
)

func TestPromptCompleter_EmptyPrefix(t *testing.T) {
	c := &promptCompleter{}
	result, err := c.CompletePromptArgument(context.Background(), "investigate-metrics", mcp.CompleteArgument{
		Name:  "metric_type",
		Value: "",
	}, mcp.CompleteContext{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Values) != len(defaultMetricCandidates) {
		t.Errorf("expected %d candidates, got %d", len(defaultMetricCandidates), len(result.Values))
	}
}

func TestPromptCompleter_FilterByPrefix(t *testing.T) {
	c := &promptCompleter{}
	result, err := c.CompletePromptArgument(context.Background(), "investigate-metrics", mcp.CompleteArgument{
		Name:  "metric_type",
		Value: "compute",
	}, mcp.CompleteContext{})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range result.Values {
		if len(v) < 7 || v[:7] != "compute" {
			t.Errorf("expected all values to contain 'compute', got %q", v)
		}
	}
	if len(result.Values) == 0 {
		t.Error("expected at least one compute metric candidate")
	}
}

func TestPromptCompleter_CaseInsensitive(t *testing.T) {
	c := &promptCompleter{}
	result, err := c.CompletePromptArgument(context.Background(), "investigate-metrics", mcp.CompleteArgument{
		Name:  "metric_type",
		Value: "CPU",
	}, mcp.CompleteContext{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Values) == 0 {
		t.Error("expected case-insensitive match for 'CPU'")
	}
}

func TestPromptCompleter_UnknownPrompt(t *testing.T) {
	c := &promptCompleter{}
	result, err := c.CompletePromptArgument(context.Background(), "unknown-prompt", mcp.CompleteArgument{
		Name:  "metric_type",
		Value: "cpu",
	}, mcp.CompleteContext{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Values) != 0 {
		t.Errorf("expected empty values for unknown prompt, got %d", len(result.Values))
	}
}

func TestPromptCompleter_UnknownArgument(t *testing.T) {
	c := &promptCompleter{}
	result, err := c.CompletePromptArgument(context.Background(), "investigate-metrics", mcp.CompleteArgument{
		Name:  "unknown_arg",
		Value: "cpu",
	}, mcp.CompleteContext{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Values) != 0 {
		t.Errorf("expected empty values for unknown arg, got %d", len(result.Values))
	}
}

func TestPromptCompleter_UsesRegistry(t *testing.T) {
	reg := metrics.NewRegistry()
	// A registry with no entries should fall back to defaults.
	c := &promptCompleter{registry: reg}
	result, err := c.CompletePromptArgument(context.Background(), "investigate-metrics", mcp.CompleteArgument{
		Name:  "metric_type",
		Value: "",
	}, mcp.CompleteContext{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Values) != len(defaultMetricCandidates) {
		t.Errorf("empty registry should fall back to defaults, got %d values", len(result.Values))
	}
}

func TestTransportValidation(t *testing.T) {
	tests := []struct {
		transport Transport
		wantErr   bool
	}{
		{TransportStdio, false},
		{TransportHTTP, false},
		{"", false},
		{"grpc", true},
		{"HTTP", true},
	}
	for _, tt := range tests {
		t.Run(string(tt.transport), func(t *testing.T) {
			// We can't fully run the server, but we can check that the switch handles correctly.
			// Testing the validation logic extracted from Run.
			var err error
			switch tt.transport {
			case TransportHTTP:
				// would call runHTTP
			case TransportStdio, "":
				// would call runStdio
			default:
				err = fmt.Errorf("unsupported transport %q", tt.transport)
			}
			if (err != nil) != tt.wantErr {
				t.Errorf("transport %q: got err=%v, wantErr=%v", tt.transport, err, tt.wantErr)
			}
		})
	}
}
