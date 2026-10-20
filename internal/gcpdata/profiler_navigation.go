package gcpdata

import (
	"context"
	"fmt"

	"github.com/google/pprof/profile"
)

// TopProfileParams selects a source profile or a request-local current-minus-base
// diff, then ranks its functions. Non-positive limits default to 20; at most 50
// functions are returned. OnProgress reports the transition from fetch to analysis.
type TopProfileParams struct {
	Project       string
	ProfileID     string
	BaseProfileID string
	ValueIndex    int
	Limit         int
	SortBy        string
	Filter        string
	OnProgress    func(current, total int, msg string)
}

// PeekProfileParams selects a profile and function for caller/callee navigation.
// Non-positive limits default to 10; at most 30 callers and callees are returned.
type PeekProfileParams struct {
	Project       string
	ProfileID     string
	BaseProfileID string
	FunctionName  string
	ValueIndex    int
	Limit         int
	OnProgress    func(current, total int, msg string)
}

// FlamegraphProfileParams selects a profile and optional subtree. MaxDepth
// defaults to 3 and is capped at 6; non-positive MinPct defaults to 1 percent.
// The returned tree always contains at most 1000 nodes, including its root.
type FlamegraphProfileParams struct {
	Project       string
	ProfileID     string
	BaseProfileID string
	RootFunction  string
	ValueIndex    int
	MaxDepth      int
	MinPct        float64
	OnProgress    func(current, total int, msg string)
}

// ProfileFetchError distinguishes source loading and diff-construction failures
// from analysis errors. Unwrap retains the underlying GCP status for guidance.
// Its message deliberately preserves the original fetch context.
type ProfileFetchError struct{ Err error }

func (e *ProfileFetchError) Error() string { return e.Err.Error() }
func (e *ProfileFetchError) Unwrap() error { return e.Err }

// TopProfile fetches, analyzes and assembles the bounded ranking result.
func (q *CloudProfilerQuerier) TopProfile(ctx context.Context, params TopProfileParams) (*ProfileTopResult, error) {
	p, meta, err := q.profileOrDiff(ctx, params.Project, params.ProfileID, params.BaseProfileID)
	if err != nil {
		return nil, &ProfileFetchError{Err: err}
	}
	navigationProgress(params.OnProgress, "Analyzing profile...")
	limit := navigationLimit(params.Limit, 20, 50)
	sortBy := params.SortBy
	if sortBy == "" {
		sortBy = "cumulative"
	}
	functions, total, truncated, err := topFunctions(p, params.ValueIndex, limit, sortBy, params.Filter)
	if err != nil {
		return nil, err
	}
	values := profileValueTypes(p)
	result := &ProfileTopResult{
		ProfileMeta: meta, ValueType: values[params.ValueIndex], AvailableValues: values,
		TotalValue: total, TopFunctions: functions, Truncated: truncated,
	}
	if truncated {
		result.TruncationHint = fmt.Sprintf("Showing top %d functions. Use filter to narrow results or increase limit (max 50).", limit)
	}
	return result, nil
}

// PeekProfile fetches and assembles caller/callee context for one function.
func (q *CloudProfilerQuerier) PeekProfile(ctx context.Context, params PeekProfileParams) (*ProfilePeekResult, error) {
	p, meta, err := q.profileOrDiff(ctx, params.Project, params.ProfileID, params.BaseProfileID)
	if err != nil {
		return nil, &ProfileFetchError{Err: err}
	}
	navigationProgress(params.OnProgress, "Analyzing function...")
	limit := navigationLimit(params.Limit, 10, 30)
	function, callers, callees, err := peekFunction(p, params.FunctionName, params.ValueIndex, limit)
	if err != nil {
		return nil, err
	}
	result := &ProfilePeekResult{
		ProfileMeta: meta, ValueType: profileValueTypes(p)[params.ValueIndex], Function: *function,
		Callers: callers, Callees: callees,
		CallersTruncated: len(callers) >= limit, CalleesTruncated: len(callees) >= limit,
	}
	if result.CallersTruncated || result.CalleesTruncated {
		result.TruncationHint = fmt.Sprintf("Showing up to %d callers/callees. Increase limit (max 30) for more.", limit)
	}
	return result, nil
}

// FlamegraphProfile fetches and assembles a bounded call tree.
func (q *CloudProfilerQuerier) FlamegraphProfile(ctx context.Context, params FlamegraphProfileParams) (*ProfileFlamegraphResult, error) {
	p, meta, err := q.profileOrDiff(ctx, params.Project, params.ProfileID, params.BaseProfileID)
	if err != nil {
		return nil, &ProfileFetchError{Err: err}
	}
	navigationProgress(params.OnProgress, "Building flamegraph...")
	depth := navigationLimit(params.MaxDepth, 3, 6)
	minPct := params.MinPct
	if minPct <= 0 {
		minPct = 1
	}
	root, total, pruned, err := flamegraph(p, params.RootFunction, params.ValueIndex, depth, 1000, minPct)
	if err != nil {
		return nil, err
	}
	result := &ProfileFlamegraphResult{
		ProfileMeta: meta, ValueType: profileValueTypes(p)[params.ValueIndex], TotalValue: total,
		Root: *root, MaxDepth: depth, MinPct: minPct, PrunedNodes: pruned,
	}
	if total == 0 {
		result.Warning = "Total profile value is zero (positive and negative values cancel out in diff profiles). All percentage values will be 0%."
	}
	return result, nil
}

func navigationLimit(limit, fallback, maximum int) int {
	if limit <= 0 {
		return fallback
	}
	return min(limit, maximum)
}

func navigationProgress(fn func(current, total int, msg string), msg string) {
	if fn != nil {
		fn(1, 2, msg)
	}
}

// profileOrDiff computes a request-local diff when baseID is present.
func (q *CloudProfilerQuerier) profileOrDiff(ctx context.Context, project, profileID, baseID string) (*profile.Profile, ProfileMeta, error) {
	if baseID == "" {
		return q.getOrFetchProfile(ctx, project, profileID)
	}
	current, base, meta, _, err := q.fetchProfilePair(ctx, project, profileID, baseID)
	if err != nil {
		return nil, ProfileMeta{}, err
	}
	diff, err := buildDiffProfile(current, base)
	if err != nil {
		return nil, ProfileMeta{}, fmt.Errorf("building diff profile: %w", err)
	}
	meta.IsDiff = true
	return diff, meta, nil
}
