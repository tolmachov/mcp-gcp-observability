package gcpdata

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"

	monitoring "cloud.google.com/go/monitoring/apiv3/v2"
	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
)

type MetricDescriptorInfo struct {
	Type        string            `json:"type"`
	DisplayName string            `json:"display_name"`
	Description string            `json:"description,omitempty"`
	MetricKind  string            `json:"metric_kind"`
	ValueType   string            `json:"value_type"`
	Unit        string            `json:"unit,omitempty"`
	Labels      []LabelDescriptor `json:"labels,omitempty"`
	// MonitoredResourceTypes lists the monitored resource types this metric
	// can be recorded against (e.g. ["pubsub_subscription"] for a subscription
	// metric). The resource labels available for filtering are defined by
	// these types — fetch them via ListMonitoredResourceDescriptors /
	// MetricsQuerier.GetResourceLabels. Almost every metric binds to a single
	// resource type but the API allows multiple and we preserve the full list.
	MonitoredResourceTypes []string `json:"monitored_resource_types,omitempty"`
}

type LabelDescriptor struct {
	Key         string `json:"key"`
	Description string `json:"description,omitempty"`
}

// MonitoredResourceDescriptor describes a monitored resource type and the
// labels it exposes for filtering (e.g. type="pubsub_subscription",
// labels=["project_id", "subscription_id"]). Callers combine this with a
// metric's MonitoredResourceTypes to answer "which resource.labels.* can I
// use with this metric".
type MonitoredResourceDescriptor struct {
	Type        string   `json:"type"`
	DisplayName string   `json:"display_name,omitempty"`
	Labels      []string `json:"labels,omitempty"`
}

// metricsQueryTimeout is the maximum time for a single Cloud Monitoring query.
const metricsQueryTimeout = 30 * time.Second

// MaxTimeSeries is the maximum number of time series returned by QueryTimeSeries
// to prevent runaway memory usage on high-cardinality queries.
const MaxTimeSeries = 500

// MetricTimeSeries holds a single time series. Cloud Monitoring exposes
// labels in four namespaces, all of which can be used as group_by_fields or
// label filters:
//   - metric.labels.*           → MetricLabels
//   - resource.labels.*         → ResourceLabels
//   - metadata.system_labels.*  → MetadataSystemLabels (e.g. GCE instance
//     name/state/zone set by the platform, not user-defined labels)
//   - metadata.user_labels.*    → MetadataUserLabels (user-defined resource
//     labels like GCE labels applied via `gcloud compute instances ...`)
//
// Metadata labels are only returned by the API when they're referenced in
// group_by_fields or the request view is FULL; otherwise both metadata maps
// are nil.
type MetricTimeSeries struct {
	MetricLabels         map[string]string `json:"metric_labels,omitempty"`
	ResourceLabels       map[string]string `json:"resource_labels,omitempty"`
	MetadataSystemLabels map[string]string `json:"metadata_system_labels,omitempty"`
	MetadataUserLabels   map[string]string `json:"metadata_user_labels,omitempty"`
	MetricKind           string            `json:"metric_kind"`
	ValueType            string            `json:"value_type"`
	Points               []metrics.Point   `json:"points"`
	Truncated            bool              `json:"truncated,omitempty"`
	// UnsupportedCount is the number of points in the upstream series that
	// had a value type this tool does not decode (e.g. BOOL, STRING, or a
	// future type not covered by extractValue). Points with usable values
	// land in Points as usual; this counter lets downstream consumers
	// surface lossy decoding without dropping the whole series.
	UnsupportedCount int `json:"unsupported_count,omitempty"`
}

// MetricDescriptorBasic contains fields needed for aligner selection and response enrichment.
// Everything from one ListMetricDescriptors call; no second RPC needed.
type MetricDescriptorBasic struct {
	Kind      string // GAUGE, DELTA, CUMULATIVE
	ValueType string // INT64, DOUBLE, DISTRIBUTION, BOOL, STRING
	// Labels are the keys available under metric.labels.* for this metric.
	// May be empty for metrics that expose no metric-level labels.
	Labels []LabelDescriptor
	// MonitoredResourceTypes lists the monitored resource types this metric
	// can be recorded against. Combine with MetricsQuerier.GetResourceLabels
	// to discover the keys available under resource.labels.*. Almost every
	// metric binds to a single type; the API allows multiple.
	MonitoredResourceTypes []string
}

