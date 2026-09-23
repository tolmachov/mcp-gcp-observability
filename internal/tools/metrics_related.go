package tools

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
		return "canceled", true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Sprintf("deadline exceeded: %v", err), false
	}
	if st, ok := status.FromError(err); ok {
		switch st.Code() {
		case codes.Canceled:
			return "canceled", true
		case codes.DeadlineExceeded:
			return fmt.Sprintf("deadline exceeded: %v", err), false
		case codes.NotFound:
			return fmt.Sprintf("metric type not found in project — check the registry entry is correct: %v", err), false
		}
	}
	return err.Error(), false
}

// relatedConcurrency bounds how many related metrics are queried at once. Each
// issues its current and baseline queries concurrently, so at most twice this
// many Monitoring API calls are in flight — a conservative bound for the
// default rate limits.
const relatedConcurrency = 5

func RegisterMetricsRelated(s *mcp.Server, d Deps) {
	requireQuerier(d.Querier)
	requireRegistry(d.Registry)
	mcp.AddTool(s, &mcp.Tool{
		Name: "metrics_related",
		Description: applyMode(d.Mode, "Check all related metrics (configured in the semantic registry) for the given metric and return which are anomalous. "+
			"Returns all related signals, not just anomalous ones, so you can see the full context. "+
			"Requires the metric to be configured in the registry with related_metrics. "+
			"Use this after metrics_snapshot to understand whether correlated signals moved together. "+
			"For breaking down a single metric by dimension, use metrics_top_contributors instead."),
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			OpenWorldHint:  new(true),
			IdempotentHint: true,
		},
		InputSchema: projectInputSchema[MetricsRelatedInput](d.Project,
			nonEmptyProp("metric_type"),
			enumProp("window", metricWindowNames()),
		),
		OutputSchema: outputSchemaFor[RelatedSignalsResult](),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in MetricsRelatedInput) (*mcp.CallToolResult, *RelatedSignalsResult, error) {
		project, err := d.Project.Resolve(in.ProjectID)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}

		_, windowDur, err := parseWindow(in.Window)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}

		related := d.Registry.RelatedMetrics(in.MetricType)
		if len(related) == 0 {
			r := &RelatedSignalsResult{
				Message: fmt.Sprintf("No related metrics configured for %q. Add related_metrics in the registry YAML to use this tool.", in.MetricType),
			}
			return nil, r, nil
		}

		now := time.Now().UTC()
		start := now.Add(-windowDur)
		stepSeconds := int64(metrics.DefaultStepSeconds)
		totalSignals := float64(len(related))

		sendProgress(ctx, req, 0, totalSignals, fmt.Sprintf("Querying %d related signals", len(related)))

		var signals []RelatedSignal
		var skipped []SkippedSignal
		var rpcFailures int
		var warningNotes []string
		var mu sync.Mutex
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
			mu.Lock()
			defer mu.Unlock()
			if note != "" && !slices.Contains(warningNotes, note) {
				warningNotes = append(warningNotes, note)
			}
		}

		errs := runParallel(ctx, "metrics_related", len(related), relatedConcurrency, func(i int) error {
			relMetric := related[i]
			relMeta := d.Registry.Lookup(relMetric)

			relDesc, err := d.Querier.GetMetricDescriptor(ctx, project, relMetric)
			if err != nil {
				mcpLog(ctx, req, logLevelWarning, "metrics_related",
					fmt.Sprintf("descriptor lookup failed for %s: %v", relMetric, err))
				reason, benign := classifyErr(err)
				addSkip(relMetric, fmt.Sprintf("failed to get metric descriptor: %s", reason), benign)
				return nil
			}

			relAggSpec := relMeta.ResolveAggregation()
			if err := relAggSpec.Validate(); err != nil {
				mcpLog(ctx, req, logLevelError, "metrics_related",
					fmt.Sprintf("registry misconfiguration for %s: %v", relMetric, err))
				addSkip(relMetric, formatRegistryMisconfigError(relMetric, err), false)
				return nil
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
			baselineWindows := BaselineModePrevWindow.windows(start, now, time.Time{})
			current, baselineResults := queryWithBaseline(ctx, "metrics_related", params, baselineWindows,
				func(p gcpdata.QueryTimeSeriesParams) ([]gcpdata.MetricTimeSeries, gcpdata.QueryWarnings, error) {
					return d.Querier.QueryTimeSeriesAggregated(ctx, p, relAggSpec)
				})

			addWarningNote(reportQueryWarnings(ctx, req, "metrics_related", relMetric, "current", current.warnings))
			if current.err != nil {
				mcpLog(ctx, req, logLevelWarning, "metrics_related",
					fmt.Sprintf("current window query failed for %s: %v", relMetric, current.err))
				reason, benign := classifyErr(current.err)
				addSkip(relMetric, fmt.Sprintf("query failed: %s", reason), benign)
				return nil
			}

			currentPoints := mergePoints(current.series)
			if len(currentPoints) == 0 {
				addSkip(relMetric, gcpdata.EmptyWindowReason(relDesc.Kind), true)
				return nil
			}

			baseline := baselineResults[0]
			addWarningNote(reportQueryWarnings(ctx, req, "metrics_related", relMetric, baselineWindows[0].label, baseline.warnings))
			if baseline.err != nil {
				mcpLog(ctx, req, logLevelWarning, "metrics_related",
					fmt.Sprintf("baseline query failed for %s: %v", relMetric, baseline.err))
				reason, benign := classifyErr(baseline.err)
				addSkip(relMetric, fmt.Sprintf("baseline query failed: %s", reason), benign)
				return nil
			}
			expectedBaseline := expectedPointsForWindow(windowDur, int(stepSeconds))

			f := metrics.Process(currentPoints, mergePoints(baseline.series), relMeta, int(stepSeconds), expectedBaseline, metrics.Window{Start: start, End: now})

			mu.Lock()
			signals = append(signals, RelatedSignal{
				MetricType:               relMetric,
				Kind:                     string(relMeta.Kind),
				Current:                  f.Current,
				Baseline:                 f.Baseline,
				DeltaPct:                 f.DeltaPct,
				Trend:                    string(f.Trend),
				TrendScore:               f.TrendScore,
				CV:                       f.CV,
				Classification:           string(f.Classification),
				ClassificationConfidence: string(f.Confidence),
				Anomaly:                  f.Classification.IsAnomalous(),
				NonFinitePoints:          current.warnings.NonFinitePoints + baseline.warnings.NonFinitePoints,
			})
			completed++
			progress := completed
			mu.Unlock()
			sendProgress(ctx, req, progress, totalSignals, fmt.Sprintf("Queried %s", relMetric))
			return nil
		})
		// The task reports its own skips; a returned error is a recovered
		// panic or a cancellation before the task started.
		for i, err := range errs {
			if err == nil {
				continue
			}
			var pe *panicError
			if errors.As(err, &pe) {
				mcpLog(ctx, req, logLevelError, "metrics_related", fmt.Sprintf("panic querying %s: %v", related[i], pe.value))
				addSkip(related[i], fmt.Sprintf("internal error: %v", pe.value), false)
				continue
			}
			reason, benign := classifyErr(err)
			addSkip(related[i], reason, benign)
		}

		sort.Slice(signals, func(i, j int) bool {
			return signals[i].MetricType < signals[j].MetricType
		})

		if len(signals) == 0 && rpcFailures > 0 {
			reasons := distinctRpcFailureReasons(skipped)
			msg := fmt.Sprintf("All related signal queries failed (or were skipped) and %d had real RPC failures — correlation coverage is unavailable. Reasons: %s",
				rpcFailures, strings.Join(reasons, "; "))
			mcpLog(ctx, req, logLevelError, "metrics_related", msg)
			return errResult(msg), nil, nil
		}

		var partialNote string
		if rpcFailures > 0 {
			reasons := distinctRpcFailureReasons(skipped)
			partialNote = fmt.Sprintf("%d related signal(s) could not be queried due to RPC failures and are excluded from results. Reasons: %s",
				rpcFailures, strings.Join(reasons, "; "))
			mcpLog(ctx, req, logLevelWarning, "metrics_related", partialNote)
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
	TrendScore               float64 `json:"trend_score,omitempty"`
	CV                       float64 `json:"cv,omitempty"`
	Classification           string  `json:"classification"`
	ClassificationConfidence string  `json:"classification_confidence"`
	Anomaly                  bool    `json:"anomaly"`
	NonFinitePoints          int     `json:"non_finite_points"`
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
