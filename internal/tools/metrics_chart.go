package tools

import (
	"context"
	_ "embed"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// chartStaticURI is the fixed MCP resource URI for the metrics chart widget.
	// Used in both the tool definition (so hosts can prefetch the resource) and
	// in per-call results (so hosts that skip tools/list caching still bind the widget).
	chartStaticURI = "ui://metrics/chart"

	// compareChartStaticURI is the fixed MCP resource URI for the metrics compare widget.
	// Used in both the tool definition (so hosts can prefetch the resource) and
	// in per-call results (so hosts that skip tools/list caching still bind the widget).
	compareChartStaticURI = "ui://metrics/compare"

	// chartMIMEType is the MCP Apps MIME type shared by both chart widgets.
	chartMIMEType = "text/html;profile=mcp-app"
)

//go:embed metrics_chart.html
var chartHTML string

//go:embed metrics_compare_chart.html
var compareChartHTML string

// chartPoint is the compact JSON representation of a time-series point
// sent in structuredContent for the UI to render.
type chartPoint struct {
	TS int64   `json:"ts"` // Unix seconds
	V  float64 `json:"v"`
}

// RegisterChartResources registers the static MCP Apps chart widgets:
// ui://metrics/chart (metrics_snapshot) and ui://metrics/compare (metrics_compare).
// Each is a self-contained HTML page that implements the MCP Apps bridge protocol
// to receive structuredContent from the host and render a pure SVG widget
// (no external dependencies).
func RegisterChartResources(s *mcp.Server) {
	registerHTMLResource(s, chartStaticURI, "metrics-chart",
		"Interactive time-series chart for a Cloud Monitoring metric. "+
			"Rendered as an inline SVG widget. Data is delivered via the MCP Apps bridge "+
			"from structuredContent in the metrics_snapshot tool result.",
		chartHTML)
	registerHTMLResource(s, compareChartStaticURI, "metrics-compare-chart",
		"Interactive dual-series chart for a Cloud Monitoring metrics comparison. "+
			"Rendered as an inline SVG widget showing two time windows side by side. "+
			"Data is delivered via the MCP Apps bridge from structuredContent in the metrics_compare tool result.",
		compareChartHTML)
}

// registerHTMLResource registers a static MCP Apps HTML resource at uri.
func registerHTMLResource(s *mcp.Server, uri, name, desc, html string) {
	s.AddResource(
		&mcp.Resource{
			URI:         uri,
			Name:        name,
			MIMEType:    chartMIMEType,
			Description: desc,
		},
		func(_ context.Context, _ *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{
				Contents: []*mcp.ResourceContents{{
					URI:      uri,
					MIMEType: chartMIMEType,
					Text:     html,
				}},
			}, nil
		},
	)
}