// GetMetricDescriptor returns kind, value_type, labels, and resource types.
// Kind+ValueType determine the valid aligner (e.g., ALIGN_RATE rejected for DELTA+DISTRIBUTION).
func GetMetricDescriptor(ctx context.Context, client *monitoring.MetricClient, project, metricType string) (MetricDescriptorBasic, error) {
	filter := fmt.Sprintf(`metric.type = "%s"`, EscapeFilterValue(metricType))
	descriptors, err := ListMetricDescriptors(ctx, client, project, filter, 1)
	if err != nil {
		return MetricDescriptorBasic{}, err
	}
	if len(descriptors) == 0 {
		return MetricDescriptorBasic{}, fmt.Errorf("metric descriptor not found for %q in project %q", metricType, project)
	}
	d := descriptors[0]
	return MetricDescriptorBasic{
		Kind:                   d.MetricKind,
		ValueType:              d.ValueType,
		Labels:                 append([]LabelDescriptor(nil), d.Labels...),
		MonitoredResourceTypes: append([]string(nil), d.MonitoredResourceTypes...),
	}, nil
}

func ListMetricDescriptors(ctx context.Context, client *monitoring.MetricClient, project, filter string, limit int) ([]MetricDescriptorInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, metricsQueryTimeout)
	defer cancel()

	req := &monitoringpb.ListMetricDescriptorsRequest{
		Name:   fmt.Sprintf("projects/%s", project),
		Filter: filter,
	}

	var result []MetricDescriptorInfo
	it := client.ListMetricDescriptors(ctx, req)
	for i := 0; limit <= 0 || i < limit; i++ {
		desc, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("listing metric descriptors: %w", err)
		}
		info := MetricDescriptorInfo{
			Type:                   desc.Type,
			DisplayName:            desc.DisplayName,
			Description:            desc.Description,
			MetricKind:             desc.MetricKind.String(),
			ValueType:              desc.ValueType.String(),
			Unit:                   desc.Unit,
			MonitoredResourceTypes: append([]string(nil), desc.MonitoredResourceTypes...),
		}
		for _, l := range desc.Labels {
			info.Labels = append(info.Labels, LabelDescriptor{
				Key:         l.Key,
				Description: l.Description,
			})
		}
		result = append(result, info)
	}
	return result, nil
}

// ListMonitoredResourceDescriptors returns all monitored resource descriptors
// visible to the project, each with its defined label keys. Results are
// globally stable (the resource schema is part of the Cloud Monitoring API,
// not per-project state), so callers should cache.
func ListMonitoredResourceDescriptors(ctx context.Context, client *monitoring.MetricClient, project string) ([]MonitoredResourceDescriptor, error) {
	ctx, cancel := context.WithTimeout(ctx, metricsQueryTimeout)
	defer cancel()

	req := &monitoringpb.ListMonitoredResourceDescriptorsRequest{
		Name: fmt.Sprintf("projects/%s", project),
	}
	it := client.ListMonitoredResourceDescriptors(ctx, req)
	var result []MonitoredResourceDescriptor
	for {
		desc, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("listing monitored resource descriptors: %w", err)
		}
		labels := make([]string, 0, len(desc.Labels))
		for _, l := range desc.Labels {
			labels = append(labels, l.Key)
		}
		result = append(result, MonitoredResourceDescriptor{
			Type:        desc.Type,
			DisplayName: desc.DisplayName,
			Labels:      labels,
		})
	}
	return result, nil
}

type QueryTimeSeriesParams struct {
	Project     string
	MetricType  string
	LabelFilter string
	Start       time.Time
	End         time.Time
	StepSeconds int64
	MetricKind  string
	// ValueType is the metric descriptor's value type (INT64, DOUBLE,
	// DISTRIBUTION, BOOL). Combined with MetricKind, it determines the
	// per-series aligner — critical because some combinations (e.g.
	// DELTA+DISTRIBUTION) reject ALIGN_RATE and require ALIGN_MEAN.
	//
	// Callers MUST set this field alongside MetricKind. Leaving it empty
	// falls back to the kind-only aligner choice, which will be rejected
	// by the Cloud Monitoring API for distribution metrics and is the
	// exact bug this field exists to prevent. Always source both Kind and
	// ValueType from the same MetricDescriptorBasic returned by
	// GetMetricDescriptor — do not construct QueryTimeSeriesParams by
	// hand with only the kind.
	ValueType     string
	GroupByFields []string
	Reducer       monitoringpb.Aggregation_Reducer
}

