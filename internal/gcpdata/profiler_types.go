package gcpdata

// ProfileMeta is metadata for a single profile.
// StartTime and Duration are strings from the API (RFC3339 and duration format respectively).
// When IsDiff is true, the profile is a synthetic diff from CompareProfiles;
// StartTime, Duration, and DeploymentLabels may be empty.
type ProfileMeta struct {
	ProfileID        string            `json:"profile_id"`
	ProfileType      string            `json:"profile_type"`
	Target           string            `json:"target"`
	Duration         string            `json:"duration"`
	StartTime        string            `json:"start_time,omitempty"`
	DeploymentLabels map[string]string `json:"deployment_labels,omitempty"`
	Labels           map[string]string `json:"labels,omitempty"`
	IsDiff           bool              `json:"is_diff,omitempty"`
}

// ProfileSummary provides aggregate stats about listed profiles.
type ProfileSummary struct {
	CountByType   map[string]int `json:"count_by_type"`
	CountByTarget map[string]int `json:"count_by_target"`
}

// ProfileListResult is the response for profiler.list.
type ProfileListResult struct {
	Count         int            `json:"count"`
	Profiles      []ProfileMeta  `json:"profiles"`
	Summary       ProfileSummary `json:"summary"`
	NextPageToken string         `json:"next_page_token,omitempty"`
	ExcludedCount int            `json:"excluded_count,omitempty"`
	Warning       string         `json:"warning,omitempty"`
}

// ValueTypeInfo describes what a profile value represents.
type ValueTypeInfo struct {
	Index int    `json:"index"`
	Type  string `json:"type"`
	Unit  string `json:"unit"`
}

// TopFunction is a single entry in the top-functions ranking.
type TopFunction struct {
	FunctionName    string  `json:"function_name"`
	File            string  `json:"file,omitempty"`
	SelfValue       int64   `json:"self_value"`
	SelfPct         float64 `json:"self_pct"`
	CumulativeValue int64   `json:"cumulative_value"`
	CumulativePct   float64 `json:"cumulative_pct"`
}

// ProfileTopResult is the response for profiler.top.
type ProfileTopResult struct {
	ProfileMeta    ProfileMeta     `json:"profile_meta"`
	ValueType      ValueTypeInfo   `json:"value_type"`
	AvailableValues []ValueTypeInfo `json:"available_values"`
	TotalValue     int64           `json:"total_value"`
	TopFunctions   []TopFunction   `json:"top_functions"`
	Truncated      bool            `json:"truncated,omitempty"`
	TruncationHint string          `json:"truncation_hint,omitempty"`
}

// PeekEntry is a caller or callee of the target function.
type PeekEntry struct {
	Name string  `json:"name"`
	File string  `json:"file,omitempty"`
	Cost int64   `json:"cost"`
	Pct  float64 `json:"pct"`
}

// PeekFunctionInfo describes the target function in a peek result.
type PeekFunctionInfo struct {
	Name       string `json:"name"`
	File       string `json:"file,omitempty"`
	Self       int64  `json:"self"`
	Cumulative int64  `json:"cumulative"`
}

// ProfilePeekResult is the response for profiler.peek.
type ProfilePeekResult struct {
	ProfileMeta      ProfileMeta      `json:"profile_meta"`
	ValueType        ValueTypeInfo    `json:"value_type"`
	Function         PeekFunctionInfo `json:"function"`
	Callers          []PeekEntry      `json:"callers"`
	Callees          []PeekEntry      `json:"callees"`
	CallersTruncated bool             `json:"callers_truncated,omitempty"`
	CalleesTruncated bool             `json:"callees_truncated,omitempty"`
	TruncationHint   string           `json:"truncation_hint,omitempty"`
}

// FlamegraphNode is a node in a bounded call tree.
type FlamegraphNode struct {
	Name       string           `json:"name"`
	File       string           `json:"file,omitempty"`
	Self       int64            `json:"self"`
	Cumulative int64            `json:"cumulative"`
	Pct        float64          `json:"pct"`
	Children   []FlamegraphNode `json:"children,omitempty"`
}

// ProfileFlamegraphResult is the response for profiler.flamegraph.
type ProfileFlamegraphResult struct {
	ProfileMeta ProfileMeta    `json:"profile_meta"`
	ValueType   ValueTypeInfo  `json:"value_type"`
	TotalValue  int64          `json:"total_value"`
	Root        FlamegraphNode `json:"root"`
	MaxDepth    int            `json:"max_depth"`
	MinPct      float64        `json:"min_pct"`
	PrunedNodes int            `json:"pruned_nodes,omitempty"`
	Warning     string         `json:"warning,omitempty"`
}

// CompareTopEntry is a function entry in the compare result showing the delta.
type CompareTopEntry struct {
	Name     string  `json:"name"`
	File     string  `json:"file,omitempty"`
	Delta    int64   `json:"delta"`
	DeltaPct float64 `json:"delta_pct"`
}

// CompareSummary provides high-level stats about the profile diff.
type CompareSummary struct {
	TotalCurrent int64 `json:"total_current"`
	TotalBase    int64 `json:"total_base"`
	TotalDelta   int64 `json:"total_delta"`
	TotalDeltaPct float64 `json:"total_delta_pct"`
	FunctionsRegressed int `json:"functions_regressed"`
	FunctionsImproved  int `json:"functions_improved"`
}

// ProfileCompareResult is the response for profiler.compare.
type ProfileCompareResult struct {
	DiffID          string            `json:"diff_id"`
	CurrentMeta     ProfileMeta       `json:"current_meta"`
	BaseMeta        ProfileMeta       `json:"base_meta"`
	ValueType       ValueTypeInfo     `json:"value_type"`
	Summary         CompareSummary    `json:"summary"`
	TopRegressions  []CompareTopEntry `json:"top_regressions"`
	TopImprovements []CompareTopEntry `json:"top_improvements"`
	Truncated       bool              `json:"truncated,omitempty"`
	TruncationHint  string            `json:"truncation_hint,omitempty"`
	Warning         string            `json:"warning,omitempty"`
}

// TrendsDataPoint is a single point in a function's history over time.
type TrendsDataPoint struct {
	ProfileID string  `json:"profile_id"`
	Timestamp string  `json:"timestamp"`
	SelfValue int64   `json:"self_value"`
	SelfPct   float64 `json:"self_pct"`
	CumulativeValue int64   `json:"cumulative_value"`
	CumulativePct   float64 `json:"cumulative_pct"`
}

// TrendsFunctionSeries is the time series for one function.
type TrendsFunctionSeries struct {
	Name       string            `json:"name"`
	File       string            `json:"file,omitempty"`
	DataPoints []TrendsDataPoint `json:"data_points"`
}

// ProfileTrendsResult is the response for profiler.trends.
type ProfileTrendsResult struct {
	Target            string                 `json:"target"`
	ProfileType       string                 `json:"profile_type"`
	ValueType         ValueTypeInfo          `json:"value_type"`
	ProfileCount      int                    `json:"profile_count"`
	AnalyzedCount     int                    `json:"analyzed_count"`
	TimeRangeStart    string                 `json:"time_range_start,omitempty"`
	TimeRangeEnd      string                 `json:"time_range_end,omitempty"`
	Functions         []TrendsFunctionSeries `json:"functions"`
	DownloadErrors    int                    `json:"download_errors,omitempty"`
	LastDownloadError string                 `json:"last_download_error,omitempty"`
	Warning           string                 `json:"warning,omitempty"`
	Truncated         bool                   `json:"truncated,omitempty"`
	TruncationHint    string                 `json:"truncation_hint,omitempty"`
}
