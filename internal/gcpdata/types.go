package gcpdata

// LogEntry represents a normalized log entry for LLM consumption.
type LogEntry struct {
	Timestamp   string            `json:"timestamp"`
	Severity    string            `json:"severity"`
	LogName     string            `json:"log_name"`
	InsertID    string            `json:"insert_id"`
	TextPayload string            `json:"text_payload,omitempty"`
	JSONPayload map[string]any    `json:"json_payload,omitempty"`
	Resource    *ResourceInfo     `json:"resource,omitempty"`
	HTTPRequest *HTTPRequestInfo  `json:"http_request,omitempty"`
	Trace       string            `json:"trace,omitempty"`
	SpanID      string            `json:"span_id,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Operation   *OperationInfo    `json:"operation,omitempty"`
}

// ResourceInfo describes the monitored resource that produced a log entry.
type ResourceInfo struct {
	Type   string            `json:"type"`
	Labels map[string]string `json:"labels,omitempty"`
}

// HTTPRequestInfo contains HTTP request details from a log entry.
type HTTPRequestInfo struct {
	Method       string `json:"method,omitempty"`
	URL          string `json:"url,omitempty"`
	Status       int    `json:"status,omitempty"`
	ResponseSize int64  `json:"response_size,omitempty"`
	UserAgent    string `json:"user_agent,omitempty"`
	RemoteIP     string `json:"remote_ip,omitempty"`
	Latency      string `json:"latency,omitempty"`
}

// OperationInfo describes an operation associated with a log entry.
type OperationInfo struct {
	ID       string `json:"id,omitempty"`
	Producer string `json:"producer,omitempty"`
}

// LogQueryResult is the response for log queries.
type LogQueryResult struct {
	Count         int        `json:"count"`
	Entries       []LogEntry `json:"entries"`
	NextPageToken string     `json:"next_page_token,omitempty"`
}

// RequestInfo represents an HTTP request found in logs.
type RequestInfo struct {
	Timestamp    string `json:"timestamp"`
	Method       string `json:"method"`
	URL          string `json:"url"`
	Status       int    `json:"status"`
	Latency      string `json:"latency,omitempty"`
	TraceID      string `json:"trace_id,omitempty"`
	RequestID    string `json:"request_id,omitempty"`
	Service      string `json:"service"`
	ResourceType string `json:"resource_type"`
}

// RequestList is the response for logs.find_requests.
type RequestList struct {
	Count    int           `json:"count"`
	Requests []RequestInfo `json:"requests"`
}

// ErrorGroup represents an aggregated error group from Error Reporting.
type ErrorGroup struct {
	GroupID          string   `json:"group_id"`
	Service          string   `json:"service"`
	Message          string   `json:"message"`
	Count            int64    `json:"count"`
	FirstSeen        string   `json:"first_seen"`
	LastSeen         string   `json:"last_seen"`
	AffectedVersions []string `json:"affected_versions,omitempty"`
}

// ErrorGroupList is the response for errors.list.
type ErrorGroupList struct {
	Count  int          `json:"count"`
	Groups []ErrorGroup `json:"groups"`
}

// ErrorInstance represents a single error event.
type ErrorInstance struct {
	Timestamp string `json:"timestamp"`
	Message   string `json:"message"`
	Service   string `json:"service,omitempty"`
	Version   string `json:"version,omitempty"`
}

// ErrorGroupDetail is the response for errors.get.
type ErrorGroupDetail struct {
	GroupID   string          `json:"group_id"`
	Message   string          `json:"message"`
	Service   string          `json:"service"`
	Instances []ErrorInstance `json:"instances"`
}

// ServiceInfo represents a discovered service in the project.
type ServiceInfo struct {
	Name         string `json:"name"`
	ResourceType string `json:"resource_type"`
	Namespace    string `json:"namespace,omitempty"`
}

// ServiceList is the response for logs.services.
type ServiceList struct {
	Count    int           `json:"count"`
	Services []ServiceInfo `json:"services"`
}

// LogsSummary is the response for logs.summary.
type LogsSummary struct {
	TotalEntries         int            `json:"total_entries"`
	SeverityDistribution map[string]int `json:"severity_distribution"`
	TopServices          []ServiceCount `json:"top_services"`
	TopErrors            []ErrorSample  `json:"top_errors"`
	SampleEntries        []LogEntry     `json:"sample_entries"`
	Truncated            bool           `json:"truncated"`
}

// ServiceCount is a service with its log entry count.
type ServiceCount struct {
	Service string `json:"service"`
	Count   int    `json:"count"`
}

// ErrorSample is an error message with its occurrence count.
type ErrorSample struct {
	Message string `json:"message"`
	Count   int    `json:"count"`
}

// TraceSpan represents a single span within a trace, with nested children.
type TraceSpan struct {
	SpanID    string            `json:"span_id"`
	Name      string            `json:"name"`
	Kind      string            `json:"kind,omitempty"`
	StartTime string            `json:"start_time"`
	EndTime   string            `json:"end_time"`
	Duration  string            `json:"duration"`
	Labels    map[string]string `json:"labels,omitempty"`
	Children  []TraceSpan       `json:"children,omitempty"`
}

// TraceDetail is the response for trace.get.
type TraceDetail struct {
	TraceID string      `json:"trace_id"`
	Count   int         `json:"span_count"`
	Spans   []TraceSpan `json:"spans"`
}
