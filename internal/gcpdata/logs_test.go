package gcpdata

import (
	"testing"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"github.com/stretchr/testify/assert"
	"google.golang.org/protobuf/types/known/structpb"
)

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
