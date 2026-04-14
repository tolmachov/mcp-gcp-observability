# Code Review: Cloud Monitoring (Metrics) Feature

## Overview

Substantial feature adding Cloud Monitoring support to the MCP server. Introduces 5 new tools (`metrics_list`, `metrics_snapshot`, `metrics_top_contributors`, `metrics_related`, `metrics_compare`) with a semantic analysis layer — a signal processor that classifies metric behavior (stable, spike, regression, saturation, etc.) rather than dumping raw time series data.

## Architecture — Well Done

- **`internal/metrics/`** — Clean separation of concerns: types, classification, processing, registry. The `MetricsQuerier` interface in `gcpdata/querier.go` makes the tool handlers testable without a real GCP client.
- **Signal Processor** (`processor.go`) — Solid statistical analysis: baseline comparison, trend detection via linear regression, step change detection, spike z-score, SLO breach tracking, saturation detection. All deterministic, no ML black boxes.
- **Classification decision tree** (`classify.go`) — Well-ordered priority (saturation > spike > no-deviation > recovery > step > sustained > noisy > fallback). The `isDegrading` function correctly handles `DirectionNone` by refusing to call anything a "regression" when direction is unknown.
- **Semantic Registry** — Auto-detection from metric names is a pragmatic default; YAML config for domain knowledge is the right escalation path.
- **Integration tests** (`metrics_integration_test.go`, 954 lines) — Thorough handler-level tests for all 5 tools with `fakeQuerier` mock. Covers happy paths, validation errors, error propagation, project resolution, and auto-detection.

## Issues

### HIGH: Sequential API calls in metrics_related.go

**File:** `internal/tools/metrics_related.go:113-165`

The loop queries each related metric sequentially — 3 API calls per metric (GetMetricKind, current window, baseline window). These are independent and should be parallelized with `sync.WaitGroup` + `sync.Mutex`, following the same pattern as `queryBaselineSeries` same_weekday_hour mode in `metrics_snapshot.go:223-253`.

### HIGH: Sequential window queries in metrics_compare.go

**File:** `internal/tools/metrics_compare.go:144-158`

Window A and B queries are independent but run sequentially:
```go
seriesA, err := h.querier.QueryTimeSeries(ctx, paramsA)  // line 147
seriesB, err := h.querier.QueryTimeSeries(ctx, paramsB)  // line 155
```
Should be parallelized — halves the latency for this tool.

### HIGH: No timeout on metric queries

**File:** `internal/gcpdata/metrics.go` — all three exported functions

The log/error/trace queries have explicit timeouts (e.g., `loggingQueryTimeout = 30 * time.Second` in errors.go, `traceQueryTimeout = 30 * time.Second` in traces.go). The metrics queries rely on the context passed in, which has no timeout set. If a Cloud Monitoring query hangs, there's no safety net. Especially dangerous for `same_weekday_hour` mode (4 parallel queries) and `metrics_related` (sequential queries per related metric).

**Fix:** Add a `const metricsQueryTimeout = 30 * time.Second` and wrap context in `GetMetricKind`, `ListMetricDescriptors`, and `QueryTimeSeries`, consistent with existing patterns.

### HIGH: Error from GetMetricKind silently ignored in metrics_related.go

**File:** `internal/tools/metrics_related.go:116`

```go
gcpKind, _ := h.querier.GetMetricKind(ctx, project, relMetric)
```

The error is discarded. If the descriptor lookup fails, `gcpKind` is empty string, which falls through to `ALIGN_MEAN` in `buildAggregation`. For DELTA or CUMULATIVE metrics, this produces **silently incorrect data** — rates will be reported as raw counts. The numbers are wrong by orders of magnitude but look plausible.

**Fix:**
```go
gcpKind, err := h.querier.GetMetricKind(ctx, project, relMetric)
if err != nil {
    skipped = append(skipped, SkippedSignal{MetricType: relMetric, Reason: fmt.Sprintf("failed to get metric kind: %v", err)})
    continue
}
```

### MEDIUM: `GetMetricKind` returns `("", nil)` for missing descriptors

**File:** `internal/gcpdata/metrics.go:57-58`