// QueryTimeSeries fetches time series data from Cloud Monitoring.
// Returns at most MaxTimeSeries real series. If the result was truncated,
// a sentinel MetricTimeSeries{Truncated: true} is appended as the final
// element; it carries no Points and should be excluded from data aggregation.
func QueryTimeSeries(ctx context.Context, client *monitoring.MetricClient, params QueryTimeSeriesParams) ([]MetricTimeSeries, error) {
	ctx, cancel := context.WithTimeout(ctx, metricsQueryTimeout)
	defer cancel()

	filter := fmt.Sprintf(`metric.type = "%s"`, EscapeFilterValue(params.MetricType))
	if params.LabelFilter != "" {
		filter += " AND " + params.LabelFilter
	}

	stepSeconds := params.StepSeconds
	if stepSeconds <= 0 {
		stepSeconds = 60
	}

	agg := buildAggregation(params.MetricKind, params.ValueType, stepSeconds, params.GroupByFields, params.Reducer)

	req := &monitoringpb.ListTimeSeriesRequest{
		Name:   fmt.Sprintf("projects/%s", params.Project),
		Filter: filter,
		Interval: &monitoringpb.TimeInterval{
			StartTime: timestamppb.New(params.Start),
			EndTime:   timestamppb.New(params.End),
		},
		Aggregation: agg,
		View:        monitoringpb.ListTimeSeriesRequest_FULL,
	}

	var result []MetricTimeSeries
	it := client.ListTimeSeries(ctx, req)
	for {
		ts, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("listing time series: %w", err)
		}

		mts := MetricTimeSeries{
			MetricKind: ts.MetricKind.String(),
			ValueType:  ts.ValueType.String(),
		}
		if ts.Metric != nil {
			mts.MetricLabels = ts.Metric.Labels
		}
		if ts.Resource != nil {
			mts.ResourceLabels = ts.Resource.Labels
		}
		// Metadata labels are populated when the request group_by_fields
		// or filter references metadata.system_labels.* / metadata.user_labels.*.
		// System labels come back as a protobuf Struct with heterogeneous
		// value kinds (string, number, bool, null, nested struct/list).
		// flattenStructValue below handles each kind explicitly so empty
		// strings survive, null/unset values are dropped, and numeric/bool
		// values are formatted deterministically (no fmt.Sprint(interface{})
		// which would render nil as "<nil>" into a label value).
		if ts.Metadata != nil {
			if sl := ts.Metadata.GetSystemLabels(); sl != nil && len(sl.GetFields()) > 0 {
				mts.MetadataSystemLabels = make(map[string]string, len(sl.GetFields()))
				for k, v := range sl.GetFields() {
					if s, ok := flattenStructValue(v); ok {
						mts.MetadataSystemLabels[k] = s
					}
				}
				if len(mts.MetadataSystemLabels) == 0 {
					mts.MetadataSystemLabels = nil
				}
			}
			if ul := ts.Metadata.GetUserLabels(); len(ul) > 0 {
				mts.MetadataUserLabels = ul
			}
		}

		for _, p := range ts.Points {
			if p.Interval == nil || p.Interval.EndTime == nil || p.Value == nil {
				continue
			}
			val, ok := extractValue(p.Value)
			if !ok {
				mts.UnsupportedCount++
				continue
			}
			mts.Points = append(mts.Points, metrics.Point{
				Timestamp: p.Interval.EndTime.AsTime(),
				Value:     val,
			})
		}

		result = append(result, mts)

		if len(result) >= MaxTimeSeries {
			// Append a zero-valued sentinel to signal that the result was
			// truncated. Using a sentinel rather than marking the last real
			// series avoids the ambiguity of Truncated meaning "this series'
			// own data is incomplete" vs "the result set was cut off here".
			// The sentinel has no Points, so mergePoints ignores its data.
			result = append(result, MetricTimeSeries{Truncated: true})
			break
		}
	}
	return result, nil
}

