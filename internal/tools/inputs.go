package tools

// TimeFilterInput is embedded by tools that accept start_time/end_time.
type TimeFilterInput struct {
	StartTime string `json:"start_time,omitempty" jsonschema:"Start of time range in RFC3339 format (e.g. '2025-01-15T00:00:00Z'). Defaults to 24 hours ago, or 24 hours before end_time if only end_time is provided."`
	EndTime   string `json:"end_time,omitempty"   jsonschema:"End of time range in RFC3339 format (e.g. '2025-01-15T23:59:59Z'). If omitted, only the start bound is applied (open-ended towards now)."`
}

// ProjectInput is embedded by tools that accept project_id.
type ProjectInput struct {
	ProjectID string `json:"project_id,omitempty" jsonschema:"GCP project ID (uses default if not specified)"`
}

// LogsQueryInput is the input for logs.query.
type LogsQueryInput struct {
	ProjectInput
	TimeFilterInput
	Filter    string `json:"filter"              jsonschema:"Cloud Logging filter expression (e.g. 'severity>=ERROR', 'resource.type=\"k8s_container\"')"`
	Limit     int    `json:"limit,omitempty"     jsonschema:"Maximum number of log entries to return (default 100, server max applies)"`
	Order     string `json:"order,omitempty"     jsonschema:"Sort order by timestamp (default 'desc'). One of: asc, desc"`
	PageToken string `json:"page_token,omitempty" jsonschema:"Page token for pagination"`
}

// LogsByTraceInput is the input for logs.by_trace.
type LogsByTraceInput struct {
	ProjectInput
	TimeFilterInput
	TraceID   string `json:"trace_id"             jsonschema:"The trace ID (32-character hex string, not the full resource path)"`
	Limit     int    `json:"limit,omitempty"      jsonschema:"Maximum number of log entries to return (default 100, server max applies)"`
	PageToken string `json:"page_token,omitempty" jsonschema:"Page token for pagination (from previous response's next_page_token)"`
}

// LogsByRequestIDInput is the input for logs.by_request_id.
type LogsByRequestIDInput struct {
	ProjectInput
	TimeFilterInput
	RequestID string `json:"request_id"           jsonschema:"The request ID to search for"`
	Limit     int    `json:"limit,omitempty"      jsonschema:"Maximum number of log entries to return (default 100, server max applies)"`
	PageToken string `json:"page_token,omitempty" jsonschema:"Page token for pagination (from previous response's next_page_token)"`
}

// LogsFindRequestsInput is the input for logs.find_requests.
type LogsFindRequestsInput struct {
	ProjectInput
	TimeFilterInput
	URLPattern string `json:"url_pattern"          jsonschema:"URL substring to match (e.g. '/api/profile', '/v1/connect')"`
	Method     string `json:"method,omitempty"     jsonschema:"HTTP method filter (one of: GET, POST, PUT, PATCH, DELETE, HEAD, OPTIONS)"`
	StatusCode int    `json:"status_code,omitempty" jsonschema:"HTTP status code filter (e.g. 500, 404). Range: 100-599"`
	TracedOnly bool   `json:"traced_only,omitempty" jsonschema:"Only return requests that have a trace_id (default false)"`
	Limit      int    `json:"limit,omitempty"      jsonschema:"Maximum number of requests to return (default 20, server max applies)"`
}

// LogsK8sInput is the input for logs.k8s.
type LogsK8sInput struct {
	ProjectInput
	TimeFilterInput
	Namespace     string `json:"namespace,omitempty"      jsonschema:"Kubernetes namespace name"`
	PodName       string `json:"pod_name,omitempty"       jsonschema:"Pod name (supports substring match)"`
	ContainerName string `json:"container_name,omitempty" jsonschema:"Container name"`
	Severity      string `json:"severity,omitempty"       jsonschema:"Minimum log severity level to return (one of: DEFAULT, DEBUG, INFO, NOTICE, WARNING, ERROR, CRITICAL, ALERT, EMERGENCY)"`
	TextSearch    string `json:"text_search,omitempty"    jsonschema:"Text to search for in log payloads"`
	Limit         int    `json:"limit,omitempty"          jsonschema:"Maximum number of log entries to return (default 100, server max applies)"`
	Order         string `json:"order,omitempty"          jsonschema:"Sort order by timestamp (default 'desc'). One of: asc, desc"`
	PageToken     string `json:"page_token,omitempty"     jsonschema:"Page token for pagination (from previous response's next_page_token)"`
}

// LogsServicesInput is the input for logs.services.
type LogsServicesInput struct {
	ProjectInput
	TimeFilterInput
}

// LogsSummaryInput is the input for logs.summary.
type LogsSummaryInput struct {
	ProjectInput
	TimeFilterInput
	Filter string `json:"filter,omitempty" jsonschema:"Additional Cloud Logging filter to narrow the scope"`
}

