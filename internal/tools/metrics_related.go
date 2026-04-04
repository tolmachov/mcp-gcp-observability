package tools

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
)

// MetricsRelatedHandler handles the metrics.related tool.
type MetricsRelatedHandler struct {
	querier        gcpdata.MetricsQuerier
	registry       *metrics.Registry
	defaultProject string
}

// NewMetricsRelatedHandler creates a new MetricsRelatedHandler.
func NewMetricsRelatedHandler(querier gcpdata.MetricsQuerier, registry *metrics.Registry, defaultProject string) *MetricsRelatedHandler {
	return &MetricsRelatedHandler{querier: querier, registry: registry, defaultProject: defaultProject}
}

// Tool returns the MCP tool definition.
func (h *MetricsRelatedHandler) Tool() mcp.Tool {
	return mcp.NewTool("metrics.related",
		mcp.WithDescription("Check all related metrics (configured in the semantic registry) for the given metric and return which are anomalous. "+
			"Returns all related signals, not just anomalous ones, so you can see the full context. "+
			"Requires the metric to be configured in the registry with related_metrics. "+
			"Use this after metrics.snapshot to understand whether correlated signals moved together. "+
			"For breaking down a single metric by dimension, use metrics.top_contributors instead."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithString("project_id",
			mcp.Description("GCP project ID (uses default if not specified)"),
		),
		mcp.WithString("metric_type",
			mcp.Description("Full Cloud Monitoring metric type"),
			mcp.Required(),
		),
		mcp.WithString("filter",
			mcp.Description("Additional Cloud Monitoring label filter"),
		),
		mcp.WithString("window",
			mcp.Description("Time window to analyze (default '1h')"),
			mcp.Enum("15m", "30m", "1h", "3h", "6h", "24h"),
		),
	)
}

// RelatedSignalsResult is the output for metrics.related.
type RelatedSignalsResult struct {
	RelatedSignals []RelatedSignal `json:"related_signals"`
	Skipped        []SkippedSignal `json:"skipped,omitempty"`
	Message        string          `json:"message,omitempty"`
}

// RelatedSignal describes a related metric's current status.
type RelatedSignal struct {
	MetricType     string  `json:"metric_type"`
	Kind           string  `json:"kind"`
	Current        float64 `json:"current"`
	Baseline       float64 `json:"baseline"`
	DeltaPct       float64 `json:"delta_pct"`
	Trend          string  `json:"trend"`
	Classification string  `json:"classification"`
	Anomaly        bool    `json:"anomaly"`
}

// SkippedSignal describes a related metric that could not be queried.
type SkippedSignal struct {
	MetricType string `json:"metric_type"`
	Reason     string `json:"reason"`
}

// Handle processes the metrics.related tool request.
func (h *MetricsRelatedHandler) Handle(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	metricType, err := request.RequireString("metric_type")
	if err != nil {
		return mcp.NewToolResultError("metric_type is required"), nil
	}

	project, errResult := requireProject(request, h.defaultProject)
	if errResult != nil {
		return errResult, nil
	}

	labelFilter := request.GetString("filter", "")
	windowStr := request.GetString("window", "1h")

	windowDur, err := parseWindow(windowStr)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	related := h.registry.RelatedMetrics(metricType)
	if len(related) == 0 {
		return jsonResult(RelatedSignalsResult{
			Message: fmt.Sprintf("No related metrics configured for %q. Add related_metrics in the registry YAML to use this tool.", metricType),
		})
	}

	now := time.Now().UTC()
	start := now.Add(-windowDur)
	stepSeconds := int64(60)
	totalSignals := float64(len(related))

	sendProgress(ctx, request, 0, totalSignals, fmt.Sprintf("Querying %d related signals", len(related)))

	var signals []RelatedSignal
	var skipped []SkippedSignal
	var mu sync.Mutex
	var wg sync.WaitGroup
	completed := float64(0)

	// Limit concurrent GCP API calls to avoid rate limiting.
	sem := make(chan struct{}, 10)

	for _, relMetric := range related {
		wg.Add(1)
		go func(relMetric string) {
			sem <- struct{}{}
			defer func() { <-sem }()
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					mcpLog(ctx, mcp.LoggingLevelError, "metrics.related", fmt.Sprintf("panic querying %s: %v", relMetric, r))
					mu.Lock()
					skipped = append(skipped, SkippedSignal{MetricType: relMetric, Reason: fmt.Sprintf("internal error: %v", r)})
					mu.Unlock()
				}
			}()

			if ctx.Err() != nil {
				mu.Lock()
				skipped = append(skipped, SkippedSignal{MetricType: relMetric, Reason: "cancelled"})
				mu.Unlock()
				return
			}

			relMeta := h.registry.Lookup(relMetric)

			gcpKind, err := h.querier.GetMetricKind(ctx, project, relMetric)
			if err != nil {
				mu.Lock()
				skipped = append(skipped, SkippedSignal{MetricType: relMetric, Reason: fmt.Sprintf("failed to get metric kind: %v", err)})
				mu.Unlock()
				return
			}

			params := gcpdata.QueryTimeSeriesParams{
				Project:     project,
				MetricType:  relMetric,
				LabelFilter: labelFilter,
				Start:       start,
				End:         now,
				StepSeconds: stepSeconds,
				MetricKind:  gcpKind,
				Reducer:     monitoringpb.Aggregation_REDUCE_MEAN,
			}

			currentSeries, qErr := h.querier.QueryTimeSeries(ctx, params)
			if qErr != nil {
				mu.Lock()
				skipped = append(skipped, SkippedSignal{MetricType: relMetric, Reason: fmt.Sprintf("query failed: %v", qErr)})
				mu.Unlock()
				return
			}

			currentPoints := mergePoints(currentSeries)
			if len(currentPoints) == 0 {
				mu.Lock()
				skipped = append(skipped, SkippedSignal{MetricType: relMetric, Reason: "no data in window"})
				mu.Unlock()
				return
			}

			// Baseline: prev_window.
			baselineParams := params
			baselineParams.End = start
			baselineParams.Start = start.Add(-windowDur)
			baselineSeries, qErr := h.querier.QueryTimeSeries(ctx, baselineParams)
			if qErr != nil {
				mu.Lock()
				skipped = append(skipped, SkippedSignal{MetricType: relMetric, Reason: fmt.Sprintf("baseline query failed: %v", qErr)})
				mu.Unlock()
				return
			}
			baselinePoints := mergePoints(baselineSeries)

			f := metrics.Process(currentPoints, baselinePoints, relMeta, int(stepSeconds))

			anomaly := f.Classification != metrics.ClassStable && f.Classification != metrics.ClassNoisy
			mu.Lock()
			signals = append(signals, RelatedSignal{
				MetricType:     relMetric,
				Kind:           string(relMeta.Kind),
				Current:        f.Current,
				Baseline:       f.Baseline,
				DeltaPct:       f.DeltaPct,
				Trend:          f.Trend,
				Classification: string(f.Classification),
				Anomaly:        anomaly,
			})
			completed++
			progress := completed
			mu.Unlock()
			sendProgress(ctx, request, progress, totalSignals, fmt.Sprintf("Queried %s", relMetric))
		}(relMetric)
	}
	wg.Wait()

	sort.Slice(signals, func(i, j int) bool {
		return signals[i].MetricType < signals[j].MetricType
	})

	return jsonResult(RelatedSignalsResult{RelatedSignals: signals, Skipped: skipped})
}
