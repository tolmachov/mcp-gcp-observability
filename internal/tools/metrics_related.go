package tools

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
)

// classifyErr buckets an error from a per-signal goroutine into either a
// benign skip or a real failure. Only context.Canceled is benign — the
// client hung up and there is nothing to report. context.DeadlineExceeded is
// intentionally treated as a real failure: it signals a performance issue and
// operators need to see Partial=true when it occurs. This distinction drives
// the rpcFailures counter and the all-failed error path.
func classifyErr(err error) (reason string, benign bool) {
	if err == nil {
		return "", true
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled", true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Sprintf("deadline exceeded: %v", err), false
	}
	if st, ok := status.FromError(err); ok {
		switch st.Code() {
		case codes.Canceled:
			return "cancelled", true
		case codes.DeadlineExceeded:
			return fmt.Sprintf("deadline exceeded: %v", err), false
		case codes.NotFound:
			return fmt.Sprintf("metric type not found in project — check the registry entry is correct: %v", err), false
		}
	}
	return err.Error(), false
}

func RegisterMetricsRelated(s *mcp.Server, querier gcpdata.MetricsQuerier, registry *metrics.Registry, defaultProject string) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "metrics.related",
		Description: "Check all related metrics (configured in the semantic registry) for the given metric and return which are anomalous. " +
			"Returns all related signals, not just anomalous ones, so you can see the full context. " +
			"Requires the metric to be configured in the registry with related_metrics. " +
			"Use this after metrics.snapshot to understand whether correlated signals moved together. " +
			"For breaking down a single metric by dimension, use metrics.top_contributors instead.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			OpenWorldHint:  new(true),
			IdempotentHint: true,
		},
		InputSchema: inputSchemaWithEnums[MetricsRelatedInput](
			enumPatch{"window", enumWindow},
		),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in MetricsRelatedInput) (*mcp.CallToolResult, *RelatedSignalsResult, error) {
		if in.MetricType == "" {
			return errResult("metric_type is required"), nil, nil
		}
		project, err := resolveProject(in.ProjectID, defaultProject)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}

		windowStr := in.Window
		if windowStr == "" {
			windowStr = "1h"
		}
		windowDur, err := parseWindow(windowStr)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}

		related := registry.RelatedMetrics(in.MetricType)
		if len(related) == 0 {
			r := &RelatedSignalsResult{
				Message: fmt.Sprintf("No related metrics configured for %q. Add related_metrics in the registry YAML to use this tool.", in.MetricType),
			}
			return nil, r, nil
		}

		now := time.Now().UTC()
		start := now.Add(-windowDur)
		stepSeconds := int64(60)
		totalSignals := float64(len(related))

		sendProgress(ctx, req, 0, totalSignals, fmt.Sprintf("Querying %d related signals", len(related)))

		var signals []RelatedSignal
		var skipped []SkippedSignal
		var rpcFailures int
		var warningNotes []string
		var warningNotesSeen = make(map[string]bool)
		var mu sync.Mutex
		var wg sync.WaitGroup
		completed := float64(0)

		// addSkip must be called with a pre-classified benign flag —
		// distinctRpcFailureReasons uses the flag, not the reason text.
		addSkip := func(relMetric, reason string, benign bool) {
			mu.Lock()
			defer mu.Unlock()
			skipped = append(skipped, SkippedSignal{MetricType: relMetric, Reason: reason, benign: benign})
			if !benign {
				rpcFailures++
			}
		}
		addWarningNote := func(note string) {
			if note == "" {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if warningNotesSeen[note] {
				return
			}
			warningNotesSeen[note] = true
			warningNotes = append(warningNotes, note)
		}

		// sem limits concurrent GCP API calls to avoid quota exhaustion; capacity
		// 10 is a conservative bound for Monitoring API default rate limits.
		sem := make(chan struct{}, 10)

		for _, relMetric := range related {
			wg.Add(1)
			go func(relMetric string) {
				sem <- struct{}{}
				defer func() { <-sem }()
				defer wg.Done()
				defer func() {
					if r := recover(); r != nil {
						stack := debug.Stack()
						msg := fmt.Sprintf("panic querying %s: %v\n%s", relMetric, r, stack)
						notifyErrLog.Load().Printf("metrics.related: %s", msg)
						mcpLog(ctx, req, logLevelError, "metrics.related", msg)
						addSkip(relMetric, fmt.Sprintf("internal error: %v", r), false)
					}
				}()

				if ctx.Err() != nil {
					reason, benign := classifyErr(ctx.Err())
					addSkip(relMetric, reason, benign)
					return
				}

				relMeta := registry.Lookup(relMetric)

				relDesc, err := querier.GetMetricDescriptor(ctx, project, relMetric)
				if err != nil {
					mcpLog(ctx, req, logLevelWarning, "metrics.related",
						fmt.Sprintf("descriptor lookup failed for %s: %v", relMetric, err))
					reason, benign := classifyErr(err)
					addSkip(relMetric, fmt.Sprintf("failed to get metric descriptor: %s", reason), benign)
					return
				}

				params := gcpdata.QueryTimeSeriesParams{
					Project:     project,
					MetricType:  relMetric,
					LabelFilter: in.Filter,
					Start:       start,
					End:         now,
					StepSeconds: stepSeconds,
					MetricKind:  relDesc.Kind,
					ValueType:   relDesc.ValueType,
				}

				relAggSpec := relMeta.ResolveAggregation()
				if err := relAggSpec.Validate(); err != nil {
					mcpLog(ctx, req, logLevelError, "metrics.related",
						fmt.Sprintf("registry misconfiguration for %s: %v", relMetric, err))
					addSkip(relMetric, formatRegistryMisconfigError(relMetric, err), false)
					return
				}

				currentSeries, currentWarnings, qErr := querier.QueryTimeSeriesAggregated(ctx, params, relAggSpec)
				logAggregationWarnings(ctx, req, "metrics.related", relMetric, "current", currentWarnings)
				addWarningNote(aggregationWarningsNote(relMetric, "current", currentWarnings))
				if qErr != nil {
					mcpLog(ctx, req, logLevelWarning, "metrics.related",
						fmt.Sprintf("current window query failed for %s: %v", relMetric, qErr))
					reason, benign := classifyErr(qErr)
					addSkip(relMetric, fmt.Sprintf("query failed: %s", reason), benign)
					return
				}
				reportUnsupportedPoints(ctx, req, "metrics.related", relMetric, currentSeries)

				currentPoints := mergePoints(currentSeries)
				if len(currentPoints) == 0 {
					reason := "no data in window"
					switch relDesc.Kind {
					case "DELTA", "CUMULATIVE":
						reason = "no events in window (counter inactive)"
					case "GAUGE":
						reason = "no data in window (no resources reporting)"
					}
					addSkip(relMetric, reason, true)
					return
				}

				baselineParams := params
				baselineParams.End = start
				baselineParams.Start = start.Add(-windowDur)
				baselineSeries, baselineWarnings, qErr := querier.QueryTimeSeriesAggregated(ctx, baselineParams, relAggSpec)
				logAggregationWarnings(ctx, req, "metrics.related", relMetric, "baseline", baselineWarnings)
				addWarningNote(aggregationWarningsNote(relMetric, "baseline", baselineWarnings))
				if qErr != nil {
					mcpLog(ctx, req, logLevelWarning, "metrics.related",
						fmt.Sprintf("baseline query failed for %s: %v", relMetric, qErr))
					reason, benign := classifyErr(qErr)
					addSkip(relMetric, fmt.Sprintf("baseline query failed: %s", reason), benign)
					return
				}
				baselinePoints := mergePoints(baselineSeries)
				expectedBaseline := expectedPointsForWindow(windowDur, int(stepSeconds))

				f := metrics.Process(currentPoints, baselinePoints, relMeta, int(stepSeconds), expectedBaseline)

				anomaly := f.Classification != metrics.ClassStable && f.Classification != metrics.ClassNoisy
				mu.Lock()
				signals = append(signals, RelatedSignal{
					MetricType:               relMetric,
					Kind:                     string(relMeta.Kind),
					Current:                  f.Current,
					Baseline:                 f.Baseline,
					DeltaPct:                 f.DeltaPct,
					Trend:                    string(f.Trend),
					Classification:           safeClassification(f.Classification),
					ClassificationConfidence: string(f.Confidence),
					Anomaly:                  anomaly,
				})
				completed++
				progress := completed
				mu.Unlock()
				sendProgress(ctx, req, progress, totalSignals, fmt.Sprintf("Queried %s", relMetric))
			}(relMetric)
		}
		wg.Wait()

		sort.Slice(signals, func(i, j int) bool {
			return signals[i].MetricType < signals[j].MetricType
		})

		if len(signals) == 0 && rpcFailures > 0 {
			reasons := distinctRpcFailureReasons(skipped)
			msg := fmt.Sprintf("All related signal queries failed (or were skipped) and %d had real RPC failures — correlation coverage is unavailable. Reasons: %s",
				rpcFailures, strings.Join(reasons, "; "))
			mcpLog(ctx, req, logLevelError, "metrics.related", msg)
			return errResult(msg), nil, nil
		}

		var partialNote string
		if rpcFailures > 0 {
			reasons := distinctRpcFailureReasons(skipped)
			partialNote = fmt.Sprintf("%d related signal(s) could not be queried due to RPC failures and are excluded from results. Reasons: %s",
				rpcFailures, strings.Join(reasons, "; "))
			mcpLog(ctx, req, logLevelWarning, "metrics.related", partialNote)
		}
		partialNote = joinNote(partialNote, joinNote(warningNotes...))
		return nil, &RelatedSignalsResult{
			RelatedSignals: signals,
			Skipped:        skipped,
			Partial:        rpcFailures > 0 || len(warningNotes) > 0,
			Note:           partialNote,
		}, nil
	})
}

