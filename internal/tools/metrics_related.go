package tools

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
)

// relatedConcurrency bounds how many related metrics are queried at once.
// Each issues its descriptor, current and baseline queries one after another,
// so at most this many Monitoring API calls are in flight — a conservative
// bound for the default rate limits.
const relatedConcurrency = 10

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
		Annotations: readOnlyAnnotations,
		InputSchema: projectInputSchema[MetricsRelatedInput](d.Project,
			nonEmptyProp("metric_type"),
			enumProp("window", metricWindowNames(), defaultMetricWindow),
		),
		OutputSchema: outputSchemaFor[RelatedSignalsResult](),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in MetricsRelatedInput) (*mcp.CallToolResult, *RelatedSignalsResult, error) {
		project, err := d.Project.Resolve(in.ProjectID)
		if err != nil {
			return ErrorResult(err.Error()), nil, nil
		}

		_, windowDur, err := parseWindow(in.Window)
		if err != nil {
			return ErrorResult(err.Error()), nil, nil
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
		var warningNotes []string
		var mu sync.Mutex
		completed := float64(0)

		addSkip := func(relMetric, reason string, cause skipCause, err error) {
			mu.Lock()
			defer mu.Unlock()
			skipped = append(skipped, SkippedSignal{MetricType: relMetric, Reason: reason, cause: cause, err: err})
		}
		// skipFailure records err, the failure of relMetric's task, as a
		// benign skip or a query failure.
		skipFailure := func(relMetric string, err error) {
			cause := skipQueryFailed
			if isBenign(err) {
				cause = skipBenign
			}
			addSkip(relMetric, err.Error(), cause, err)
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
				skipFailure(relMetric, fmt.Errorf("failed to get metric descriptor: %w", err))
				return nil
			}

			relAggSpec := relMeta.ResolveAggregation()
			if err := relAggSpec.Validate(); err != nil {
				mcpLog(ctx, req, logLevelError, "metrics_related",
					fmt.Sprintf("registry misconfiguration for %s: %v", relMetric, err))
				addSkip(relMetric, formatRegistryMisconfigError(relMetric, err), skipMisconfig, err)
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
			baselineWindows := baselineSpec{mode: baselinePrevWindow}.windows(start, now)
			current, baselineResults := queryWithBaseline(ctx, "metrics_related", params, baselineWindows,
				func(p gcpdata.QueryTimeSeriesParams) ([]gcpdata.MetricTimeSeries, gcpdata.QueryWarnings, error) {
					return d.Querier.QueryTimeSeriesAggregated(ctx, p, relAggSpec)
				})

			addWarningNote(reportQueryWarnings(ctx, req, "metrics_related", relMetric, "current", current.warnings))
			if current.err != nil {
				mcpLog(ctx, req, logLevelWarning, "metrics_related",
					fmt.Sprintf("current window query failed for %s: %v", relMetric, current.err))
				skipFailure(relMetric, fmt.Errorf("query failed: %w", current.err))
				return nil
			}
			if !current.hasPoints() {
				addSkip(relMetric, gcpdata.EmptyWindowReason(relDesc.Kind), skipBenign, nil)
				return nil
			}

			baseline := baselineResults[0]
			addWarningNote(reportQueryWarnings(ctx, req, "metrics_related", relMetric, baselineWindows[0].label, baseline.warnings))
			if baseline.err != nil {
				mcpLog(ctx, req, logLevelWarning, "metrics_related",
					fmt.Sprintf("baseline query failed for %s: %v", relMetric, baseline.err))
				skipFailure(relMetric, fmt.Errorf("baseline query failed: %w", baseline.err))
				return nil
			}
			expectedBaseline := expectedPointsForWindow(windowDur, int(stepSeconds))

			f := metrics.Process(mergePoints(current.series), mergePoints(baseline.series), relMeta, int(stepSeconds), expectedBaseline, metrics.Window{Start: start, End: now})

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
		// panic or a task that never started, and says which.
		for i, err := range errs {
			if err != nil {
				skipFailure(related[i], err)
			}
		}

		sort.Slice(signals, func(i, j int) bool {
			return signals[i].MetricType < signals[j].MetricType
		})

		if err := ctx.Err(); err != nil && len(signals) == 0 {
			return gcpErrorResult(fmt.Sprintf("metrics_related stopped before any related signal was queried: %v", err), err, ""), nil, nil
		}

		failures, failedErrs := failureSummary(skipped)
		if len(signals) == 0 && failures != "" {
			msg := "Every related signal failed or was skipped — correlation coverage is unavailable. " + failures
			mcpLog(ctx, req, logLevelError, "metrics_related", msg)
			return gcpErrorsResult(msg, failedErrs, ""), nil, nil
		}

		var partialNote string
		if failures != "" {
			partialNote = joinNote("Some related signals could not be queried and are excluded from results. "+failures,
				errorGuidance(failedErrs, ""))
			mcpLog(ctx, req, logLevelWarning, "metrics_related", partialNote)
		}
		return nil, &RelatedSignalsResult{
			RelatedSignals: signals,
			Skipped:        skipped,
			Partial:        failures != "" || len(warningNotes) > 0,
			Note:           joinNote(partialNote, joinNote(warningNotes...)),
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
	MetricType string    `json:"metric_type"`
	Reason     string    `json:"reason"`
	cause      skipCause // not serialized
	err        error     // the failure behind Reason, nil for a skip without one; not serialized
}

// skipCause is why a related signal was skipped. Every cause but skipBenign
// makes the result partial.
type skipCause int

const (
	skipBenign      skipCause = iota // no data in the window, or the client canceled
	skipQueryFailed                  // a GCP call failed, or the task panicked or never started
	skipMisconfig                    // the registry entry is invalid
)

// skipCauseCounts labels the count of the skips of each non-benign cause.
var skipCauseCounts = [...]string{
	skipQueryFailed: "%d query failure(s)",
	skipMisconfig:   "%d registry misconfiguration(s) (fix the registry YAML; retrying will not help)",
}

// failureSummary describes the non-benign skips: how many there are of each
// cause, then their distinct reasons. It is empty when every skip is benign.
// It also returns the errors behind those skips, for their guidance.
func failureSummary(skipped []SkippedSignal) (string, []error) {
	var counts [len(skipCauseCounts)]int
	var reasons []string
	var errs []error
	for _, s := range skipped {
		if s.cause == skipBenign {
			continue
		}
		counts[s.cause]++
		errs = append(errs, s.err)
		if !slices.Contains(reasons, s.Reason) {
			reasons = append(reasons, s.Reason)
		}
	}
	if len(reasons) == 0 {
		return "", nil
	}
	var labels []string
	for cause, n := range counts {
		if n > 0 {
			labels = append(labels, fmt.Sprintf(skipCauseCounts[cause], n))
		}
	}
	return strings.Join(labels, ", ") + ". Reasons: " + strings.Join(reasons, "; "), errs
}
