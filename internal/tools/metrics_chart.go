package tools

import (
	"bytes"
	"context"
	_ "embed"
	"log/slog"
	"text/template"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// chartStaticURI is the fixed MCP resource URI for the metrics chart widget.
	// Declared in the tool definition so hosts can prefetch the template.
	chartStaticURI = "ui://metrics/chart"
	chartMIMEType  = "text/html;profile=mcp-app"
)

//go:embed metrics_chart.html
var chartHTMLRaw string

var chartHTMLTmpl = template.Must(template.New("metrics-chart").Parse(chartHTMLRaw))

// chartPoint is the compact JSON representation of a time-series point
// sent in structuredContent for the UI to render.
type chartPoint struct {
	TS int64   `json:"ts"` // Unix seconds
	V  float64 `json:"v"`
}

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

// renderChartHTML executes the embedded HTML template and returns the result.
// The template currently has no actions; text/template is used for
// forward-compatibility (e.g. injecting ToolName or version in the future).
func renderChartHTML() string {
	var buf bytes.Buffer
	if err := chartHTMLTmpl.Execute(&buf, nil); err != nil {
		// Should never happen: the template has no actions.
		slog.Error("[metrics-chart] BUG: template execution failed; serving raw HTML", "err", err)
		return chartHTMLRaw
	}
	return buf.String()
}