// ErrorsListInput is the input for errors.list.
// Note: Error Reporting only supports lookback periods ending at now, so
// start_time/end_time are intentionally absent. Use time_range_hours instead.
type ErrorsListInput struct {
	ProjectInput
	TimeRangeHours int    `json:"time_range_hours,omitempty" jsonschema:"Time range in hours to look back (default 24, max 720). Error Reporting only supports lookback periods ending now."`
	Limit          int    `json:"limit,omitempty"            jsonschema:"Maximum number of error groups to return (default 50, server max applies)"`
	ServiceFilter  string `json:"service_filter,omitempty"   jsonschema:"Filter by service name"`
	VersionFilter  string `json:"version_filter,omitempty"   jsonschema:"Filter by service version"`
}

// ErrorsGetInput is the input for errors.get.
type ErrorsGetInput struct {
	ProjectInput
	GroupID   string `json:"group_id" jsonschema:"Error group ID (from errors.list results)"`
	Limit     int    `json:"limit,omitempty" jsonschema:"Maximum number of error events to return (default 20, server max applies)"`
	PageToken string `json:"page_token,omitempty" jsonschema:"Page token for pagination (from previous response's next_page_token)"`
}

// TraceGetInput is the input for trace.get.
type TraceGetInput struct {
	ProjectInput
	TraceID string `json:"trace_id" jsonschema:"The trace ID (32-character hex string, not the full resource path)"`
}

// MetricsListInput is the input for metrics.list.
type MetricsListInput struct {
	ProjectInput
	Match string `json:"match,omitempty" jsonschema:"Substring to filter metric names or semantic keywords (e.g. 'cpu', 'latency', 'queue', 'cache', 'database', 'pubsub')"`
	Kind  string `json:"kind,omitempty"  jsonschema:"Filter by metric kind (one of: latency, throughput, error_rate, resource_utilization, saturation, availability, freshness, business_kpi)"`
	Limit int    `json:"limit,omitempty" jsonschema:"Maximum number of metrics to return (default 50, max 200)"`
}

// MetricsSnapshotInput is the input for metrics.snapshot.
type MetricsSnapshotInput struct {
	ProjectInput
	MetricType   string `json:"metric_type"            jsonschema:"Full Cloud Monitoring metric type (e.g. 'compute.googleapis.com/instance/cpu/utilization')"`
	Filter       string `json:"filter,omitempty"       jsonschema:"Additional Cloud Monitoring label filter. Labels live in two namespaces: metric.labels.* and resource.labels.*."`
	Window       string `json:"window,omitempty"       jsonschema:"Time window to analyze (default '1h'). One of: 15m, 30m, 1h, 3h, 6h, 24h"`
	BaselineMode string `json:"baseline_mode,omitempty" jsonschema:"Baseline comparison mode (default 'prev_window'). One of: prev_window, same_weekday_hour, pre_event"`
	EventTime    string `json:"event_time,omitempty"   jsonschema:"Event time in RFC3339 format, required when baseline_mode is 'pre_event'"`
	StepSeconds  int    `json:"step_seconds,omitempty" jsonschema:"Alignment period in seconds (default 60, minimum 10)"`
}

// MetricsTopInput is the input for metrics.top_contributors.
type MetricsTopInput struct {
	ProjectInput
	MetricType   string `json:"metric_type"            jsonschema:"Full Cloud Monitoring metric type"`
	Dimension    string `json:"dimension"              jsonschema:"Label key to group by (e.g. 'metric.labels.response_code', 'resource.labels.instance_id')"`
	Filter       string `json:"filter,omitempty"       jsonschema:"Additional Cloud Monitoring label filter"`
	Window       string `json:"window,omitempty"       jsonschema:"Time window to analyze (default '1h'). One of: 15m, 30m, 1h, 3h, 6h, 24h"`
	BaselineMode string `json:"baseline_mode,omitempty" jsonschema:"Baseline comparison mode (default 'prev_window'). One of: prev_window, same_weekday_hour, pre_event"`
	EventTime    string `json:"event_time,omitempty"   jsonschema:"Event time in RFC3339 for pre_event baseline"`
	Limit        int    `json:"limit,omitempty"        jsonschema:"Maximum number of contributors to return (default 5, max 20)"`
}

// MetricsRelatedInput is the input for metrics.related.
type MetricsRelatedInput struct {
	ProjectInput
	MetricType string `json:"metric_type"      jsonschema:"Full Cloud Monitoring metric type"`
	Filter     string `json:"filter,omitempty" jsonschema:"Additional Cloud Monitoring label filter"`
	Window     string `json:"window,omitempty" jsonschema:"Time window to analyze (default '1h'). One of: 15m, 30m, 1h, 3h, 6h, 24h"`
}

// ProfilerListInput is the input for profiler.list.
type ProfilerListInput struct {
	ProjectInput
	TimeFilterInput
	ProfileType string `json:"profile_type,omitempty" jsonschema:"Profile type filter, case-insensitive (one of: CPU, WALL, HEAP, THREADS, CONTENTION, PEAK_HEAP, HEAP_ALLOC)"`
	Target      string `json:"target,omitempty"       jsonschema:"Deployment target (service name) filter"`
	Limit       int    `json:"limit,omitempty"        jsonschema:"Number of profiles to return (default 20, max 100)"`
	PageToken   string `json:"page_token,omitempty"   jsonschema:"Page token for pagination"`
}

