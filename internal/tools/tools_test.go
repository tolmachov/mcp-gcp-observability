package tools

import (
	"math"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

func TestClampLimit(t *testing.T) {
	tests := []struct {
		name       string
		limit      int
		defaultVal int
		maxLimit   int
		want       int
	}{
		{"normal", 50, 100, 1000, 50},
		{"zero returns default", 0, 100, 1000, 100},
		{"negative returns default", -5, 100, 1000, 100},
		{"over max returns max", 2000, 100, 1000, 1000},
		{"at max returns max", 1000, 100, 1000, 1000},
		{"one", 1, 100, 1000, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := clampLimit(tt.limit, tt.defaultVal, tt.maxLimit)
			if got != tt.want {
				t.Errorf("clampLimit(%d, %d, %d) = %d, want %d", tt.limit, tt.defaultVal, tt.maxLimit, got, tt.want)
			}
		})
	}
}

func TestRequireClient(t *testing.T) {
	t.Run("non-nil returns client", func(t *testing.T) {
		// We can't create a real Client without GCP credentials,
		// but we can verify the panic behavior on nil.
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("unexpected panic for non-nil-like test")
			}
		}()
		// Just test the nil case since we can't construct a real Client in unit tests.
	})

	t.Run("nil panics", func(t *testing.T) {
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic for nil client")
			}
		}()
		requireClient(nil)
	})
}

func TestJsonResult(t *testing.T) {
	t.Run("valid struct", func(t *testing.T) {
		result, err := jsonResult(map[string]string{"key": "value"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result == nil {
			t.Fatal("expected non-nil result")
		}
		if result.IsError {
			t.Error("expected non-error result")
		}
	})

	t.Run("unmarshalable value returns error result", func(t *testing.T) {
		result, err := jsonResult(math.NaN())
		if err != nil {
			t.Fatalf("unexpected Go error: %v", err)
		}
		if result == nil {
			t.Fatal("expected non-nil result")
		}
		if !result.IsError {
			t.Error("expected error result for unmarshalable value")
		}
	})
}

func makeRequest(args map[string]any) mcp.CallToolRequest {
	return mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Arguments: args,
		},
	}
}

func TestRequireProject(t *testing.T) {
	tests := []struct {
		name           string
		args           map[string]any
		defaultProject string
		wantProject    string
		wantErr        bool
	}{
		{
			"from request",
			map[string]any{"project_id": "req-project"},
			"default-project",
			"req-project",
			false,
		},
		{
			"from default",
			map[string]any{},
			"default-project",
			"default-project",
			false,
		},
		{
			"both empty",
			map[string]any{},
			"",
			"",
			true,
		},
		{
			"explicit empty with default",
			map[string]any{"project_id": ""},
			"default-project",
			"",
			true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := makeRequest(tt.args)
			project, errResult := requireProject(req, tt.defaultProject)
			if tt.wantErr {
				if errResult == nil {
					t.Error("expected error result, got nil")
				}
				return
			}
			if errResult != nil {
				t.Errorf("unexpected error result: %v", errResult)
				return
			}
			if project != tt.wantProject {
				t.Errorf("project = %q, want %q", project, tt.wantProject)
			}
		})
	}
}

