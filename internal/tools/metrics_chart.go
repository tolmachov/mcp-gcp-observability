package tools

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"sync"
	"text/template"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// chartStaticURI is the fixed MCP resource URI for the metrics chart widget.
	// Used in both the tool definition (so hosts can prefetch the resource) and
	// in per-call results (so hosts that skip tools/list caching still bind the widget).
	chartStaticURI = "ui://metrics/chart"
	chartMIMEType  = "text/html;profile=mcp-app"

	// compareChartStaticURI is the fixed MCP resource URI for the metrics compare widget.
	// Used in both the tool definition (so hosts can prefetch the resource) and
	// in per-call results (so hosts that skip tools/list caching still bind the widget).
	compareChartStaticURI = "ui://metrics/compare"
	compareChartMIMEType  = "text/html;profile=mcp-app"
)

//go:embed metrics_chart.html
var chartHTMLRaw string

//go:embed metrics_compare_chart.html
var compareChartHTMLRaw string

var chartHTMLTmpl = template.Must(template.New("metrics-chart").Parse(chartHTMLRaw))
var compareChartHTMLTmpl = template.Must(template.New("metrics-compare-chart").Parse(compareChartHTMLRaw))

// chartPoint is the compact JSON representation of a time-series point
// sent in structuredContent for the UI to render.
type chartPoint struct {
	TS int64   `json:"ts"` // Unix seconds
	V  float64 `json:"v"`
}

// renderChartHTML returns the rendered metrics-chart widget HTML, executing
// the template once on first call and reusing the cached string thereafter.
// Cached because each variant's *mcp.Server registers the same resource
// (full/compact/monitoring => 3 boots → 3 renders without the cache).
var renderChartHTML = sync.OnceValue(func() string {
	var buf bytes.Buffer
	if err := chartHTMLTmpl.Execute(&buf, nil); err != nil {
		// Should never happen: the template has no actions.
		// Panic (like template.Must) rather than silently serving broken HTML.
		panic(fmt.Sprintf("[metrics-chart] BUG: template execution failed: %v", err))
	}
	return buf.String()
})

// renderCompareChartHTML is the metrics_compare equivalent of renderChartHTML.
// Same caching rationale: registered once per variant.
var renderCompareChartHTML = sync.OnceValue(func() string {
	var buf bytes.Buffer
	if err := compareChartHTMLTmpl.Execute(&buf, nil); err != nil {
		panic(fmt.Sprintf("[metrics-compare-chart] BUG: template execution failed: %v", err))
	}
	return buf.String()
})

// RegisterMetricsChartStaticResource registers the ui://metrics/chart resource.
// It returns a self-contained HTML page that implements the MCP Apps bridge
// protocol to receive structuredContent from the host and render a pure SVG
// time-series widget (no external dependencies).
func RegisterMetricsChartStaticResource(s *mcp.Server) {
	html := renderChartHTML()
	s.AddResource(
		&mcp.Resource{
			URI:      chartStaticURI,
			Name:     "metrics-chart",
			MIMEType: chartMIMEType,
			Description: "Interactive time-series chart for a Cloud Monitoring metric. " +
				"Rendered as an inline SVG widget. Data is delivered via the MCP Apps bridge " +
				"from structuredContent in the metrics_snapshot tool result.",
		},
		func(_ context.Context, _ *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{
				Contents: []*mcp.ResourceContents{{
					URI:      chartStaticURI,
					MIMEType: chartMIMEType,
					Text:     html,
				}},
			}, nil
		},
	)
}

// RegisterMetricsCompareChartStaticResource registers the ui://metrics/compare resource.
// It returns a self-contained HTML page that implements the MCP Apps bridge protocol
// to receive structuredContent and render a dual-series SVG comparing two windows.
func RegisterMetricsCompareChartStaticResource(s *mcp.Server) {
	html := renderCompareChartHTML()
	s.AddResource(
		&mcp.Resource{
			URI:      compareChartStaticURI,
			Name:     "metrics-compare-chart",
			MIMEType: compareChartMIMEType,
			Description: "Interactive dual-series chart for a Cloud Monitoring metrics comparison. " +
				"Rendered as an inline SVG widget showing two time windows side by side. " +
				"Data is delivered via the MCP Apps bridge from structuredContent in the metrics_compare tool result.",
		},
		func(_ context.Context, _ *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{
				Contents: []*mcp.ResourceContents{{
					URI:      compareChartStaticURI,
					MIMEType: compareChartMIMEType,
					Text:     html,
				}},
			}, nil
		},
	)
}