// ProfilerTopInput is the input for profiler.top.
type ProfilerTopInput struct {
	ProjectInput
	ProfileID  string `json:"profile_id"             jsonschema:"Profile ID from profiler.list results, or diff_id from profiler.compare results"`
	Limit      int    `json:"limit,omitempty"         jsonschema:"Maximum number of functions to return (default 20, max 50)"`
	SortBy     string `json:"sort_by,omitempty"       jsonschema:"Sort by 'self' or 'cumulative' cost (default 'cumulative')"`
	ValueIndex int    `json:"value_index,omitempty"   jsonschema:"Value index for multi-value profiles (default 0). Use profiler.top once to see available_values."`
	Filter     string `json:"filter,omitempty"        jsonschema:"Substring filter on function name or file path (e.g. 'mypackage/handler')"`
}

// ProfilerPeekInput is the input for profiler.peek.
type ProfilerPeekInput struct {
	ProjectInput
	ProfileID    string `json:"profile_id"             jsonschema:"Profile ID from profiler.list results, or diff_id from profiler.compare results"`
	FunctionName string `json:"function_name"          jsonschema:"Function name to inspect (from profiler.top results). Substring match."`
	Limit        int    `json:"limit,omitempty"         jsonschema:"Max callers/callees to return (default 10, max 30)"`
	ValueIndex   int    `json:"value_index,omitempty"   jsonschema:"Value index for multi-value profiles (default 0)"`
}

// ProfilerFlamegraphInput is the input for profiler.flamegraph.
type ProfilerFlamegraphInput struct {
	ProjectInput
	ProfileID    string  `json:"profile_id"              jsonschema:"Profile ID from profiler.list results, or diff_id from profiler.compare results"`
	RootFunction string  `json:"root_function,omitempty"  jsonschema:"Function to use as subtree root (omit for full profile root). Substring match."`
	MaxDepth     int     `json:"max_depth,omitempty"      jsonschema:"Maximum tree depth to return (default 3, max 6)"`
	MinPct       float64 `json:"min_pct,omitempty"        jsonschema:"Minimum percentage of total to include a node (default 1.0)"`
	ValueIndex   int     `json:"value_index,omitempty"    jsonschema:"Value index for multi-value profiles (default 0)"`
}

// ProfilerCompareInput is the input for profiler.compare.
type ProfilerCompareInput struct {
	ProjectInput
	ProfileID     string `json:"profile_id"              jsonschema:"Current profile ID from profiler.list (diff_id is not supported here)"`
	BaseProfileID string `json:"base_profile_id"         jsonschema:"Base profile ID to compare against from profiler.list (diff_id is not supported here)"`
	ValueIndex    int    `json:"value_index,omitempty"    jsonschema:"Value index for multi-value profiles (default 0)"`
}

// ProfilerTrendsInput is the input for profiler.trends.
type ProfilerTrendsInput struct {
	ProjectInput
	ProfileType    string `json:"profile_type"            jsonschema:"Profile type, case-insensitive (one of: CPU, WALL, HEAP, THREADS, CONTENTION, PEAK_HEAP, HEAP_ALLOC)"`
	Target         string `json:"target"                  jsonschema:"Deployment target (service name)"`
	FunctionFilter string `json:"function_filter,omitempty" jsonschema:"Substring filter to track specific functions (e.g. 'mypackage/handler'). If omitted, tracks top functions by peak cost."`
	ValueIndex     int    `json:"value_index,omitempty"    jsonschema:"Value index for multi-value profiles (default 0)"`
	MaxProfiles    int    `json:"max_profiles,omitempty"   jsonschema:"Maximum number of profiles to analyze (default 30, max 100). More profiles = wider time range."`
	MaxFunctions   int    `json:"max_functions,omitempty"  jsonschema:"Maximum number of functions to track (default 10, max 20)"`
}

// MetricsCompareInput is the input for metrics.compare.
type MetricsCompareInput struct {
	ProjectInput
	MetricType   string `json:"metric_type"              jsonschema:"Full Cloud Monitoring metric type"`
	Filter       string `json:"filter,omitempty"         jsonschema:"Additional Cloud Monitoring label filter"`
	WindowAFrom  string `json:"window_a_from"            jsonschema:"Start of window A in RFC3339 format"`
	WindowATo    string `json:"window_a_to"              jsonschema:"End of window A in RFC3339 format"`
	WindowBFrom  string `json:"window_b_from"            jsonschema:"Start of window B in RFC3339 format"`
	WindowBTo    string `json:"window_b_to"              jsonschema:"End of window B in RFC3339 format"`
	WindowALabel string `json:"window_a_label,omitempty" jsonschema:"Label for window A (default 'window_a')"`
	WindowBLabel string `json:"window_b_label,omitempty" jsonschema:"Label for window B (default 'window_b')"`
}