// AggregationWarnings describes non-fatal issues encountered while running
// a two-stage aggregation. Callers (snapshot/compare/related tool handlers)
// forward these through mcpLog so operators can see registry typos and
// sparse group coverage without trawling stderr. Zero value = no warnings.
type AggregationWarnings struct {
	// SingleGroup is set when a two-stage query was requested but the
	// upstream returned exactly one group. Legitimate when the window
	// genuinely has one entity (single game, single tenant); almost
	// always a registry typo otherwise (label qualifier missing).
	SingleGroup bool

	// CarryForwardBuckets counts timestamps where at least one series
	// contributed a carried-forward value (within the
	// maxCarryForwardBuckets bound) instead of a fresh point. Non-zero
	// means transient publishing gaps the fold smoothed over — usable
	// numbers but trend/spike detection may be noisy.
	CarryForwardBuckets int

	// DepartedGroupBuckets counts timestamps where at least one series
	// has been treated as departed (carry bound exhausted) and was
	// excluded from the fold entirely. Non-zero means a group went
	// permanently silent mid-window — common during deploy cutovers,
	// tenant deprovisioning, or leader-lock handoffs.
	DepartedGroupBuckets int

	// DepartedSeries counts the distinct first-stage series that crossed
	// the carry-forward bound and were treated as departed at least
	// once. A series can later resurrect via a new fresh point; this
	// counter does NOT decrement on resurrection — it tracks "how many
	// distinct groups went silent long enough to be dropped" so the
	// log line can name a concrete number of suspect entities.
	DepartedSeries int

	// TotalBuckets is the total number of folded buckets that the
	// CarryForward / DepartedGroup counters are measured against. Zero
	// if the fold produced no buckets.
	TotalBuckets int

	// GroupCount is the number of per-group series the first stage
	// returned. Useful context for SingleGroup and the departed counters.
	GroupCount int

	// TruncatedSeries is set when the upstream query hit MaxTimeSeries and the
	// result is therefore incomplete. Any aggregate built on top of it is only
	// a partial view of the metric and callers must surface that loudly.
	TruncatedSeries bool
}

// HasAny returns true if any actionable warning field is set.
// (TotalBuckets and GroupCount are context, not warnings.)
func (w AggregationWarnings) HasAny() bool {
	return w.SingleGroup || w.CarryForwardBuckets > 0 || w.DepartedGroupBuckets > 0 || w.DepartedSeries > 0 || w.TruncatedSeries
}

// RaggedBuckets returns legacy combined counter (carry-forward + departed-group).
func (w AggregationWarnings) RaggedBuckets() int {
	return w.CarryForwardBuckets + w.DepartedGroupBuckets
}

// buildAggregatedParams translates AggregationSpec to QueryTimeSeriesParams.
// Single-stage: clears GroupByFields, sets Reducer from AcrossGroups.
// Caller must validate spec first.
//
// Two-stage: GroupByFields is set from spec.GroupBy (already a fresh
// slice because MetricMeta.ResolveAggregation clones it, so no extra
// defensive copy) and Reducer is set from spec.WithinGroup. The
// AcrossGroups reducer is applied later in Go via foldGroupSeries.
func buildAggregatedParams(params QueryTimeSeriesParams, spec metrics.AggregationSpec) QueryTimeSeriesParams {
	p := params
	if spec.IsTwoStage() {
		p.GroupByFields = spec.GroupBy
		p.Reducer = ReducerToGCP(spec.WithinGroup)
		return p
	}
	p.GroupByFields = nil
	p.Reducer = ReducerToGCP(spec.AcrossGroups)
	return p
}

