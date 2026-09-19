package gcpdata

import (
	"testing"
	"time"

	"cloud.google.com/go/errorreporting/apiv1beta1/errorreportingpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestErrorWindowSpec(t *testing.T) {
	tests := []struct {
		window ErrorWindow
		period errorreportingpb.QueryTimeRange_Period
		span   time.Duration
		bucket time.Duration
	}{
		{ErrorWindow1H, errorreportingpb.QueryTimeRange_PERIOD_1_HOUR, time.Hour, 5 * time.Minute},
		{ErrorWindow6H, errorreportingpb.QueryTimeRange_PERIOD_6_HOURS, 6 * time.Hour, 30 * time.Minute},
		{ErrorWindow24H, errorreportingpb.QueryTimeRange_PERIOD_1_DAY, 24 * time.Hour, time.Hour},
		{ErrorWindow7D, errorreportingpb.QueryTimeRange_PERIOD_1_WEEK, 7 * 24 * time.Hour, 6 * time.Hour},
		{ErrorWindow30D, errorreportingpb.QueryTimeRange_PERIOD_30_DAYS, 30 * 24 * time.Hour, 24 * time.Hour},
	}
	for _, tt := range tests {
		t.Run(string(tt.window), func(t *testing.T) {
			got, ok := tt.window.Spec()
			require.True(t, ok)
			assert.Equal(t, tt.period, got.Period)
			assert.Equal(t, tt.span, got.Span)
			assert.Equal(t, tt.bucket, got.Bucket)
		})
	}
	_, ok := ErrorWindow("2h").Spec()
	assert.False(t, ok)
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
