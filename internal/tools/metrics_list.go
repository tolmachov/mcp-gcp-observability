package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
)

func RegisterMetricsList(s *mcp.Server, d Deps) {
	requireQuerier(d.Querier)
	requireRegistry(d.Registry)
	mcp.AddTool(s, &mcp.Tool{
		Name: "metrics_list",
		Description: applyMode(d.Mode, "Discover available metrics from Cloud Monitoring and the semantic registry. "+
			"Use this first to find metric_type values before calling metrics_snapshot. "+
			"The 'match' parameter searches metric names, the auto-derived service prefix, "+
			"and semantic keywords — so category synonyms like 'queue', 'cache', 'database', "+
			"'nosql', 'warehouse', or 'serverless' will find the relevant services even when "+
			"the literal word isn't in the metric name. "+
			"Results include kind, unit, and direction for each metric. "+
			"Does NOT return time series data — use metrics_snapshot for that."),
		Annotations: readOnlyAnnotations,
		InputSchema: projectInputSchema[MetricsListInput](d.Project,
			enumProp("kind", metrics.ValidMetricKindsForInput(), ""),
		),
		OutputSchema: outputSchemaFor[MetricsListResult](),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in MetricsListInput) (*mcp.CallToolResult, *MetricsListResult, error) {
		project, err := d.Project.Resolve(in.ProjectID)
		if err != nil {
			return ErrorResult(err.Error()), nil, nil
		}

		kind := metrics.MetricKind(in.Kind)
		limit := clampLimit(in.Limit, 50, 200)

		sendProgress(ctx, req, 0, 1, "Discovering metrics...")

		// Registry entries.
		seen := make(map[string]bool)
		var entries []MetricsListEntry
		for name, meta := range d.Registry.List(in.Match, kind) {
			seen[name] = true
			entries = append(entries, MetricsListEntry{
				MetricType:      name,
				Kind:            string(meta.Kind),
				Unit:            meta.Unit,
				BetterDirection: string(meta.BetterDirection),
				SLOThreshold:    meta.SLOThreshold,
				RelatedMetrics:  meta.RelatedMetrics,
			})
		}

		// Cloud Monitoring API discovery.
		apiFilter := ""
		if in.Match != "" {
			apiFilter = fmt.Sprintf(`metric.type = has_substring("%s")`, gcpdata.EscapeFilterValue(in.Match))
		}
		apiLimit := limit - len(entries)
		if kind != "" {
			apiLimit *= 4 // Over-fetch to compensate for local kind filtering.
		}
		if apiLimit > 0 {
			descriptors, err := d.Querier.ListMetricDescriptors(ctx, project, apiFilter, apiLimit)
			if err != nil {
				mcpLog(ctx, req, logLevelError, "metrics_list", fmt.Sprintf("listing metric descriptors failed: %v", err))
				return gcpErrorResult(fmt.Sprintf("Failed to list metrics: %v", err), err, ""), nil, nil
			}

			for _, desc := range descriptors {
				if seen[desc.Type] {
					continue
				}
				meta := d.Registry.Lookup(desc.Type)
				if kind != "" && meta.Kind != kind {
					continue
				}
				entries = append(entries, MetricsListEntry{
					MetricType:      desc.Type,
					DisplayName:     desc.DisplayName,
					Kind:            string(meta.Kind),
					Unit:            unitFromDescriptor(desc, meta),
					BetterDirection: string(meta.BetterDirection),
					AutoDetected:    meta.AutoDetected,
				})
			}
		}

		// Apply limit.
		result := &MetricsListResult{}
		if len(entries) > limit {
			result.Truncated = true
			result.TruncationHint = fmt.Sprintf("Showing %d of %d+ metrics. Use 'match' to narrow the search or increase 'limit' (max 200).", limit, len(entries))
			entries = entries[:limit]
		}
		result.Count = len(entries)
		result.Metrics = entries

		return nil, result, nil
	})
}

type MetricsListResult struct {
	Count          int                `json:"count"`
	Metrics        []MetricsListEntry `json:"metrics"`
	Truncated      bool               `json:"truncated,omitempty"`
	TruncationHint string             `json:"truncation_hint,omitempty"`
}

type MetricsListEntry struct {
	MetricType      string   `json:"metric_type"`
	DisplayName     string   `json:"display_name,omitempty"`
	Kind            string   `json:"kind"`
	Unit            string   `json:"unit"`
	BetterDirection string   `json:"better_direction"`
	SLOThreshold    *float64 `json:"slo_threshold,omitempty"`
	RelatedMetrics  []string `json:"related_metrics,omitempty"`
	AutoDetected    bool     `json:"auto_detected,omitempty"`
}

func unitFromDescriptor(d gcpdata.MetricDescriptorInfo, meta metrics.MetricMeta) string {
	if meta.Unit != "" {
		return meta.Unit
	}
	u := strings.TrimPrefix(d.Unit, "{")
	u = strings.TrimSuffix(u, "}")
	if u == "1" || u == "" {
		return ""
	}
	return u
}
