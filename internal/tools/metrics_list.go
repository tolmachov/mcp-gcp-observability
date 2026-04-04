package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
)

// MetricsListHandler handles the metrics.list tool.
type MetricsListHandler struct {
	querier        gcpdata.MetricsQuerier
	registry       *metrics.Registry
	defaultProject string
}

// NewMetricsListHandler creates a new MetricsListHandler.
func NewMetricsListHandler(querier gcpdata.MetricsQuerier, registry *metrics.Registry, defaultProject string) *MetricsListHandler {
	return &MetricsListHandler{querier: querier, registry: registry, defaultProject: defaultProject}
}

// Tool returns the MCP tool definition.
func (h *MetricsListHandler) Tool() mcp.Tool {
	return mcp.NewTool("metrics.list",
		mcp.WithDescription("Discover available metrics from Cloud Monitoring and the semantic registry. "+
			"Use this first to find metric_type values before calling metrics.snapshot. "+
			"Results include kind, unit, and direction for each metric. "+
			"Does NOT return time series data — use metrics.snapshot for that."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithString("project_id",
			mcp.Description("GCP project ID (uses default if not specified)"),
		),
		mcp.WithString("match",
			mcp.Description("Substring to filter metric names (e.g. 'cpu', 'latency', 'request')"),
		),
		mcp.WithString("kind",
			mcp.Description("Filter by metric kind"),
			mcp.Enum("latency", "throughput", "error_rate", "resource_utilization", "saturation", "availability", "business_kpi"),
		),
		mcp.WithNumber("limit",
			mcp.Description("Maximum number of metrics to return (default 50, max 200)"),
			mcp.Min(1),
		),
	)
}

// MetricsListResult is the output for metrics.list.
type MetricsListResult struct {
	Count          int                `json:"count"`
	Metrics        []MetricsListEntry `json:"metrics"`
	Truncated      bool               `json:"truncated,omitempty"`
	TruncationHint string             `json:"truncation_hint,omitempty"`
}

// MetricsListEntry describes a single metric in the list output.
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

// Handle processes the metrics.list tool request.
func (h *MetricsListHandler) Handle(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project, errResult := requireProject(request, h.defaultProject)
	if errResult != nil {
		return errResult, nil
	}

	match := request.GetString("match", "")
	kindStr := request.GetString("kind", "")
	var kind metrics.MetricKind
	if kindStr != "" {
		kind = metrics.MetricKind(kindStr)
		if !kind.IsValid() || kind == metrics.KindUnknown {
			return mcp.NewToolResultError(fmt.Sprintf("invalid kind %q: must be one of %v", kindStr, metrics.ValidMetricKindsForInput())), nil
		}
	}
	limit := clampLimit(request.GetInt("limit", 50), 50, 200)

	// Registry entries.
	registryEntries := h.registry.List(match, kind)
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
	if match != "" {
		apiFilter = fmt.Sprintf(`metric.type = has_substring("%s")`, gcpdata.EscapeFilterValue(match))
	}
	apiLimit := limit - len(entries)
	if kind != "" {
		apiLimit *= 4 // Over-fetch to compensate for local kind filtering.
	}
	if apiLimit > 0 {
		descriptors, err := h.querier.ListMetricDescriptors(ctx, project, apiFilter, apiLimit)
		if err != nil {
			mcpLog(ctx, mcp.LoggingLevelError, "metrics.list", fmt.Sprintf("listing metric descriptors failed: %v", err))
			return mcp.NewToolResultError(fmt.Sprintf("Failed to list metrics: %v", err)), nil
		}

		for _, d := range descriptors {
			if seen[d.Type] {
				continue
			}
			meta := h.registry.Lookup(d.Type)
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
	result := MetricsListResult{}
	if len(entries) > limit {
		result.Truncated = true
		result.TruncationHint = fmt.Sprintf("Showing %d of %d+ metrics. Use 'match' to narrow the search or increase 'limit' (max 200).", limit, len(entries))
		entries = entries[:limit]
	}
	result.Count = len(entries)
	result.Metrics = entries

	return jsonResult(result)
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
