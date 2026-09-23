package tools

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestQueryWarningMessages(t *testing.T) {
	const metricType = "custom.googleapis.com/players_count"
	const window = "current"

	t.Run("Edge: zero warnings → empty slice", func(t *testing.T) {
		got := queryWarningMessages(metricType, window, gcpdata.QueryWarnings{})
		assert.Nil(t, got)
	})

	t.Run("Positive: SingleGroup-only emits one message naming the metric, window, and group count", func(t *testing.T) {
		got := queryWarningMessages(metricType, window, gcpdata.QueryWarnings{
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
		got := queryWarningMessages(metricType, "baseline", gcpdata.QueryWarnings{
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
		got := queryWarningMessages(metricType, window, gcpdata.QueryWarnings{
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
		got := queryWarningMessages(metricType, window, gcpdata.QueryWarnings{
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

	t.Run("Positive: UnsupportedPoints emits one message with the count", func(t *testing.T) {
		got := queryWarningMessages(metricType, window, gcpdata.QueryWarnings{UnsupportedPoints: 4})
		require.Len(t, got, 1)
		assert.Contains(t, got[0], "dropped 4 point(s) with unsupported")
		assert.Contains(t, got[0], window)
	})

	t.Run("Edge: zero counters with TotalBuckets>0 does NOT emit ragged warning", func(t *testing.T) {
		got := queryWarningMessages(metricType, window, gcpdata.QueryWarnings{
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

func TestRequireBackendGuards(t *testing.T) {
	cases := map[string]func(){
		"logs":     func() { requireLogs(nil) },
		"errors":   func() { requireErrors(nil) },
		"traces":   func() { requireTraces(nil) },
		"profiler": func() { requireProfiler(nil) },
		"querier":  func() { requireQuerier(nil) },
		"registry": func() { requireRegistry(nil) },
	}
	for name, fn := range cases {
		t.Run(name+" nil panics", func(t *testing.T) {
			defer func() {
				assert.NotNil(t, recover())
			}()
			fn()
		})
	}
}

func TestProjectPolicy(t *testing.T) {
	pinned := MustProjectPolicy("default-project")
	project, err := pinned.Resolve("")
	require.NoError(t, err)
	assert.Equal(t, "default-project", project)
	_, err = pinned.Resolve("other-project")
	assert.Error(t, err)
	unpinned := MustProjectPolicy("")
	project, err = unpinned.Resolve("request-project")
	require.NoError(t, err)
	assert.Equal(t, "request-project", project)
	_, err = unpinned.Resolve("")
	assert.Error(t, err)
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

	t.Run("start_time keeps sub-second precision and offset", func(t *testing.T) {
		filter, err := buildTimeFilter(TimeFilterInput{StartTime: "2025-01-15T00:00:00.5+02:00"})
		require.NoError(t, err)
		assert.Equal(t, `timestamp>="2025-01-15T00:00:00.5+02:00"`, filter)
	})

	t.Run("invalid start_time", func(t *testing.T) {
		_, err := buildTimeFilter(TimeFilterInput{StartTime: "not-a-date"})
		assert.ErrorContains(t, err, `invalid start_time "not-a-date": must be RFC3339 format`)
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

func TestParseTimeRange(t *testing.T) {
	t.Run("both empty defaults to last 1 hour", func(t *testing.T) {
		before := time.Now().UTC()
		start, end, err := parseTimeRange("", "", time.Hour)
		after := time.Now().UTC()

		require.NoError(t, err)
		assert.True(t, end.After(before.Add(-time.Second)))
		assert.True(t, end.Before(after.Add(time.Second)))
		assert.True(t, start.Before(end))

		diff := end.Sub(start)
		assert.InDelta(t, time.Hour.Seconds(), diff.Seconds(), 1.0)
	})

	t.Run("both specified", func(t *testing.T) {
		start, end, err := parseTimeRange("2025-01-15T10:00:00Z", "2025-01-15T11:00:00Z", time.Hour)
		require.NoError(t, err)
		assert.Equal(t, "2025-01-15T10:00:00Z", start.Format(time.RFC3339))
		assert.Equal(t, "2025-01-15T11:00:00Z", end.Format(time.RFC3339))
	})

	t.Run("only end_time", func(t *testing.T) {
		start, end, err := parseTimeRange("", "2025-01-15T12:00:00Z", time.Hour)
		require.NoError(t, err)
		assert.Equal(t, "2025-01-15T12:00:00Z", end.Format(time.RFC3339))
		assert.Equal(t, "2025-01-15T11:00:00Z", start.Format(time.RFC3339))
	})

	t.Run("only start_time", func(t *testing.T) {
		start, end, err := parseTimeRange("2025-01-15T10:00:00Z", "", time.Hour)
		require.NoError(t, err)
		assert.Equal(t, "2025-01-15T10:00:00Z", start.Format(time.RFC3339))
		assert.True(t, end.After(start))
	})

	t.Run("end before start", func(t *testing.T) {
		_, _, err := parseTimeRange("2025-01-15T12:00:00Z", "2025-01-15T10:00:00Z", time.Hour)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "end_time must be after start_time")
	})

	t.Run("invalid start_time format", func(t *testing.T) {
		_, _, err := parseTimeRange("not-a-date", "", time.Hour)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "invalid start_time")
	})

	t.Run("future start_time without end_time", func(t *testing.T) {
		_, _, err := parseTimeRange(time.Now().Add(time.Hour).Format(time.RFC3339), "", time.Hour)
		assert.ErrorContains(t, err, "end_time must be after start_time")
	})

	t.Run("invalid end_time format", func(t *testing.T) {
		_, _, err := parseTimeRange("", "not-a-date", time.Hour)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "invalid end_time")
	})
}

func TestQueryWarningMessagesTruncation(t *testing.T) {
	got := queryWarningMessages("custom.googleapis.com/foo", "current", gcpdata.QueryWarnings{
		TruncatedSeries: true,
	})
	require.Len(t, got, 1)
	assert.Contains(t, got[0], "truncated")
	assert.Contains(t, got[0], "500")
}

func TestGCPErrorResult(t *testing.T) {
	notFound := codeGuidance{codes.NotFound, "The trace does not exist."}
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"tool code guidance", status.Error(codes.NotFound, "missing"), "Failed: rpc error: code = NotFound desc = missing. The trace does not exist."},
		{"unauthenticated", status.Error(codes.Unauthenticated, "token expired"), "re-connect this MCP server"},
		{"wrapped permission denied", fmt.Errorf("listing: %w", status.Error(codes.PermissionDenied, "denied")), "IAM permission"},
		{"unavailable", status.Error(codes.Unavailable, "down"), "temporarily unavailable"},
		{"rate limited", status.Error(codes.ResourceExhausted, "quota"), "rate-limited"},
		{"context deadline", fmt.Errorf("query: %w", context.DeadlineExceeded), "did not respond in time"},
		{"context canceled", context.Canceled, "was canceled"},
		{"fallback", errors.New("boom"), "Failed: boom. Verify the project_id."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := gcpErrorResult("Failed: "+tt.err.Error(), tt.err, "Verify the project_id.", notFound)
			require.True(t, res.IsError)
			assert.Contains(t, res.Content[0].(*mcp.TextContent).Text, tt.want)
		})
	}
	t.Run("no guidance", func(t *testing.T) {
		res := gcpErrorResult("Failed: boom", errors.New("boom"), "")
		assert.Equal(t, "Failed: boom", res.Content[0].(*mcp.TextContent).Text)
	})
	t.Run("tool guidance overrides shared guidance for the same code", func(t *testing.T) {
		own := codeGuidance{codes.DeadlineExceeded, "Lower max_profiles."}
		res := gcpErrorResult("Failed", status.Error(codes.DeadlineExceeded, "slow"), "", own)
		assert.Equal(t, "Failed. Lower max_profiles.", res.Content[0].(*mcp.TextContent).Text)
	})
}

func TestGCPErrorsResult(t *testing.T) {
	denied := status.Error(codes.PermissionDenied, "denied")
	unavailable := status.Error(codes.Unavailable, "down")
	exhausted := status.Error(codes.ResourceExhausted, "quota")
	for _, tc := range []struct {
		name     string
		errs     []error
		fallback string
		want     string
	}{
		{"one guidance per distinct code, in order", []error{denied, nil, unavailable, denied}, "",
			"Failed. " + sharedCodeGuidance[codes.PermissionDenied] + " " + sharedCodeGuidance[codes.Unavailable]},
		{"codes sharing guidance give it once", []error{unavailable, exhausted}, "", "Failed. " + sharedCodeGuidance[codes.Unavailable]},
		{"fallback for codes without guidance", []error{errors.New("boom"), denied}, "Retry.",
			"Failed. Retry. " + sharedCodeGuidance[codes.PermissionDenied]},
		{"no errors", []error{nil, nil}, "Retry.", "Failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := gcpErrorsResult("Failed", tc.errs, tc.fallback)
			assert.Equal(t, tc.want, res.Content[0].(*mcp.TextContent).Text)
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

func TestStartProgressHeartbeat_NoToken(t *testing.T) {
	// With no request (hence no progress token), the heartbeat must be an inert
	// no-op: it spawns no goroutine and its stop function returns immediately
	// without panicking or blocking.
	stop := startProgressHeartbeat(context.Background(), nil, "scanning…")
	require.NotNil(t, stop)

	done := make(chan struct{})
	go func() {
		stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stop() blocked for a no-token heartbeat")
	}

	// Calling stop again must stay safe.
	assert.NotPanics(t, stop)
}

func TestJoinNote(t *testing.T) {
	assert.Empty(t, joinNote())
	assert.Empty(t, joinNote("", ""))
	assert.Equal(t, "a b", joinNote("", "a", "", "b"))
}

func TestRunParallel(t *testing.T) {
	t.Run("collects errors by index and recovers panics", func(t *testing.T) {
		boom := errors.New("boom")
		errs := runParallel(context.Background(), "test", 3, 2, func(i int) error {
			switch i {
			case 1:
				return boom
			case 2:
				panic("bug")
			}
			return nil
		})
		require.Len(t, errs, 3)
		assert.NoError(t, errs[0])
		assert.ErrorIs(t, errs[1], boom)
		var pe *panicError
		require.ErrorAs(t, errs[2], &pe)
		assert.Equal(t, "bug", pe.value)
		assert.True(t, isPanic(errs[2]))
		assert.False(t, isPanic(errs[1]))
	})

	t.Run("respects the concurrency limit", func(t *testing.T) {
		var running, peak atomic.Int32
		runParallel(context.Background(), "test", 8, 2, func(int) error {
			n := running.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			running.Add(-1)
			return nil
		})
		assert.LessOrEqual(t, peak.Load(), int32(2))
	})

	t.Run("does not start tasks after cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var calls atomic.Int32
		errs := runParallel(ctx, "test", 2, 0, func(int) error {
			calls.Add(1)
			return nil
		})
		assert.Zero(t, calls.Load())
		for _, err := range errs {
			assert.ErrorIs(t, err, context.Canceled)
		}
	})
}