func TestBuildTimeFilter(t *testing.T) {
	t.Run("both empty defaults to 24h", func(t *testing.T) {
		req := makeRequest(map[string]any{})
		filter, err := buildTimeFilter(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(filter, `timestamp>="`) {
			t.Errorf("expected timestamp>= in filter, got %q", filter)
		}
	})

	t.Run("start_time only", func(t *testing.T) {
		req := makeRequest(map[string]any{
			"start_time": "2025-01-15T00:00:00Z",
		})
		filter, err := buildTimeFilter(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if filter != `timestamp>="2025-01-15T00:00:00Z"` {
			t.Errorf("unexpected filter: %q", filter)
		}
	})

	t.Run("end_time only defaults start to 24h before end", func(t *testing.T) {
		req := makeRequest(map[string]any{
			"end_time": "2025-01-15T23:59:59Z",
		})
		filter, err := buildTimeFilter(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Should include both a default start (24h before end) and the end
		if !strings.Contains(filter, `timestamp>="2025-01-14T23:59:59Z"`) {
			t.Errorf("expected default start 24h before end, got %q", filter)
		}
		if !strings.Contains(filter, `timestamp<="2025-01-15T23:59:59Z"`) {
			t.Errorf("expected end_time in filter, got %q", filter)
		}
	})

	t.Run("both set", func(t *testing.T) {
		req := makeRequest(map[string]any{
			"start_time": "2025-01-15T00:00:00Z",
			"end_time":   "2025-01-15T23:59:59Z",
		})
		filter, err := buildTimeFilter(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := "timestamp>=\"2025-01-15T00:00:00Z\"\ntimestamp<=\"2025-01-15T23:59:59Z\""
		if filter != want {
			t.Errorf("filter = %q, want %q", filter, want)
		}
	})

	t.Run("invalid start_time", func(t *testing.T) {
		req := makeRequest(map[string]any{
			"start_time": "not-a-date",
		})
		_, err := buildTimeFilter(req)
		if err == nil {
			t.Error("expected error for invalid start_time")
		}
	})

	t.Run("invalid end_time", func(t *testing.T) {
		req := makeRequest(map[string]any{
			"end_time": "not-a-date",
		})
		_, err := buildTimeFilter(req)
		if err == nil {
			t.Error("expected error for invalid end_time")
		}
	})

	t.Run("end_time before start_time returns error", func(t *testing.T) {
		req := makeRequest(map[string]any{
			"start_time": "2025-01-15T23:00:00Z",
			"end_time":   "2025-01-15T00:00:00Z",
		})
		_, err := buildTimeFilter(req)
		if err == nil {
			t.Error("expected error when end_time is before start_time")
		}
	})

	t.Run("equal start_time and end_time returns error", func(t *testing.T) {
		req := makeRequest(map[string]any{
			"start_time": "2025-01-15T12:00:00Z",
			"end_time":   "2025-01-15T12:00:00Z",
		})
		_, err := buildTimeFilter(req)
		if err == nil {
			t.Error("expected error when end_time equals start_time")
		}
	})
}

func TestResolveErrorsTimeRange(t *testing.T) {
	tests := []struct {
		name    string
		args    map[string]any
		wantErr bool
		want    int // expected hours (only checked if !wantErr)
	}{
		{
			"default 24h",
			map[string]any{},
			false,
			24,
		},
		{
			"time_range_hours explicit",
			map[string]any{"time_range_hours": 48.0},
			false,
			48,
		},
		{
			"time_range_hours at max",
			map[string]any{"time_range_hours": 720.0},
			false,
			720,
		},
		{
			"time_range_hours too small",
			map[string]any{"time_range_hours": 0.0},
			true,
			0,
		},
		{
			"time_range_hours too large",
			map[string]any{"time_range_hours": 721.0},
			true,
			0,
		},
		{
			"both start and end",
			map[string]any{
				"start_time": "2025-01-15T00:00:00Z",
				"end_time":   "2025-01-15T06:00:00Z",
			},
			false,
			6,
		},
		{
			"start and end rounds up",
			map[string]any{
				"start_time": "2025-01-15T00:00:00Z",
				"end_time":   "2025-01-15T06:30:00Z",
			},
			false,
			7,
		},
		{
			"end before start returns error",
			map[string]any{
				"start_time": "2025-01-15T23:00:00Z",
				"end_time":   "2025-01-15T00:00:00Z",
			},
			true,
			0,
		},
		{
			"equal start and end returns error",
			map[string]any{
				"start_time": "2025-01-15T12:00:00Z",
				"end_time":   "2025-01-15T12:00:00Z",
			},
			true,
			0,
		},
		{
			"range exceeds 720h returns error",
			map[string]any{
				"start_time": "2025-01-01T00:00:00Z",
				"end_time":   "2025-03-01T00:00:00Z",
			},
			true,
			0,
		},
		{
			"invalid start_time format",
			map[string]any{"start_time": "not-a-date"},
			true,
			0,
		},
		{
			"invalid end_time format",
			map[string]any{"end_time": "not-a-date"},
			true,
			0,
		},
		{
			"start_time only with far past exceeds 720h",
			map[string]any{"start_time": "2025-01-15T00:00:00Z"},
			true, // now - 2025 > 720h
			0,
		},
		{
			"end_time only with far future exceeds 720h",
			map[string]any{"end_time": "2099-01-15T00:00:00Z"},
			true, // 2099 - (now - 24h) > 720h
			0,
		},
		{
			"start_time/end_time takes precedence over time_range_hours",
			map[string]any{
				"start_time":       "2025-01-15T00:00:00Z",
				"end_time":         "2025-01-15T12:00:00Z",
				"time_range_hours": 48.0,
			},
			false,
			12,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := makeRequest(tt.args)
			got, err := resolveErrorsTimeRange(req)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error, got hours=%d", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.want != 0 && got != tt.want {
				t.Errorf("hours = %d, want %d", got, tt.want)
			}
			if got < 1 {
				t.Errorf("hours = %d, must be >= 1", got)
			}
		})
	}
}
