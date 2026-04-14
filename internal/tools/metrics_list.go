package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
)

func RegisterMetricsList(s *mcp.Server, querier gcpdata.MetricsQuerier, registry *metrics.Registry, defaultProject string) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "metrics_list",
		Description: "Discover available metrics from Cloud Monitoring and the semantic registry. " +
			"Use this first to find metric_type values before calling metrics_snapshot. " +
			"The 'match' parameter searches metric names, the auto-derived service prefix, " +
			"and semantic keywords — so category synonyms like 'queue', 'cache', 'database', " +
			"'nosql', 'warehouse', or 'serverless' will find the relevant services even when " +
			"the literal word isn't in the metric name. " +
			"Results include kind, unit, and direction for each metric. " +
			"Does NOT return time series data — use metrics_snapshot for that.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			OpenWorldHint:  new(true),
			IdempotentHint: true,
		},
		InputSchema: inputSchemaWithEnums[MetricsListInput](
			enumPatch{"kind", toAny(metrics.ValidMetricKindsForInput())},
		),
		OutputSchema: outputSchemaFor[MetricsListResult](),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in MetricsListInput) (*mcp.CallToolResult, *MetricsListResult, error) {
		project, err := resolveProject(in.ProjectID, defaultProject)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}

		var kind metrics.MetricKind
		if in.Kind != "" {
			kind = metrics.MetricKind(in.Kind)
			if !kind.IsValid() || kind == metrics.KindUnknown {
				return errResult(fmt.Sprintf("invalid kind %q: must be one of %v", in.Kind, metrics.ValidMetricKindsForInput())), nil, nil
			}
		}
		limit := clampLimit(in.Limit, 50, 200)

		sendProgress(ctx, req, 0, 1, "Discovering metrics...")

		// Registry entries.
		registryEntries := registry.List(in.Match, kind)
		seen := make(map[string]bool, len(registryEntries))

		var entries []MetricsListEntry
		for _, re := range registryEntries {
			seen[re.MetricType] = true
			entries = append(entries, MetricsListEntry{
				MetricType:      re.MetricType,
				Kind:            string(re.Kind),
				Unit:            re.Unit,
				BetterDirection: string(re.BetterDirection),
				SLOThreshold:    re.SLOThreshold,
				RelatedMetrics:  re.RelatedMetrics,
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
			descriptors, err := querier.ListMetricDescriptors(ctx, project, apiFilter, apiLimit)
			if err != nil {
				mcpLog(ctx, req, logLevelError, "metrics_list", fmt.Sprintf("listing metric descriptors failed: %v", err))
				return errResult(fmt.Sprintf("Failed to list metrics: %v", err)), nil, nil
			}

			for _, d := range descriptors {
				if seen[d.Type] {
					continue
				}
				meta := registry.Lookup(d.Type)
				if kind != "" && meta.Kind != kind {
					continue
				}
				entries = append(entries, MetricsListEntry{
					MetricType:      d.Type,
					DisplayName:     d.DisplayName,
					Kind:            string(meta.Kind),
					Unit:            unitFromDescriptor(d, meta),
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