When `ListMetricDescriptors` returns zero results (metric doesn't exist or is misspelled), `GetMetricKind` returns `("", nil)` — no error. Every caller then passes empty string to `buildAggregation`, which silently applies `ALIGN_MEAN`. A misspelled metric type gets a confusing "no data found" error instead of "metric not found" at descriptor lookup.

**Fix:** Return an error when no descriptors are found:
```go
if len(descriptors) == 0 {
    return "", fmt.Errorf("metric descriptor not found for %q in project %q", metricType, project)
}
```

### MEDIUM: `ClassificationThresholds` has no validation

**File:** `internal/metrics/types.go:91-96`

No validation anywhere. A user could put `SignificantDeltaPct: -5` or `SpikeZScore: 0` in YAML and the system silently misclassifies everything. `MetricMeta.Validate()` checks kind, direction, SLO threshold, and saturation cap — but never inspects `Thresholds`.

**Fix:** Add `Validate() error` to `ClassificationThresholds` (check all fields positive, `BreachRatioForRegress` in [0,1]) and call it from `MetricMeta.Validate()`.

### MEDIUM: `splitDimension` uses magic numbers

**File:** `internal/tools/metrics_top.go:264-275`

Uses hardcoded lengths `14` and `16` for `"metric.labels."` and `"resource.labels."` prefix detection:
```go
if len(dimension) > 14 && dimension[:14] == "metric.labels." {
    return dimensionParts{prefix: "metric", key: dimension[14:]}
}
```
Replace with `strings.CutPrefix` — clearer and safer.

### MEDIUM: Baseline mode strings are not constants

**Files:** `metrics_snapshot.go`, `metrics_top.go`, `metrics_related.go`

Strings `"prev_window"`, `"same_weekday_hour"`, `"pre_event"` are repeated across multiple files in enum definitions, default values, and switch cases. Should be extracted as package-level constants.

### MEDIUM: Missing `business_kpi` in `metrics_list` kind enum

**File:** `internal/tools/metrics_list.go:43-44`

The `Enum` for the `kind` parameter lists 6 kinds but omits `business_kpi`, which is a valid `MetricKind`. If a user has `business_kpi` metrics in their registry, they can't filter for them.

### MEDIUM: `metrics_list` kind filter under-fetches from API

**File:** `internal/tools/metrics_list.go:97-142`

`apiLimit` is calculated before kind filtering. The GCP API doesn't support filtering by semantic kind, so descriptors are fetched and then filtered locally. With `kind=latency` and `limit=50`, the API may return 45 descriptors where only 3 are latency — the result has far fewer than 50 entries even though more exist. Consider over-fetching when a kind filter is active.

### MEDIUM: `extractValue` silently drops unsupported value types

**File:** `internal/gcpdata/metrics.go:162-168`

Boolean and string metric points are silently skipped via `continue`. A metric with all boolean values returns zero points and a confusing "no data found" error. Should count dropped points and return an explicit error about unsupported value types when all points are dropped.

### LOW: `new(*cfg)` in client.go

**File:** `internal/gcpclient/client.go:83`

```go
config: new(*cfg),
```

Replaced the previous explicit copy pattern. While it compiles on Go 1.26, it's unusual syntax that will confuse readers. The previous `cfgCopy := *cfg; config: &cfgCopy` was more idiomatic. Verify that `new(*cfg)` produces a proper copy rather than a zero-valued allocation.

### LOW: `same_weekday_hour` baseline silently degrades on partial failure

**File:** `internal/tools/metrics_snapshot.go:248-253`

When 3 of 4 parallel week queries fail, the baseline is computed from 1/4 of expected data. The only check is whether ALL queries failed. Also, when all fail, only `errs[0]` is reported — the other 3 errors are discarded. Consider using `errors.Join(errs...)` and surfacing partial failures in `baseline_warnings`.

### LOW: `labelValueFromSeries` silent `"(unknown)"` fallback

**File:** `internal/tools/metrics_top.go:235-257`

When the dimension key isn't found in labels, returns `"(unknown)"`. If the user provides a wrong dimension key, all series get aggregated under `"(unknown)"` with 100% share — looks like a valid result. Should warn if all/most values are `"(unknown)"`.

### LOW: `QueryTimeSeries` truncation flag never surfaced

**File:** `internal/gcpdata/metrics.go:172-175`

When `MaxTimeSeries` (500) is exceeded, the last series gets `Truncated: true`. But no downstream consumer checks this field. The user gets results computed from incomplete data with no warning.

### LOW: `SignalFeatures.P50` and `Median` are always identical

**File:** `internal/metrics/types.go` + `processor.go:39`

Set on the same line: `f.P50 = f.Median`. Remove one to avoid future divergence.

### LOW: `SignalFeatures.BaselineMode` is a dead field

**File:** `internal/metrics/types.go:140`

Never populated by `Process()`. The snapshot handler sets baseline mode on its own result struct (`MetricSnapshotResult.BaselineMode`). Remove from `SignalFeatures`.

### LOW: `ValidMetricKindsForInput` comment inaccurate

**File:** `internal/metrics/types.go:34`

Comment says "excludes unknown" but also excludes `business_kpi`. Should say "excludes unknown and business_kpi".

### LOW: `mergePoints` deduplication

**File:** `internal/tools/metrics_snapshot.go:273-282`

When `same_weekday_hour` baseline queries return series from multiple weeks, `mergePoints` concatenates and sorts all points with no deduplication. Worth a comment at minimum.

### LOW: `computeSLOBreach` streak logic

**File:** `internal/metrics/processor.go:210-223`

The reverse iteration for current streak calculation is correct but non-obvious. A forward approach or explanatory comment would improve readability.

### LOW: Inline baseline logic in metrics_related.go duplicates queryBaselineSeries

**File:** `internal/tools/metrics_related.go:141-144`

Reimplements the `prev_window` case of `queryBaselineSeries`. Acceptable simplification since `metrics_related` always uses `prev_window`.

### LOW: Classification constants are untyped strings

**File:** `internal/metrics/classify.go:7-14`

The `Class*` constants are untyped strings. Making them `type Classification string` would enable exhaustive switch checking with linters and prevent accidental comparison with arbitrary strings.

### LOW: No nil-check in `NewMonitoringQuerier`

**File:** `internal/gcpdata/querier.go`

A nil client will cause a panic deep in the GCP SDK. Add a nil check in the constructor.

### INFO: GetMetricKind called separately per metric (N+1 pattern)

**Files:** `metrics_snapshot.go:144`, `metrics_compare.go:129`, `metrics_top.go:125`, `metrics_related.go:116`

Each handler calls `GetMetricKind()` which internally calls `ListMetricDescriptors` with limit=1. Could cache per request, but not worth the complexity for typical call patterns (1-5 metrics per invocation).

### INFO: Magic numbers in classify.go thresholds

**File:** `internal/metrics/classify.go`

Several hardcoded thresholds lack comments explaining their values:
- Line 28: `SpikeRatio < 0.15 && absDelta < 20` — spike detection guard
- Line 43: `BreachRatio < 0.2 && TrendScore < -0.02` — recovery detection
- Line 48: `CV < 0.35` — step regression threshold
- Line 57: `CV > 0.4` — post-significance noise catch-all
- `processor.go:82`: `len(bValues) >= 7` — baseline reliability minimum
- `processor.go:189`: `z >= 3.0` — spike counting threshold (differs from configurable `SpikeZScore`)
- `processor.go:256`: `0.95 * satCap` — saturation detection margin

Not bugs, but these should have brief comments explaining the rationale.

### INFO: `stddev` computes population standard deviation

**File:** `internal/metrics/processor.go:295+`

`stddev` divides by N (population), not N-1 (sample). This affects CV, z-scores, and all downstream classifications. Worth a one-line comment: `// Population standard deviation (divides by N, not N-1).`

## Minor / Style

- Import alias cleanup in `errors.go`, `helpers.go`, `logs.go`, `traces.go`, `traces_test.go` — removing unnecessary `pkg "path"` aliases. Good hygiene.
- README table reorganization is a clear improvement.
- Tool descriptions with cross-references ("use metrics_snapshot instead", "use metrics_list first") are excellent MCP design — helps LLMs navigate between tools.

## Test Coverage

Strong coverage overall. The new `metrics_integration_test.go` (954 lines) provides thorough handler-level tests for all 5 tools using a well-designed `fakeQuerier` mock.

**Covered:**

- `classify_test.go` — all 7 classification paths, direction handling
- `processor_test.go` — stable, step regression, saturation, spike, noisy, zero baseline, few points, direction up, empty points, percentiles, data quality gaps
- `registry_test.go` — auto-detection (9 real GCP metric types), YAML loading, validation errors, list filtering
- `metrics_integration_test.go` — all 5 handler happy paths, validation errors, error propagation, project resolution, auto-detection, pre_event baseline
- `metrics_compare_test.go` — severity ordering, unknown classification
- `metrics_top_test.go` — dimension splitting

**Remaining gaps (lower priority):**

- `same_weekday_hour` baseline mode at handler level (4 concurrent goroutines, partial-failure tolerance)
- `metrics_top_contributors` reducer selection (REDUCE_SUM for throughput vs REDUCE_MEAN)
- `metrics_compare` no-data in one window only
- `metrics_related` query error (vs. no-data) skip path
- `labelValueFromSeries` fallback to "(unknown)"
- `mergePoints` direct unit test
- `MetricMeta.Validate()` edge cases: negative SLO threshold, zero saturation cap
- `computeSLOBreach` partial streak patterns
- `EffectiveThresholds` with custom thresholds
- `unitFromDescriptor` — strips `{`/`}` and handles `"1"` sentinel

## Security

- `EscapeFilterValue` is used to sanitize the `match` parameter before interpolation into the filter string — good.
- No user-controlled values are directly interpolated into queries without escaping.

## Verdict

Well-designed feature that transforms the MCP server from a "logs/errors viewer" into a proper observability tool. The semantic layer (classification + baseline comparison + contributor drill-down) is the right abstraction for LLM consumption. The integration tests are thorough and the `MetricsQuerier` interface is now consistently used across all handlers.

**Must fix before merge:**
1. No timeout on metric queries (HIGH) — can hang the MCP server
2. GetMetricKind error silently discarded in metrics_related.go (HIGH) — produces incorrect data
3. Sequential API calls in metrics_related.go and metrics_compare.go (HIGH) — user-facing latency

**Should fix:**
4. `GetMetricKind` returns ("", nil) for missing descriptors (MEDIUM)
5. `ClassificationThresholds` validation (MEDIUM)
6. `splitDimension` magic numbers (MEDIUM)
7. Baseline mode string constants (MEDIUM)
8. `extractValue` silent drops (MEDIUM)

**Nice to have:** P50/Median redundancy, dead BaselineMode field, classification typed constants, comment improvements.