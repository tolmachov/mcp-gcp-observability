package tools

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpclient"
	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestAggregationWarningMessages(t *testing.T) {
	const metricType = "custom.googleapis.com/players_count"
	const window = "current"

	t.Run("Edge: zero warnings → empty slice", func(t *testing.T) {
		got := aggregationWarningMessages(metricType, window, gcpdata.AggregationWarnings{})
		assert.Nil(t, got)
	})

	t.Run("Positive: SingleGroup-only emits one message naming the metric, window, and group count", func(t *testing.T) {
		got := aggregationWarningMessages(metricType, window, gcpdata.AggregationWarnings{
			SingleGroup: true,
			GroupCount:  1,
		})
		require.Len(t, got, 1)
		msg := got[0]
		assert.Contains(t, msg, metricType)
		assert.Contains(t, msg, window)
		assert.Contains(t, msg, "1 group")
		assert.Contains(t, msg, "group_by")
	})

	t.Run("Positive: CarryForwardBuckets-only emits one message with the ratio", func(t *testing.T) {
		got := aggregationWarningMessages(metricType, "baseline", gcpdata.AggregationWarnings{
			CarryForwardBuckets: 7,
			TotalBuckets:        20,
		})
		require.Len(t, got, 1)
		msg := got[0]
		assert.Contains(t, msg, "7 of 20")
		assert.Contains(t, msg, "baseline")
		assert.Contains(t, msg, "carry-forward")
	})

	t.Run("Positive: DepartedGroupBuckets emits a message naming distinct departed series", func(t *testing.T) {
		got := aggregationWarningMessages(metricType, window, gcpdata.AggregationWarnings{
			DepartedGroupBuckets: 4,
			DepartedSeries:       2,
			TotalBuckets:         60,
		})
		require.Len(t, got, 1)
		msg := got[0]
		assert.Contains(t, msg, "4 of 60")
		assert.Contains(t, msg, "2 distinct group series departed")
	})

	t.Run("Positive: SingleGroup + DepartedGroup + CarryForward → three messages, departed before carry-forward", func(t *testing.T) {
		got := aggregationWarningMessages(metricType, window, gcpdata.AggregationWarnings{
			SingleGroup:          true,
			GroupCount:           1,
			DepartedGroupBuckets: 2,
			DepartedSeries:       1,
			CarryForwardBuckets:  3,
			TotalBuckets:         10,
		})
		require.Len(t, got, 3)
		assert.Contains(t, got[0], "two-stage aggregation")
		assert.Contains(t, got[1], "departed")
		assert.Contains(t, got[2], "carry-forward")
		assert.NotContains(t, got[2], "departed")
	})

	t.Run("Edge: zero counters with TotalBuckets>0 does NOT emit ragged warning", func(t *testing.T) {
		got := aggregationWarningMessages(metricType, window, gcpdata.AggregationWarnings{
			TotalBuckets: 60,
		})
		assert.Nil(t, got)
	})
}

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
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestRequireClient(t *testing.T) {
	t.Run("nil panics", func(t *testing.T) {
		defer func() {
			assert.NotNil(t, recover())
		}()
		requireClient(nil)
	})
}

func TestResolveProject(t *testing.T) {
	tests := []struct {
		name           string
		projectID      string
		defaultProject string
		wantProject    string
		wantErr        bool
	}{
		{"from request", "req-project", "default-project", "req-project", false},
		{"from default", "", "default-project", "default-project", false},
		{"both empty", "", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			project, err := resolveProject(tt.projectID, tt.defaultProject)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantProject, project)
		})
	}
}

func TestBuildTimeFilter(t *testing.T) {
	t.Run("both empty defaults to 24h", func(t *testing.T) {
		filter, err := buildTimeFilter(TimeFilterInput{})
		require.NoError(t, err)
		assert.Contains(t, filter, `timestamp>="`)
	})

	t.Run("start_time only", func(t *testing.T) {
		filter, err := buildTimeFilter(TimeFilterInput{StartTime: "2025-01-15T00:00:00Z"})
		require.NoError(t, err)
		assert.Equal(t, `timestamp>="2025-01-15T00:00:00Z"`, filter)
	})

	t.Run("end_time only defaults start to 24h before end", func(t *testing.T) {
		filter, err := buildTimeFilter(TimeFilterInput{EndTime: "2025-01-15T23:59:59Z"})
		require.NoError(t, err)
		assert.Contains(t, filter, `timestamp>="2025-01-14T23:59:59Z"`)
		assert.Contains(t, filter, `timestamp<="2025-01-15T23:59:59Z"`)
	})

	t.Run("both set", func(t *testing.T) {
		filter, err := buildTimeFilter(TimeFilterInput{
			StartTime: "2025-01-15T00:00:00Z",
			EndTime:   "2025-01-15T23:59:59Z",
		})
		require.NoError(t, err)
		want := "timestamp>=\"2025-01-15T00:00:00Z\"\ntimestamp<=\"2025-01-15T23:59:59Z\""
		assert.Equal(t, want, filter)
	})

	t.Run("invalid start_time", func(t *testing.T) {
		_, err := buildTimeFilter(TimeFilterInput{StartTime: "not-a-date"})
		assert.Error(t, err)
	})

	t.Run("invalid end_time", func(t *testing.T) {
		_, err := buildTimeFilter(TimeFilterInput{EndTime: "not-a-date"})
		assert.Error(t, err)
	})

	t.Run("end_time before start_time returns error", func(t *testing.T) {
		_, err := buildTimeFilter(TimeFilterInput{
			StartTime: "2025-01-15T23:00:00Z",
			EndTime:   "2025-01-15T00:00:00Z",
		})
		assert.Error(t, err)
	})

	t.Run("equal start_time and end_time returns error", func(t *testing.T) {
		_, err := buildTimeFilter(TimeFilterInput{
			StartTime: "2025-01-15T12:00:00Z",
			EndTime:   "2025-01-15T12:00:00Z",
		})
		assert.Error(t, err)
	})
}