// QueryTimeSeriesAggregated runs a time-series query with AggregationSpec.
// Single-stage: applies AcrossGroups directly. Two-stage: groups then folds
// across groups in Go. Returns single synthetic series and non-fatal warnings.
func QueryTimeSeriesAggregated(ctx context.Context, client *monitoring.MetricClient, params QueryTimeSeriesParams, spec metrics.AggregationSpec) ([]MetricTimeSeries, AggregationWarnings, error) {
	var warnings AggregationWarnings
	if err := spec.Validate(); err != nil {
		return nil, warnings, fmt.Errorf("%w: %w", metrics.ErrInvalidAggregationSpec, err)
	}

	p := buildAggregatedParams(params, spec)

	if !spec.IsTwoStage() {
		// Single-stage: let Cloud Monitoring do the work.
		series, err := QueryTimeSeries(ctx, client, p)
		series, warnings.TruncatedSeries = stripTruncationSentinel(series)
		return series, warnings, err
	}

	// Two-stage: query with first-stage reducer, then fold in Go.
	groupSeries, err := QueryTimeSeries(ctx, client, p)
	if err != nil {
		return nil, warnings, err
	}
	groupSeries, warnings.TruncatedSeries = stripTruncationSentinel(groupSeries)
	warnings.GroupCount = len(groupSeries)

	// Return a single synthetic series carrying the folded points. When
	// the upstream query returned nothing, we still return one series
	// with nil points so downstream len(series)==0 handling behaves
	// identically to the single-stage path (which also returns an empty
	// slice on no data — same net effect via mergePoints).
	if len(groupSeries) == 0 {
		return []MetricTimeSeries{{
			MetricKind: params.MetricKind,
			ValueType:  params.ValueType,
		}}, warnings, nil
	}
	if len(groupSeries) == 1 {
		// Two-stage was requested but GCP returned a single series. The
		// legitimate reason is that the window genuinely contains one
		// entity (one game, one tenant). The problematic reason is a
		// registry typo — the group_by label is absent from the metric
		// descriptor, or the qualifier is wrong (bare "game_id" instead
		// of "metric.labels.game_id"). The fold below is still
		// mathematically safe (applyReducer on a single-element slice
		// returns that element for every reducer), so we do not fail —
		// but we surface the condition via warnings so the caller can
		// decide how loudly to flag it.
		warnings.SingleGroup = true
	}

	folded, stats := foldGroupSeries(groupSeries, spec.AcrossGroups)
	warnings.CarryForwardBuckets = stats.CarryForwardBuckets
	warnings.DepartedGroupBuckets = stats.DepartedGroupBuckets
	warnings.DepartedSeries = stats.DepartedSeries
	warnings.TotalBuckets = len(folded)
	return []MetricTimeSeries{{
		MetricKind: groupSeries[0].MetricKind,
		ValueType:  groupSeries[0].ValueType,
		Points:     folded,
	}}, warnings, nil
}

func stripTruncationSentinel(series []MetricTimeSeries) ([]MetricTimeSeries, bool) {
	if len(series) == 0 {
		return series, false
	}
	if !series[len(series)-1].Truncated {
		return series, false
	}
	return series[:len(series)-1], true
}

// StripTruncationSentinel exposes the sentinel-removal helper to tool-layer
// callers that use raw QueryTimeSeries rather than QueryTimeSeriesAggregated.
func StripTruncationSentinel(series []MetricTimeSeries) ([]MetricTimeSeries, bool) {
	return stripTruncationSentinel(series)
}

// maxCarryForwardBuckets bounds how many consecutive buckets a per-group
// series may be carried forward after its last fresh point before it is
// treated as genuinely gone. Three buckets ≈ 180s at the 60s alignment
// period used by every current tool caller (snapshot/compare/related
// hardcode StepSeconds=60). Wide enough to bridge a rolling-deploy
// replica handoff, narrow enough that a truly departed group (leader-
// lock lost, tenant deprovisioned, instance terminated) stops
// contributing its last value within one metrics snapshot window.
// Without this bound, a single fresh point at the start of the window
// would inflate every bucket until the end of time and silently
// misrepresent steady-state presence for departed groups. If a future
// caller uses a smaller alignment period the absolute bound shrinks
// proportionally — re-evaluate this constant if step ever drops below
// 30s. See the foldGroupSeries docblock for rationale.
const maxCarryForwardBuckets = 3

// foldGroupSeries combines multiple per-group series into a single series
// by applying the cross-group reducer at each unique timestamp. The input
// series are produced by a two-stage Cloud Monitoring query where the
// first stage already collapsed within-group dimensions; this is the
// second stage of the aggregation pipeline.
//
// Output contract: the returned []metrics.Point is sorted ascending by
// timestamp. This is load-bearing — downstream trend/spike detection in
// metrics.Process walks the series in order and would fabricate deltas
// from an unsorted input. TestFoldSortsOutput pins this invariant with
// enough timestamps that Go's randomized map iteration reliably scrambles
// the natural order.
//
// Sparse-bucket handling: the shared alignment_period keeps bucket
// endpoints in lockstep in the common case — Cloud Monitoring enforces
// this as part of REDUCE_*_GROUP_BY contracts — so carry-forward is a
// defensive fallback, not the hot path. A low-volume series can still
// drop an individual bucket (gap in publishing, deploy cutover), and a
// naive fold that only uses values present at each exact timestamp would
// silently under-count Sum/Mean whenever one group is momentarily
// missing — the silent-undercount class the aggregation refactor
// exists to eliminate. Instead we carry forward the last-seen value per series
// for up to maxCarryForwardBuckets consecutive buckets. A series that
// hasn't started yet (missing its first point) is excluded from buckets
// before its first point; after that point, it contributes its most
// recent value to every bucket until a newer one arrives — but only
// within the bounded window, so a genuinely departed group stops
// inflating the sum instead of fabricating steady-state presence forever.
//
// Ragged buckets — timestamps where at least one series contributed via
// carry-forward instead of a fresh point, or where at least one series
// had not yet produced its first point (and was therefore excluded from
// that bucket entirely) — are counted and returned so callers can log
// the coverage gap. Common causes: a series that starts mid-window, a
// gap in one group, or a deploy cutting publishing from one replica.
// foldStats reports per-fold sparse-coverage counters separately so the
// caller can populate AggregationWarnings without re-walking the buckets.
type foldStats struct {
	CarryForwardBuckets  int
	DepartedGroupBuckets int
	DepartedSeries       int
}

