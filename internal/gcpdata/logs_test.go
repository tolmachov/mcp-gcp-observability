package gcpdata

import (
	"encoding/json"
	"strings"
	"testing"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestBoundLogEntryAlwaysFitsAndReportsTruncation(t *testing.T) {
	huge := strings.Repeat("界", 20_000)
	entry := LogEntry{
		Timestamp: huge, Severity: huge, LogName: huge, InsertID: huge,
		JSONPayload:            map[string]any{"huge": huge},
		PayloadConversionError: huge,
		Resource:               &ResourceInfo{Type: huge, Labels: map[string]string{huge: huge}},
		HTTPRequest:            &HTTPRequestInfo{URL: huge, UserAgent: huge, RemoteIP: huge},
		Labels:                 map[string]string{huge: huge},
	}
	bounded := boundLogEntry(entry)
	encoded, err := json.Marshal(bounded)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(encoded), maxNormalizedLogEntryBytes)
	assert.True(t, bounded.EntryTruncated)
	assert.Positive(t, bounded.OmittedBytes)
	assert.NotEmpty(t, bounded.TruncatedFields)
}

func TestRequestIDFilterIncludesCommonFields(t *testing.T) {
	got := requestIDFilter("abc-123")
	for _, want := range []string{
		`jsonPayload.request_id="abc-123"`,
		`jsonPayload.requestId="abc-123"`,
		`labels.request_id="abc-123"`,
		`labels.requestId="abc-123"`,
	} {
		assert.Contains(t, got, want)
	}
}

func TestExtractRequestID(t *testing.T) {
	t.Run("json payload snake case", func(t *testing.T) {
		entry := &loggingpb.LogEntry{
			Payload: &loggingpb.LogEntry_JsonPayload{
				JsonPayload: &structpb.Struct{Fields: map[string]*structpb.Value{
					"request_id": structpb.NewStringValue("snake"),
				}},
			},
		}
		assert.Equal(t, "snake", extractRequestID(entry))
	})

	t.Run("json payload camel case", func(t *testing.T) {
		entry := &loggingpb.LogEntry{
			Payload: &loggingpb.LogEntry_JsonPayload{
				JsonPayload: &structpb.Struct{Fields: map[string]*structpb.Value{
					"requestId": structpb.NewStringValue("camel"),
				}},
			},
		}
		assert.Equal(t, "camel", extractRequestID(entry))
	})

	t.Run("labels fallback", func(t *testing.T) {
		entry := &loggingpb.LogEntry{
			Labels: map[string]string{"requestId": "label-id"},
		}
		assert.Equal(t, "label-id", extractRequestID(entry))
	})
}

func TestConvertLogEntry_PayloadTypes(t *testing.T) {
	t.Run("text payload passed through", func(t *testing.T) {
		entry := &loggingpb.LogEntry{
			Payload: &loggingpb.LogEntry_TextPayload{
				TextPayload: "plain text message",
			},
		}
		le := convertLogEntry(entry)
		assert.Equal(t, "plain text message", le.TextPayload)
		assert.Nil(t, le.JSONPayload)
		assert.Empty(t, le.PayloadConversionError)
	})

	t.Run("json payload converted to map", func(t *testing.T) {
		entry := &loggingpb.LogEntry{
			Payload: &loggingpb.LogEntry_JsonPayload{
				JsonPayload: &structpb.Struct{
					Fields: map[string]*structpb.Value{
						"field": structpb.NewStringValue("value"),
					},
				},
			},
		}
		le := convertLogEntry(entry)
		assert.Empty(t, le.TextPayload)
		assert.NotNil(t, le.JSONPayload)
		assert.Equal(t, "value", le.JSONPayload["field"])
		assert.Empty(t, le.PayloadConversionError)
	})
}
