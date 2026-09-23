package gcpdata

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
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

// MetricKind is a Cloud Monitoring metric kind: how a metric's points relate
// in time. Not to be confused with metrics.MetricKind, the semantic kind
// (latency, throughput, ...) assigned by the registry.
type MetricKind string

const (
	MetricKindGauge      MetricKind = "GAUGE"
	MetricKindDelta      MetricKind = "DELTA"
	MetricKindCumulative MetricKind = "CUMULATIVE"
)

// EmptyWindowReason explains why a metric of the given kind that exists in
// Cloud Monitoring has no points in a query window.
func EmptyWindowReason(kind MetricKind) string {
	switch kind {
	case MetricKindDelta, MetricKindCumulative:
		return "no events occurred in the window (the counter was inactive)"
	case MetricKindGauge:
		return "no matching resources reported values in the window (check that they exist and the metric is being collected)"
	default:
		return "no data in window"
	}
}

type MetricDescriptorInfo struct {
	Type        string            `json:"type"`
	DisplayName string            `json:"display_name"`
	Description string            `json:"description,omitempty"`
	MetricKind  MetricKind        `json:"metric_kind"`
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
	MetricKind           MetricKind        `json:"metric_kind"`
	ValueType            string            `json:"value_type"`
	Points               []metrics.Point   `json:"points"`
}

// MetricDescriptorBasic contains fields needed for aligner selection and response enrichment.
// Everything from one ListMetricDescriptors call; no second RPC needed.
type MetricDescriptorBasic struct {
	Kind      MetricKind
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
func (q *MonitoringQuerier) GetMetricDescriptor(ctx context.Context, project, metricType string) (MetricDescriptorBasic, error) {
	filter := fmt.Sprintf(`metric.type = "%s"`, EscapeFilterValue(metricType))
	descriptors, err := q.ListMetricDescriptors(ctx, project, filter, 1)
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
		Labels:                 d.Labels,
		MonitoredResourceTypes: d.MonitoredResourceTypes,
	}, nil
}

func (q *MonitoringQuerier) ListMetricDescriptors(ctx context.Context, project, filter string, limit int) ([]MetricDescriptorInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, metricsQueryTimeout)
	defer cancel()

	req := &monitoringpb.ListMetricDescriptorsRequest{
		Name:   fmt.Sprintf("projects/%s", project),
		Filter: filter,
	}
	if limit > 0 {
		// Fetch no more than the caller keeps; the API clamps oversized pages.
		req.PageSize = safeInt32(limit)
	}

	var result []MetricDescriptorInfo
	it := q.client.ListMetricDescriptors(ctx, req)
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
			MetricKind:             MetricKind(desc.MetricKind.String()),
			ValueType:              desc.ValueType.String(),
			Unit:                   desc.Unit,
			MonitoredResourceTypes: desc.MonitoredResourceTypes,
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

// listMonitoredResourceDescriptors returns all monitored resource descriptors
// visible to the project, each with its defined label keys. Results are
// globally stable (the resource schema is part of the Cloud Monitoring API,
// not per-project state), so callers should cache. The caller bounds ctx.
func listMonitoredResourceDescriptors(ctx context.Context, client *monitoring.MetricClient, project string) ([]MonitoredResourceDescriptor, error) {
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
	// StepSeconds is the alignment period; callers must set it (tools
	// default to metrics.DefaultStepSeconds).
	StepSeconds int64
	MetricKind  MetricKind
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

// QueryTimeSeries fetches time series data from Cloud Monitoring. It returns
// at most MaxTimeSeries series; the warnings report whether the result was
// cut off there and how many points were dropped during decoding.
func (q *MonitoringQuerier) QueryTimeSeries(ctx context.Context, params QueryTimeSeriesParams) ([]MetricTimeSeries, QueryWarnings, error) {
	ctx, cancel := context.WithTimeout(ctx, metricsQueryTimeout)
	defer cancel()

	filter := fmt.Sprintf(`metric.type = "%s"`, EscapeFilterValue(params.MetricType))
	if params.LabelFilter != "" {
		filter += " AND " + params.LabelFilter
	}

	agg := buildAggregation(params.MetricKind, params.ValueType, params.StepSeconds, params.GroupByFields, params.Reducer)

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
	var warnings QueryWarnings
	it := q.client.ListTimeSeries(ctx, req)
	for {
		ts, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, warnings, fmt.Errorf("listing time series: %w", err)
		}
		if len(result) == MaxTimeSeries {
			warnings.TruncatedSeries = true
			break
		}

		mts := MetricTimeSeries{
			MetricKind: MetricKind(ts.MetricKind.String()),
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
				warnings.UnsupportedPoints++
				continue
			}
			if math.IsNaN(val) || math.IsInf(val, 0) {
				warnings.NonFinitePoints++
				continue
			}
			mts.Points = append(mts.Points, metrics.Point{
				Timestamp: p.Interval.EndTime.AsTime(),
				Value:     val,
			})
		}

		result = append(result, mts)
	}
	return result, warnings, nil
}