func TestResolveErrorsTimeRange(t *testing.T) {
	tests := []struct {
		name    string
		input   ErrorsListInput
		wantErr bool
		want    int
	}{
		{"default 24h", ErrorsListInput{}, false, 24},
		{"time_range_hours explicit", ErrorsListInput{TimeRangeHours: 48}, false, 48},
		{"time_range_hours at max", ErrorsListInput{TimeRangeHours: 720}, false, 720},
		{"time_range_hours too small — negative", ErrorsListInput{TimeRangeHours: -1}, true, 0},
		{"time_range_hours too large", ErrorsListInput{TimeRangeHours: 721}, true, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveErrorsTimeRange(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			if tt.want != 0 {
				assert.Equal(t, tt.want, got)
			}
			assert.GreaterOrEqual(t, got, 1)
		})
	}
}

func TestAggregationWarningMessagesTruncation(t *testing.T) {
	got := aggregationWarningMessages("custom.googleapis.com/foo", "current", gcpdata.AggregationWarnings{
		TruncatedSeries: true,
	})
	require.Len(t, got, 1)
	assert.Contains(t, got[0], "truncated")
	assert.Contains(t, got[0], "500")
}

func TestFormatTraceGetError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"invalid argument", status.Error(codes.InvalidArgument, "bad trace id"), "32-character hex"},
		{"not found", status.Error(codes.NotFound, "not found"), "does not exist"},
		{"permission denied", status.Error(codes.PermissionDenied, "denied"), "credentials"},
		{"unavailable", status.Error(codes.Unavailable, "down"), "temporarily unavailable"},
		{"deadline", context.DeadlineExceeded, "did not respond in time"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatTraceGetError("abc", tt.err)
			assert.Contains(t, got, tt.want)
		})
	}
}

func TestCompactDesc(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "multiple sentences: returns first sentence only",
			input: "First sentence. Second sentence. Third sentence.",
			want:  "First sentence.",
		},
		{
			name:  "single sentence with trailing period",
			input: "Only sentence.",
			want:  "Only sentence.",
		},
		{
			name:  "no period at all",
			input: "No period here",
			want:  "No period here",
		},
		{
			name:  "sentence ending with period-space",
			input: "First. Second.",
			want:  "First.",
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
		{
			// abbreviations like "e.g." contain ". " — the splitter cuts there.
			// This test documents the current behavior so any future change is explicit.
			name:  "abbreviation mid-sentence cuts at abbreviation period",
			input: "Fetches logs (e.g. ERROR level). Returns up to 1000 entries.",
			want:  "Fetches logs (e.g.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := compactDesc(tt.input)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestApplyMode(t *testing.T) {
	full := "First sentence. Second sentence. Third sentence."

	t.Run("ModeStandard returns full description", func(t *testing.T) {
		assert.Equal(t, full, applyMode(ModeStandard, full))
	})

	t.Run("ModeCompact returns first sentence only", func(t *testing.T) {
		assert.Equal(t, "First sentence.", applyMode(ModeCompact, full))
	})

	// Guards against future RegistrationMode additions that forget to extend
	// applyMode's switch — silent fallback to "full" would ship the wrong
	// description; a panic surfaces the bug at startup.
	t.Run("unknown mode panics", func(t *testing.T) {
		assert.PanicsWithValue(t, "unknown RegistrationMode 99", func() {
			applyMode(RegistrationMode(99), full)
		})
	})
}

func TestRegistrationModeString(t *testing.T) {
	assert.Equal(t, "standard", ModeStandard.String())
	assert.Equal(t, "compact", ModeCompact.String())
	assert.Equal(t, "RegistrationMode(99)", RegistrationMode(99).String())
}

// TestRegisterCoreToolCount pins CoreToolsCount against the tools that
// RegisterCore actually registers. The "monitoring" variant Description
// interpolates CoreToolsCount, so this test is the choke point that keeps
// the constant honest. Expected tools: logs_summary, logs_services,
// errors_list, errors_get, metrics_snapshot, metrics_top_contributors,
// trace_list, trace_get, profiler_list, profiler_top.
func TestRegisterCoreToolCount(t *testing.T) {
	ts := newTestToolServer(t)
	// Use non-nil client (required by requireClient at registration time).
	// Querier and ProfileCache can be nil — they are only accessed inside handlers.
	RegisterCore(ts.server, Deps{
		Client:         &gcpclient.Client{},
		Registry:       metrics.NewRegistry(),
		DefaultProject: "test-project",
		Mode:           ModeStandard,
	})

	ctx := context.Background()
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.session.ListTools(ctx, nil)
	require.NoError(t, err)
	assert.Len(t, result.Tools, CoreToolsCount,
		"RegisterCore registered %d tools; update CoreToolsCount if the change is intentional", len(result.Tools))

	wantTools := []string{
		"logs_summary", "logs_services",
		"errors_list", "errors_get",
		"metrics_snapshot", "metrics_top_contributors",
		"trace_list", "trace_get",
		"profiler_list", "profiler_top",
	}
	var gotNames []string
	for _, tool := range result.Tools {
		gotNames = append(gotNames, tool.Name)
	}
	assert.ElementsMatch(t, wantTools, gotNames)
}