type RelatedSignalsResult struct {
	RelatedSignals []RelatedSignal `json:"related_signals"`
	Skipped        []SkippedSignal `json:"skipped,omitempty"`
	Partial        bool            `json:"partial,omitempty"`
	Note           string          `json:"note,omitempty"`
	Message        string          `json:"message,omitempty"`
}

type RelatedSignal struct {
	MetricType               string  `json:"metric_type"`
	Kind                     string  `json:"kind"`
	Current                  float64 `json:"current"`
	Baseline                 float64 `json:"baseline"`
	DeltaPct                 float64 `json:"delta_pct"`
	Trend                    string  `json:"trend"`
	Classification           string  `json:"classification"`
	ClassificationConfidence string  `json:"classification_confidence"`
	Anomaly                  bool    `json:"anomaly"`
}

type SkippedSignal struct {
	MetricType string `json:"metric_type"`
	Reason     string `json:"reason"`
	benign     bool
}

func distinctRpcFailureReasons(skipped []SkippedSignal) []string {
	seen := make(map[string]bool, len(skipped))
	out := make([]string, 0, len(skipped))
	for _, s := range skipped {
		if s.benign {
			continue
		}
		if seen[s.Reason] {
			continue
		}
		seen[s.Reason] = true
		out = append(out, s.Reason)
	}
	return out
}
