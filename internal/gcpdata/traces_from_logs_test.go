package gcpdata

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSeverityRank(t *testing.T) {
	assert.Equal(t, severityRanks["ERROR"], severityRank("error"), "should be case-insensitive")
	assert.Greater(t, severityRank("CRITICAL"), severityRank("WARNING"))
	assert.Equal(t, 0, severityRank("NONSENSE"), "unknown severity ranks lowest")
}

func TestFirstLine(t *testing.T) {
	assert.Equal(t, "", firstLine("  \n  "))
	assert.Equal(t, "hello", firstLine("hello\nworld"))
	assert.Equal(t, "trimmed", firstLine("  trimmed  \nrest"))
	long := strings.Repeat("x", sampleMessageMaxLen+50)
	got := firstLine(long)
	assert.Equal(t, sampleMessageMaxLen+len("..."), len(got))
	assert.True(t, strings.HasSuffix(got, "..."))
}

func TestServiceFromResourceInfo(t *testing.T) {
	assert.Equal(t, "", serviceFromResourceInfo(nil))
	assert.Equal(t, "k8s_container", serviceFromResourceInfo(&ResourceInfo{Type: "k8s_container"}))
	assert.Equal(t, "checkout", serviceFromResourceInfo(&ResourceInfo{
		Type:   "cloud_run_revision",
		Labels: map[string]string{"service_name": "checkout"},
	}))
	assert.Equal(t, "api", serviceFromResourceInfo(&ResourceInfo{
		Type:   "k8s_container",
		Labels: map[string]string{"container_name": "api"},
	}))
}

func TestSampleLogMessage(t *testing.T) {
	assert.Equal(t, "boom", sampleLogMessage(&LogEntry{TextPayload: "boom\ntrace..."}))
	assert.Equal(t, "json msg", sampleLogMessage(&LogEntry{
		JSONPayload: map[string]any{"message": "json msg"},
	}))
	assert.Equal(t, "", sampleLogMessage(&LogEntry{JSONPayload: map[string]any{"other": 42}}))
	// Text payload wins over JSON.
	assert.Equal(t, "text", sampleLogMessage(&LogEntry{
		TextPayload: "text",
		JSONPayload: map[string]any{"message": "json"},
	}))
}

func TestAggregateTracesFromLogs(t *testing.T) {
	const tracePrefix = "projects/p/traces/"
	entries := []LogEntry{
		{Trace: tracePrefix + "t1", Timestamp: "2026-06-01T10:00:02.000Z", Severity: "INFO", TextPayload: "started"},
		{Trace: tracePrefix + "t1", Timestamp: "2026-06-01T10:00:01.000Z", Severity: "ERROR", TextPayload: "boom",
			Resource: &ResourceInfo{Type: "k8s_container", Labels: map[string]string{"container_name": "api"}}},
		{Trace: "", Timestamp: "2026-06-01T10:00:00.000Z", Severity: "ERROR", TextPayload: "no trace, skipped"},
		{Trace: tracePrefix + "t2", Timestamp: "2026-06-01T09:59:00.000Z", Severity: "WARNING", TextPayload: "slow"},
	}

	res := aggregateTracesFromLogs(&LogQueryResult{Count: len(entries), Entries: entries}, 10)
	require.Equal(t, 4, res.ScannedEntries)
	require.Equal(t, 2, res.Count, "entry without a trace is skipped")

	// t1 has 2 entries -> sorts first by log volume.
	t1 := res.Traces[0]
	assert.Equal(t, "t1", t1.TraceID)
	assert.Equal(t, 2, t1.LogCount)
	assert.Equal(t, "2026-06-01T10:00:01.000Z", t1.FirstSeen)
	assert.Equal(t, "2026-06-01T10:00:02.000Z", t1.LastSeen)
	assert.Equal(t, "ERROR", t1.MaxSeverity, "most severe entry wins")
	assert.Equal(t, "boom", t1.SampleMessage, "sample comes from the most severe entry")
	assert.Equal(t, "api", t1.Service)
	assert.Equal(t, "k8s_container", t1.ResourceType)

	assert.Equal(t, "t2", res.Traces[1].TraceID)
}

func TestAggregateTracesFromLogs_Truncation(t *testing.T) {
	var entries []LogEntry
	for _, id := range []string{"a", "b", "c"} {
		entries = append(entries, LogEntry{Trace: "projects/p/traces/" + id, Timestamp: "2026-06-01T10:00:00.000Z", Severity: "INFO"})
	}
	res := aggregateTracesFromLogs(&LogQueryResult{Count: 3, Entries: entries, NextPageToken: "more"}, 2)
	assert.Equal(t, 2, res.Count)
	assert.Len(t, res.Traces, 2)
	assert.True(t, res.Truncated)
	assert.Contains(t, res.TruncationHint, "distinct traces")
	assert.Contains(t, res.TruncationHint, "more match", "scan-limit note included when NextPageToken set")
}