// QueryWarnings describes non-fatal issues encountered while running a
// time-series query: lossy decoding, truncation, and (for two-stage
// aggregation) sparse group coverage. Tool handlers forward these to the
// client so operators can see registry typos and partial data without
// trawling stderr. Zero value = no warnings.
type QueryWarnings struct {
	// UnsupportedPoints is the number of upstream points whose value type
	// this tool does not decode (e.g. BOOL, STRING, an empty distribution).
	UnsupportedPoints int
	// NonFinitePoints is the number of NaN/Inf points discarded, upstream or
	// produced by the cross-group fold.
	NonFinitePoints int
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

	// TotalBuckets is the number of buckets the fold returned (non-finite
	// results excluded); CarryForwardBuckets and DepartedGroupBuckets
	// count a subset of them. Zero if the fold produced no buckets.
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
func (w QueryWarnings) HasAny() bool {
	return w.UnsupportedPoints > 0 || w.NonFinitePoints > 0 || w.SingleGroup || w.CarryForwardBuckets > 0 || w.DepartedGroupBuckets > 0 || w.DepartedSeries > 0 || w.TruncatedSeries
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
func (q *MonitoringQuerier) QueryTimeSeriesAggregated(ctx context.Context, params QueryTimeSeriesParams, spec metrics.AggregationSpec) ([]MetricTimeSeries, QueryWarnings, error) {
	if err := spec.Validate(); err != nil {
		return nil, QueryWarnings{}, fmt.Errorf("%w: %w", metrics.ErrInvalidAggregationSpec, err)
	}

	p := buildAggregatedParams(params, spec)

	if !spec.IsTwoStage() {
		// Single-stage: let Cloud Monitoring do the work.
		return q.QueryTimeSeries(ctx, p)
	}

	// Two-stage: query with first-stage reducer, then fold in Go.
	groupSeries, warnings, err := q.QueryTimeSeries(ctx, p)
	if err != nil {
		return nil, warnings, err
	}
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

	folded := foldGroupSeries(groupSeries, spec.AcrossGroups, &warnings)
	return []MetricTimeSeries{{
		MetricKind: groupSeries[0].MetricKind,
		ValueType:  groupSeries[0].ValueType,
		Points:     folded,
	}}, warnings, nil
}

// maxCarryForwardBuckets bounds how many consecutive buckets a per-group
// series may be carried forward after its last fresh point before it is
// treated as genuinely gone. The bound is in buckets, so its duration scales
// with the alignment period: 180s at metrics.DefaultStepSeconds (60s), and
// 30s at the 10s minimum step metrics_snapshot accepts. Wide enough to bridge
// a rolling-deploy replica handoff, narrow enough that a truly departed group
// (leader lock lost, tenant deprovisioned, instance terminated) stops
// contributing its last value within one metrics snapshot window. Without
// this bound, a single fresh point at the start of the window would inflate
// every later bucket and misrepresent steady-state presence for departed
// groups. See the foldGroupSeries docblock for rationale.
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
// from an unsorted input. testFoldSortsOutput (a TestFoldGroupSeries
// subtest) pins this invariant with
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
// Buckets where at least one series contributed via carry-forward, or where a
// departed series was excluded, are counted into w.CarryForwardBuckets and
// w.DepartedGroupBuckets (with w.DepartedSeries naming the distinct departed
// series) so callers can log the coverage gap. Buckets whose reduced value is
// non-finite are dropped and counted only in w.NonFinitePoints, so both
// bucket counters stay within w.TotalBuckets, the number of returned points. Common causes: a series that starts mid-window,
// a gap in one group, or a deploy cutting publishing from one replica.
func foldGroupSeries(series []MetricTimeSeries, reducer metrics.Reducer, w *QueryWarnings) []metrics.Point {
	// Collect every distinct timestamp across all input series.
	tsSet := make(map[int64]struct{})
	for _, s := range series {
		for _, p := range s.Points {
			tsSet[p.Timestamp.UnixNano()] = struct{}{}
		}
	}
	if len(tsSet) == 0 {
		return nil
	}
	tsSorted := slices.Sorted(maps.Keys(tsSet))

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
					w.DepartedSeries++
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
		value := applyReducer(values, reducer)
		if math.IsNaN(value) || math.IsInf(value, 0) {
			w.NonFinitePoints++
			continue
		}
		// Count the bucket against whichever signal is active. Departed
		// buckets are the more serious symptom (the group permanently
		// stopped contributing), so a bucket with both carries and
		// departures is counted as departed — operators triage that
		// first.
		if departedCount > 0 {
			w.DepartedGroupBuckets++
		} else if carriedCount > 0 {
			w.CarryForwardBuckets++
		}
		points = append(points, metrics.Point{
			Timestamp: time.Unix(0, ts),
			Value:     value,
		})
	}
	w.TotalBuckets = len(points)
	return points
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
// Values are finite by construction: QueryTimeSeries rejects NaN and Inf at
// ingestion. A finite sum can still overflow; foldGroupSeries drops that
// result and accounts for it in QueryWarnings.
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
			if v > mx {
				mx = v
			}
		}
		return mx
	case metrics.ReducerMin:
		mn := values[0]
		for _, v := range values[1:] {
			if v < mn {
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

func buildAggregation(metricKind MetricKind, valueType string, stepSeconds int64, groupByFields []string, reducer monitoringpb.Aggregation_Reducer) *monitoringpb.Aggregation {
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
func selectAligner(metricKind MetricKind, valueType string) monitoringpb.Aggregation_Aligner {
	if valueType == "DISTRIBUTION" {
		switch metricKind {
		case MetricKindDelta, MetricKindCumulative:
			return monitoringpb.Aggregation_ALIGN_DELTA
		default:
			return monitoringpb.Aggregation_ALIGN_MEAN
		}
	}
	switch metricKind {
	case MetricKindDelta, MetricKindCumulative:
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
