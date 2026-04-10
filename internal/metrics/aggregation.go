package metrics

// DefaultAggregation returns the default AggregationSpec for a metric kind.
// Single-stage only (GroupBy empty); two-stage requires a specific entity label.
// Business KPIs/throughput/errors: sum. Latency: mean. Freshness/saturation: max.
// Ratios: mean. Unknown: mean.
func DefaultAggregation(kind MetricKind) AggregationSpec {
	switch kind {
	case KindBusinessKPI, KindThroughput, KindErrorRate:
		// Counters/rates: total is the sum across labels.
		// Gauges carrying KPI values (session counts, pending orders,
		// queue depth) also want sum by default — the few exceptions
		// (ratios, multipliers) must set across_groups: mean explicitly
		// in the registry.
		return AggregationSpec{AcrossGroups: ReducerSum}
	case KindLatency:
		// Histograms: ALIGN_DELTA+DISTRIBUTION already produces per-series
		// means; averaging across series gives the grand mean.
		return AggregationSpec{AcrossGroups: ReducerMean}
	case KindFreshness, KindSaturation:
		// Worst-case lag / peak backlog — take the max.
		return AggregationSpec{AcrossGroups: ReducerMax}
	case KindResourceUtilization, KindAvailability:
		// Ratios — summing is meaningless, take the mean.
		return AggregationSpec{AcrossGroups: ReducerMean}
	default:
		// KindUnknown and anything unrecognized fall through to mean
		// (see the header comment for the rationale).
		return AggregationSpec{AcrossGroups: ReducerMean}
	}
}

// ResolveAggregation returns the effective AggregationSpec: explicit if set,
// otherwise the per-kind default. GroupBy is cloned for safe mutation.
// Use the same spec for current and baseline windows to ensure valid deltas.
func (m MetricMeta) ResolveAggregation() AggregationSpec {
	var spec AggregationSpec
	if m.Aggregation != nil {
		spec = *m.Aggregation
	} else {
		spec = DefaultAggregation(m.Kind)
	}
	if len(spec.GroupBy) > 0 {
		clone := make([]string, len(spec.GroupBy))
		copy(clone, spec.GroupBy)
		spec.GroupBy = clone
	}
	return spec
}
