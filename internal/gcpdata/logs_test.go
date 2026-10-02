package gcpdata

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/iterator"
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

func TestRequestIDFilterUsesOperationID(t *testing.T) {
	assert.Equal(t, `operation.id="abc-123"`, requestIDFilter("abc-123"))
	assert.Equal(t, `operation.id="a\"b"`, requestIDFilter(`a"b`))
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

// scriptedLogIterator drives the real iterator.PageInfo pager over scripted
// pages; a page with err set fails the fetch the way a canceled RPC does.
type scriptedLogIterator struct {
	pageInfo *iterator.PageInfo
	nextFunc func() error
	items    []*loggingpb.LogEntry
}

type scriptedPage struct {
	entries []*loggingpb.LogEntry
	next    string
	err     error
}

func newScriptedLogIterator(startToken string, pages map[string]scriptedPage) *scriptedLogIterator {
	it := &scriptedLogIterator{}
	fetch := func(_ int, token string) (string, error) {
		p := pages[token]
		if p.err != nil {
			return "", p.err
		}
		it.items = append(it.items, p.entries...)
		return p.next, nil
	}
	it.pageInfo, it.nextFunc = iterator.NewPageInfo(fetch,
		func() int { return len(it.items) },
		func() any { b := it.items; it.items = nil; return b })
	it.pageInfo.Token = startToken
	return it
}

func (it *scriptedLogIterator) Next() (*loggingpb.LogEntry, error) {
	if err := it.nextFunc(); err != nil {
		return nil, err
	}
	e := it.items[0]
	it.items = it.items[1:]
	return e, nil
}

func (it *scriptedLogIterator) PageInfo() *iterator.PageInfo { return it.pageInfo }

func expiredScanBudget(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithDeadlineCause(t.Context(), time.Now().Add(-time.Second), errLogScanBudget)
	t.Cleanup(cancel)
	return ctx
}

func TestCollectLogEntriesResumesAfterScanBudget(t *testing.T) {
	entry := &loggingpb.LogEntry{InsertId: "a"}
	pages := map[string]scriptedPage{
		"":   {next: "p1"}, // empty page, scan goes on
		"p1": {entries: []*loggingpb.LogEntry{entry}, next: "p2"},
		"p2": {err: context.DeadlineExceeded},
	}
	got, err := collectLogEntries(expiredScanBudget(t), newScriptedLogIterator("", pages), "", 10)
	require.NoError(t, err)
	assert.True(t, got.ScanIncomplete)
	assert.Equal(t, 1, got.Count)
	assert.Equal(t, "p2", got.NextPageToken, "resume from the page that was not fetched")
}

func TestCollectLogEntriesEmptyPagesStillResumable(t *testing.T) {
	pages := map[string]scriptedPage{
		"":   {next: "p1"},
		"p1": {err: context.DeadlineExceeded},
	}
	got, err := collectLogEntries(expiredScanBudget(t), newScriptedLogIterator("", pages), "", 10)
	require.NoError(t, err)
	assert.True(t, got.ScanIncomplete)
	assert.Zero(t, got.Count)
	assert.Equal(t, "p1", got.NextPageToken)
}

func TestCollectLogEntriesFailsWithoutProgress(t *testing.T) {
	pages := map[string]scriptedPage{"p5": {err: context.DeadlineExceeded}}
	_, err := collectLogEntries(expiredScanBudget(t), newScriptedLogIterator("p5", pages), "p5", 10)
	require.Error(t, err, "no page was read, a token equal to the input would loop")
}

func TestCollectLogEntriesCallerCancellationIsAnError(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	pages := map[string]scriptedPage{
		"":   {next: "p1"},
		"p1": {err: context.Canceled},
	}
	_, err := collectLogEntries(ctx, newScriptedLogIterator("", pages), "", 10)
	require.ErrorIs(t, err, context.Canceled)
}