func foldGroupSeries(series []MetricTimeSeries, reducer metrics.Reducer) ([]metrics.Point, foldStats) {
	// Collect every distinct timestamp across all input series.
	tsSet := make(map[int64]struct{})
	for _, s := range series {
		for _, p := range s.Points {
			tsSet[p.Timestamp.UnixNano()] = struct{}{}
		}
	}
	if len(tsSet) == 0 {
		return nil, foldStats{}
	}

	tsSorted := make([]int64, 0, len(tsSet))
	for ts := range tsSet {
		tsSorted = append(tsSorted, ts)
	}
	sort.Slice(tsSorted, func(i, j int) bool { return tsSorted[i] < tsSorted[j] })

	// Build a per-series timestamp→value map for O(1) lookups at each
	// bucket. Cheaper than repeatedly binary-searching sorted points.
	seriesIndex := make([]map[int64]float64, len(series))
	for i, s := range series {
		m := make(map[int64]float64, len(s.Points))
		for _, p := range s.Points {
			m[p.Timestamp.UnixNano()] = p.Value
		}
		seriesIndex[i] = m
	}

	// carried tracks the last-seen value per series; started flags whether
	// a series has contributed its first point yet. Pre-started series are
	// skipped from a bucket entirely — carrying forward a zero before the
	// first real point would fabricate data.
	//
	// carryStreak counts consecutive carries since the last fresh point.
	// Once a series has been carried for more than maxCarryForwardBuckets
	// without a refresh, it is treated as departed and excluded from
	// subsequent buckets until a new fresh point resets the streak. This
	// stops "fresh point at t0, then silence" scenarios from inflating
	// every downstream bucket forever.
	//
	// hasDepartedOnce flips the first time a series crosses the bound and
	// stays set even if a later fresh point resurrects it — the
	// DepartedSeries counter is "distinct series that ever went silent
	// long enough to be dropped", which is the actionable signal an
	// operator wants ("how many of my entities suspiciously stopped
	// publishing"). Decrementing on resurrection would hide flapping.
	carried := make([]float64, len(series))
	started := make([]bool, len(series))
	carryStreak := make([]int, len(series))
	hasDepartedOnce := make([]bool, len(series))

	points := make([]metrics.Point, 0, len(tsSorted))
	var stats foldStats
	for _, ts := range tsSorted {
		values := make([]float64, 0, len(series))
		fresh := 0
		carriedCount := 0
		departedCount := 0
		for i := range series {
			if v, ok := seriesIndex[i][ts]; ok {
				carried[i] = v
				started[i] = true
				carryStreak[i] = 0
				values = append(values, v)
				fresh++
				continue
			}
			if !started[i] {
				continue
			}
			if carryStreak[i] >= maxCarryForwardBuckets {
				// Series has been silent long enough that we no longer
				// trust the carried value. Treat it as departed; a new
				// fresh point would resurrect it on a later bucket.
				if !hasDepartedOnce[i] {
					hasDepartedOnce[i] = true
					stats.DepartedSeries++
				}
				departedCount++
				continue
			}
			carryStreak[i]++
			values = append(values, carried[i])
			carriedCount++
		}
		if len(values) == 0 {
			continue
		}
		// Count the bucket against whichever signal is active. Departed
		// buckets are the more serious symptom (the group permanently
		// stopped contributing), so a bucket with both carries and
		// departures is counted as departed — operators triage that
		// first.
		if departedCount > 0 {
			stats.DepartedGroupBuckets++
		} else if carriedCount > 0 {
			stats.CarryForwardBuckets++
		}
		points = append(points, metrics.Point{
			Timestamp: time.Unix(0, ts),
			Value:     applyReducer(values, reducer),
		})
	}
	return points, stats
}

