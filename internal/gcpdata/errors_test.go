package gcpdata

import (
	"testing"

	"cloud.google.com/go/errorreporting/apiv1beta1/errorreportingpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTimeRangePeriod(t *testing.T) {
	tests := []struct {
		name  string
		hours int
		want  errorreportingpb.QueryTimeRange_Period
	}{
		{"1 hour", 1, errorreportingpb.QueryTimeRange_PERIOD_1_HOUR},
		{"0 hours", 0, errorreportingpb.QueryTimeRange_PERIOD_1_HOUR},
		{"2 hours rounds to 6h", 2, errorreportingpb.QueryTimeRange_PERIOD_6_HOURS},
		{"6 hours exact", 6, errorreportingpb.QueryTimeRange_PERIOD_6_HOURS},
		{"7 hours rounds to 1d", 7, errorreportingpb.QueryTimeRange_PERIOD_1_DAY},
		{"24 hours exact", 24, errorreportingpb.QueryTimeRange_PERIOD_1_DAY},
		{"25 hours rounds to 1w", 25, errorreportingpb.QueryTimeRange_PERIOD_1_WEEK},
		{"168 hours (1 week) exact", 168, errorreportingpb.QueryTimeRange_PERIOD_1_WEEK},
		{"169 hours rounds to 30d", 169, errorreportingpb.QueryTimeRange_PERIOD_30_DAYS},
		{"720 hours (30 days)", 720, errorreportingpb.QueryTimeRange_PERIOD_30_DAYS},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := timeRangePeriod(tt.hours)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestConvertErrorContext(t *testing.T) {
	ctx := &errorreportingpb.ErrorContext{
		User: "user-1",
		HttpRequest: &errorreportingpb.HttpRequestContext{
			Method:             "GET",
			Url:                "https://example.com/foo",
			ResponseStatusCode: 500,
			RemoteIp:           "127.0.0.1",
		},
		ReportLocation: &errorreportingpb.SourceLocation{
			FilePath:     "main.go",
			LineNumber:   42,
			FunctionName: "main.handle",
		},
	}
	got := convertErrorContext(ctx)
	require.NotNil(t, got)
	assert.Equal(t, "user-1", got.User)
	require.NotNil(t, got.HTTPRequest)
	assert.Equal(t, "GET", got.HTTPRequest.Method)
	require.NotNil(t, got.ReportLocation)
	assert.Equal(t, "main.handle", got.ReportLocation.FunctionName)
}

func TestSplitReportedErrorMessage(t *testing.T) {
	t.Run("single line", func(t *testing.T) {
		headline, stack := splitReportedErrorMessage("boom")
		assert.Equal(t, "boom", headline)
		assert.Empty(t, stack)
	})

	t.Run("multiline stack trace", func(t *testing.T) {
		msg := "panic: boom\nmain.main()\n\t/app/main.go:42"
		headline, stack := splitReportedErrorMessage(msg)
		assert.Equal(t, "panic: boom", headline)
		assert.Equal(t, msg, stack)
	})
}

func TestConvertErrorContext_EmptyCases(t *testing.T) {
	t.Run("empty context returns nil", func(t *testing.T) {
		got := convertErrorContext(&errorreportingpb.ErrorContext{})
		assert.Nil(t, got)
	})

	t.Run("only user populated", func(t *testing.T) {
		ctx := &errorreportingpb.ErrorContext{User: "bob"}
		got := convertErrorContext(ctx)
		require.NotNil(t, got)
		assert.Equal(t, "bob", got.User)
		assert.Nil(t, got.HTTPRequest)
		assert.Nil(t, got.ReportLocation)
	})

	t.Run("only http request populated", func(t *testing.T) {
		ctx := &errorreportingpb.ErrorContext{
			HttpRequest: &errorreportingpb.HttpRequestContext{
				Method: "POST",
			},
		}
		got := convertErrorContext(ctx)
		require.NotNil(t, got)
		assert.Empty(t, got.User)
		assert.NotNil(t, got.HTTPRequest)
		assert.Equal(t, "POST", got.HTTPRequest.Method)
		assert.Nil(t, got.ReportLocation)
	})

	t.Run("only report location populated", func(t *testing.T) {
		ctx := &errorreportingpb.ErrorContext{
			ReportLocation: &errorreportingpb.SourceLocation{
				FunctionName: "foo",
			},
		}
		got := convertErrorContext(ctx)
		require.NotNil(t, got)
		assert.Empty(t, got.User)
		assert.Nil(t, got.HTTPRequest)
		assert.NotNil(t, got.ReportLocation)
		assert.Equal(t, "foo", got.ReportLocation.FunctionName)
	})
}
