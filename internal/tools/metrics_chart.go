package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
)

const (
	chartURITemplate = "ui://metrics/chart/{+metric_type}"
	chartMIMEType    = "text/html;profile=mcp-app"
	chartCDNDomain   = "cdn.jsdelivr.net"
)

// buildChartURL builds the resource URI for the metrics chart widget.
// The URI encodes all parameters needed to re-fetch and render the chart.
func buildChartURL(metricType, filter, window string, stepSeconds int64, project string) string {
	u := url.URL{
		Scheme: "ui",
		Host:   "metrics",
		Path:   "/chart/" + metricType,
	}
	q := url.Values{}
	q.Set("window", window)
	q.Set("step", strconv.FormatInt(stepSeconds, 10))
	if filter != "" {
		q.Set("filter", filter)
	}
	if project != "" {
		q.Set("project", project)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// RegisterMetricsChartResource registers the ui://metrics/chart/{+metric_type}
// resource template. When fetched, it returns a self-contained HTML page with
// a Chart.js time-series widget, data embedded inline.
func RegisterMetricsChartResource(s *mcp.Server, querier gcpdata.MetricsQuerier, registry *metrics.Registry, defaultProject string) {
	s.AddResourceTemplate(
		&mcp.ResourceTemplate{
			URITemplate: chartURITemplate,
			Name:        "metrics-chart",
			Description: "Interactive time-series chart for a Cloud Monitoring metric. Rendered as a Chart.js widget with current data and baseline reference. URI is provided via _meta.ui.resourceUri in metrics_snapshot results.",
			MIMEType:    chartMIMEType,
		},
		func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			uri := req.Params.URI

			// Parse: ui://metrics/chart/<metric_type>?window=...&step=...&filter=...&project=...
			parsed, err := url.Parse(uri)
			if err != nil {
				return nil, fmt.Errorf("invalid chart URI %q: %w", uri, err)
			}

			// Path is /chart/<metric_type> — trim the prefix to get the metric type.
			metricType := strings.TrimPrefix(parsed.Path, "/chart/")
			if metricType == "" {
				return nil, fmt.Errorf("missing metric_type in chart URI %q", uri)
			}

			q := parsed.Query()
			windowStr := q.Get("window")
			if windowStr == "" {
				windowStr = "1h"
			}
			filter := q.Get("filter")
			stepSeconds := int64(60)
			if stepStr := q.Get("step"); stepStr != "" {
				v, stepErr := strconv.ParseInt(stepStr, 10, 64)
				if stepErr != nil || v < 10 {
					msg := fmt.Sprintf("invalid step %q (must be integer >= 10), using default 60s", stepStr)
					if req.Session != nil {
						_ = req.Session.Log(ctx, &mcp.LoggingMessageParams{
							Level:  logLevelWarning,
							Logger: "metrics_chart",
							Data:   msg,
						})
					} else {
						notifyErrLog.Load().Printf("[warning] metrics_chart: %s", msg)
					}
				} else {
					stepSeconds = v
				}
			}
			project, err := resolveProject(q.Get("project"), defaultProject)
			if err != nil {
				return nil, fmt.Errorf("resolving project: %w", err)
			}

			windowDur, err := parseWindow(windowStr)
			if err != nil {
				return nil, fmt.Errorf("parsing window: %w", err)
			}

			descriptor, err := querier.GetMetricDescriptor(ctx, project, metricType)
			if err != nil {
				return nil, fmt.Errorf("fetching metric descriptor for %q: %w", metricType, err)
			}

			meta := registry.Lookup(metricType)
			aggSpec := meta.ResolveAggregation()
			if err := aggSpec.Validate(); err != nil {
				aggSpec = metrics.DefaultAggregation(meta.Kind)
			}

			now := time.Now().UTC()
			start := now.Add(-windowDur)

			params := gcpdata.QueryTimeSeriesParams{
				Project:     project,
				MetricType:  metricType,
				LabelFilter: filter,
				Start:       start,
				End:         now,
				StepSeconds: stepSeconds,
				MetricKind:  descriptor.Kind,
				ValueType:   descriptor.ValueType,
			}

			currentSeries, _, err := querier.QueryTimeSeriesAggregated(ctx, params, aggSpec)
			if err != nil {
				return nil, fmt.Errorf("querying current window for %q: %w", metricType, err)
			}
			currentPoints := mergePoints(currentSeries)

			// Fetch previous window for baseline reference line (optional — failures
			// are logged but do not prevent the chart from rendering).
			baselineParams := params
			baselineParams.End = start
			baselineParams.Start = start.Add(-windowDur)
			var baselineMean *float64
			baselineSeries, _, bErr := querier.QueryTimeSeriesAggregated(ctx, baselineParams, aggSpec)
			if bErr != nil {
				msg := fmt.Sprintf("baseline query failed for %q: %v", metricType, bErr)
				if req.Session != nil {
					_ = req.Session.Log(ctx, &mcp.LoggingMessageParams{
						Level:  logLevelWarning,
						Logger: "metrics_chart",
						Data:   msg,
					})
				} else {
					notifyErrLog.Load().Printf("[warning] metrics_chart: %s", msg)
				}
			} else if bp := mergePoints(baselineSeries); len(bp) > 0 {
				sum := 0.0
				for _, p := range bp {
					sum += p.Value
				}
				mean := sum / float64(len(bp))
				baselineMean = &mean
			}

			htmlPage := generateChartHTML(currentPoints, baselineMean, metricType, meta.Unit, windowStr)
			return &mcp.ReadResourceResult{
				Meta: mcp.Meta{
					"ui": map[string]any{
						"csp": map[string]any{
							"resourceDomains": []string{chartCDNDomain},
						},
					},
				},
				Contents: []*mcp.ResourceContents{{
					URI:      uri,
					MIMEType: chartMIMEType,
					Text:     htmlPage,
				}},
			}, nil
		},
	)
}

// chartPoint is the compact JSON representation of a time-series point
// embedded in the HTML widget.
type chartPoint struct {
	TS int64   `json:"ts"` // Unix seconds
	V  float64 `json:"v"`
}

// generateChartHTML produces a self-contained HTML page that uses Chart.js
// (loaded from CDN) to render the time-series. All data is embedded inline.
// baseline is nil when no previous-window data was available.
func generateChartHTML(points []metrics.Point, baseline *float64, metricType, unit, window string) string {
	// Build compact point list, filtering NaN/Inf.
	pts := make([]chartPoint, 0, len(points))
	for _, p := range points {
		if !math.IsNaN(p.Value) && !math.IsInf(p.Value, 0) {
			pts = append(pts, chartPoint{TS: p.Timestamp.Unix(), V: p.Value})
		}
	}

	type chartData struct {
		Points   []chartPoint `json:"points"`
		Baseline *float64     `json:"baseline"` // nil serialises as JSON null → falsy in JS
		Unit     string       `json:"unit"`
		Window   string       `json:"window"`
	}
	dataJSON, err := json.Marshal(chartData{
		Points:   pts,
		Baseline: baseline,
		Unit:     unit,
		Window:   window,
	})
	if err != nil {
		// Should never happen: all fields are JSON-safe types. Log the bug
		// and fall back to an empty data object so the chart renders "No data available".
		notifyErrLog.Load().Printf("[error] metrics_chart: json.Marshal failed (BUG): %v", err)
		dataJSON = []byte(`{"points":[],"baseline":null,"unit":"","window":""}`)
	}
	// Escape </script> to prevent early script-tag termination in HTML context.
	safeData := strings.ReplaceAll(string(dataJSON), "</", `<\/`)

	titleHTML := html.EscapeString(metricType)

	return `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>` + titleHTML + `</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{background:var(--color-background-primary,#0d1117);color:var(--color-text-primary,#e6edf3);font-family:var(--font-sans,-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif);padding:12px 14px 10px}
.title{font-size:11px;color:var(--color-text-secondary,#8b949e);margin-bottom:8px;word-break:break-all}
.wrap{position:relative;height:240px}
.nodata{color:var(--color-text-tertiary,#6e7681);font-size:12px;padding-top:8px}
</style>
</head>
<body>
<div class="title">` + titleHTML + `</div>
<div class="wrap" id="wrap"><canvas id="c"></canvas></div>
<script>window.__D=` + safeData + `;</script>
<script src="https://cdn.jsdelivr.net/npm/chart.js@4.4.4/dist/chart.umd.min.js"></script>
<script>
(function(){
  var d=window.__D,pts=d.points||[];
  if(!pts.length){
    var p=document.createElement('p');
    p.className='nodata';
    p.textContent='No data available';
    document.getElementById('wrap').replaceChildren(p);
    return;
  }
  var fmt=function(v){
    var a=Math.abs(v);
    var s=a>=1e9?(v/1e9).toFixed(1)+'B':a>=1e6?(v/1e6).toFixed(1)+'M':a>=1e3?(v/1e3).toFixed(1)+'K':a>=10?v.toFixed(1):a>=0.1?v.toPrecision(3):v===0?'0':v.toExponential(2);
    return d.unit&&d.unit!=='1'?s+'\u00a0'+d.unit:s;
  };
  var labels=pts.map(function(p){
    var dt=new Date(p.ts*1000);
    return dt.toLocaleTimeString([],{hour:'2-digit',minute:'2-digit'});
  });
  var vals=pts.map(function(p){return p.v;});
  var cs=getComputedStyle(document.documentElement);
  var cPrimary=cs.getPropertyValue('--color-chart-line-primary').trim()||'#4e9af1';
  var cSecondary=cs.getPropertyValue('--color-chart-line-secondary').trim()||'#f0883e';
  var cTextSec=cs.getPropertyValue('--color-text-secondary').trim()||'#8b949e';
  var cTextTert=cs.getPropertyValue('--color-text-tertiary').trim()||'#6e7681';
  var cBgSec=cs.getPropertyValue('--color-background-secondary').trim()||'#161b22';
  var cBorder=cs.getPropertyValue('--color-border-default').trim()||'#30363d';
  var cGrid=cs.getPropertyValue('--color-border-subtle').trim()||'#21262d';
  var cText=cs.getPropertyValue('--color-text-primary').trim()||'#e6edf3';
  var sets=[{
    label:'current ('+d.window+')',
    data:vals,
    borderColor:cPrimary,
    backgroundColor:cPrimary+'14',
    borderWidth:2,
    pointRadius:0,
    tension:0.3,
    fill:true
  }];
  if(typeof d.baseline==='number'){
    sets.push({
      label:'baseline (prev window)',
      data:new Array(vals.length).fill(d.baseline),
      borderColor:cSecondary,
      borderWidth:1.5,
      borderDash:[6,3],
      pointRadius:0,
      fill:false
    });
  }
  new Chart(document.getElementById('c'),{
    type:'line',
    data:{labels:labels,datasets:sets},
    options:{
      responsive:true,
      maintainAspectRatio:false,
      animation:false,
      interaction:{mode:'index',intersect:false},
      plugins:{
        legend:{labels:{color:cTextSec,boxWidth:18,padding:10,font:{size:11}}},
        tooltip:{
          backgroundColor:cBgSec,
          borderColor:cBorder,
          borderWidth:1,
          titleColor:cTextSec,
          bodyColor:cText,
          callbacks:{label:function(c){return ' '+fmt(c.raw);}}
        }
      },
      scales:{
        x:{ticks:{color:cTextTert,maxTicksLimit:6,font:{size:10}},grid:{color:cGrid}},
        y:{ticks:{color:cTextTert,font:{size:10},callback:function(v){return fmt(v);}},grid:{color:cGrid}}
      }
    }
  });
})();
</script>
</body>
</html>`
}