// applyReducer folds a slice of values into a single scalar using the
// named reducer. The input is always non-empty (foldGroupSeries skips
// empty buckets).
//
// Reducer semantics:
//
//   - Unknown reducers panic. Validation at the public entrypoint
//     (QueryTimeSeriesAggregated → spec.Validate) guarantees this branch
//     is unreachable. Silently returning a fabricated mean would make a
//     validator-bypass bug invisible in production data. Tool-layer
//     goroutines that drive this code path already wrap in defer/recover,
//     so the panic cannot crash the MCP server process.
//
//   - NaN propagates: NaN is a legitimate Cloud Monitoring value
//     (DISTRIBUTION with zero samples, divide-by-zero ratios) and
//     silencing it by skipping would falsify the numeric answer.
//     Sum/Mean poison the bucket via standard IEEE 754 arithmetic
//     (NaN + x = NaN). Max/Min are hand-rolled with an explicit
//     `v != v` check because IEEE 754 ordered comparisons return false
//     for NaN — a bare `v > mx` would silently drop NaNs and quietly
//     return whichever finite value happened to lead the slice. The
//     hand-rolled loops force NaN to win the comparison so one NaN
//     anywhere in the bucket surfaces as NaN. Operators should see NaN
//     in the tool output and fix the upstream metric, not have it
//     silently replaced with a plausible-looking number.
//
// Local variable names avoid shadowing Go 1.21+ builtins min/max so
// linters stay quiet and a future simplify-pass doesn't swap the loop
// for a slices.Max — slices.Max uses cmp.Less which orders NaN as the
// smallest value, the opposite of what this code needs.
func applyReducer(values []float64, reducer metrics.Reducer) float64 {
	switch reducer {
	case metrics.ReducerSum:
		var sum float64
		for _, v := range values {
			sum += v
		}
		return sum
	case metrics.ReducerMax:
		mx := values[0]
		for _, v := range values[1:] {
			if v > mx || (math.IsNaN(v) && !math.IsNaN(mx)) { // NaN wins
				mx = v
			}
		}
		return mx
	case metrics.ReducerMin:
		mn := values[0]
		for _, v := range values[1:] {
			if v < mn || (math.IsNaN(v) && !math.IsNaN(mn)) { // NaN wins
				mn = v
			}
		}
		return mn
	case metrics.ReducerMean:
		var sum float64
		for _, v := range values {
			sum += v
		}
		return sum / float64(len(values))
	default:
		panic(fmt.Sprintf("metrics.applyReducer: unknown reducer %q (spec validation bypassed)", reducer))
	}
}

// ReducerToGCP translates a high-level metrics.Reducer to the Cloud
// Monitoring enum value. Unknown reducers panic because spec.Validate
// at the public entrypoint already guarantees a valid reducer; mapping
// an unknown value to REDUCE_NONE would silently skip cross-series
// reduction and hand back the wrong scalar.
//
// Exported so tool handlers (metrics_top, etc.) building raw
// QueryTimeSeriesParams can share the single source of truth for the
// Reducer→monitoringpb mapping. Hand-rolled switches at call sites
// drift the moment a new reducer is added.
func ReducerToGCP(r metrics.Reducer) monitoringpb.Aggregation_Reducer {
	switch r {
	case metrics.ReducerSum:
		return monitoringpb.Aggregation_REDUCE_SUM
	case metrics.ReducerMax:
		return monitoringpb.Aggregation_REDUCE_MAX
	case metrics.ReducerMin:
		return monitoringpb.Aggregation_REDUCE_MIN
	case metrics.ReducerMean:
		return monitoringpb.Aggregation_REDUCE_MEAN
	}
	panic(fmt.Sprintf("metrics.ReducerToGCP: unknown reducer %q (spec validation bypassed)", r))
}

func buildAggregation(metricKind, valueType string, stepSeconds int64, groupByFields []string, reducer monitoringpb.Aggregation_Reducer) *monitoringpb.Aggregation {
	agg := &monitoringpb.Aggregation{
		AlignmentPeriod:  &durationpb.Duration{Seconds: stepSeconds},
		PerSeriesAligner: selectAligner(metricKind, valueType),
	}

	if len(groupByFields) > 0 {
		agg.GroupByFields = groupByFields
	}
	// Apply the reducer whenever the caller asked for one, regardless of
	// whether GroupByFields is set. Cloud Monitoring accepts empty
	// GroupByFields with a CrossSeriesReducer and collapses every series
	// to a single one — which is exactly what snapshot/compare/related
	// want when they need a "total across all labels" scalar. Previously
	// the reducer was silently ignored when GroupByFields was empty, so
	// REDUCE_MEAN set by those tools had no effect and all raw series
	// were flattened in Go instead. REDUCE_NONE (the zero value) still
	// means "don't aggregate" and lets callers opt out.
	if reducer != monitoringpb.Aggregation_REDUCE_NONE {
		agg.CrossSeriesReducer = reducer
	}

	return agg
}

// selectAligner picks the correct Cloud Monitoring per-series aligner for a
// (metricKind, valueType) pair. Rules come from the Cloud Monitoring API
// documentation for valid aligner combinations:
//
//   - GAUGE + DISTRIBUTION: ALIGN_MEAN — collapses the distribution to its
//     arithmetic mean per alignment period (produces a DOUBLE).
//   - DELTA / CUMULATIVE + DISTRIBUTION: ALIGN_DELTA — the API rejects both
//     ALIGN_RATE and ALIGN_MEAN for this combination (latency histograms
//     like pubsub ack_latencies, HTTP/RPC latency distributions, etc).
//     ALIGN_DELTA preserves the DistributionValue, and extractValue below
//     reads .Mean from it, so downstream consumers still get a per-window
//     mean latency — the same semantics ALIGN_MEAN gives on a GAUGE
//     distribution.
//   - DELTA / CUMULATIVE with numeric values: ALIGN_RATE converts the
//     counter to a per-second rate, which is what callers expect for
//     throughput/error metrics.
//   - GAUGE with numeric values (default): ALIGN_MEAN, the standard
//     time-weighted mean over the alignment period.
//
// When valueType is unknown (empty), we fall back to the kind-only logic so
// older callers keep working. This fallback is unsafe for distribution
// metrics (the API rejects ALIGN_RATE on DELTA+DISTRIBUTION), so callers
// SHOULD always supply a valueType discovered via GetMetricDescriptor.
func selectAligner(metricKind, valueType string) monitoringpb.Aggregation_Aligner {
	if valueType == "DISTRIBUTION" {
		switch metricKind {
		case "DELTA", "CUMULATIVE":
			return monitoringpb.Aggregation_ALIGN_DELTA
		default:
			return monitoringpb.Aggregation_ALIGN_MEAN
		}
	}
	switch metricKind {
	case "DELTA", "CUMULATIVE":
		return monitoringpb.Aggregation_ALIGN_RATE
	default:
		return monitoringpb.Aggregation_ALIGN_MEAN
	}
}

// flattenStructValue converts a structpb.Value from a monitored-resource
// metadata label into a string. It returns (value, true) for types that have
// a meaningful textual representation (string, number, bool) and ("", false)
// for null, nested struct/list, unknown kinds, and nil. Empty strings are
// legitimate and returned as ("", true).
func flattenStructValue(v *structpb.Value) (string, bool) {
	if v == nil {
		return "", false
	}
	switch k := v.Kind.(type) {
	case *structpb.Value_StringValue:
		return k.StringValue, true
	case *structpb.Value_NumberValue:
		return strconv.FormatFloat(k.NumberValue, 'f', -1, 64), true
	case *structpb.Value_BoolValue:
		return strconv.FormatBool(k.BoolValue), true
	case *structpb.Value_NullValue, *structpb.Value_StructValue, *structpb.Value_ListValue:
		return "", false
	default:
		return "", false
	}
}

func extractValue(tv *monitoringpb.TypedValue) (float64, bool) {
	switch v := tv.Value.(type) {
	case *monitoringpb.TypedValue_Int64Value:
		return float64(v.Int64Value), true
	case *monitoringpb.TypedValue_DoubleValue:
		return v.DoubleValue, true
	case *monitoringpb.TypedValue_DistributionValue:
		dv := v.DistributionValue
		// Count == 0 means no samples were recorded in this bucket — the
		// Mean field is zero by default and cannot be distinguished from a
		// genuine zero-mean measurement. Treat as unsupported so the point
		// is excluded rather than pulling down aggregate statistics.
		if dv == nil || dv.Count == 0 {
			return 0, false
		}
		return dv.Mean, true
	default:
		return 0, false
	}
}
