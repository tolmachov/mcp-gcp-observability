package gcpdata

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/pprof/profile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests cross the navigation boundary through compressed source profiles
// served by the in-process Export service, including the cache on repeated calls.
func TestProfileNavigationSourceAndDiff(t *testing.T) {
	for _, baseID := range []string{"", "base"} {
		t.Run("base="+baseID, func(t *testing.T) {
			q, srv := newStockProfilerQuerier(t,
				stockProfile("cur", 2, encodeProfile(t, buildTestProfile())),
				stockProfile("base", 1, encodeProfile(t, halvedTestProfile())),
			)
			var navigation ProfilerQuerier = q
			expectedTotal := int64(100)
			expectedScans := 1
			if baseID != "" {
				expectedTotal = 50
				expectedScans = 2
			}
			progress := 0
			top, err := navigation.TopProfile(context.Background(), TopProfileParams{
				Project: stockProject, ProfileID: "cur", BaseProfileID: baseID,
				Limit: 2, OnProgress: func(current, total int, msg string) {
					assert.Equal(t, 1, current)
					assert.Equal(t, 2, total)
					assert.Equal(t, "Analyzing profile...", msg)
					progress++
				},
			})
			require.NoError(t, err)
			assert.Equal(t, expectedTotal, top.TotalValue)
			assert.Equal(t, stockName("cur"), top.ProfileMeta.ProfileID)
			assert.Equal(t, baseID != "", top.ProfileMeta.IsDiff)
			assert.Equal(t, ValueTypeInfo{Index: 0, Type: "cpu", Unit: "nanoseconds"}, top.ValueType)
			assert.Equal(t, []ValueTypeInfo{top.ValueType}, top.AvailableValues)
			require.Len(t, top.TopFunctions, 2)
			assert.Equal(t, "main.main", top.TopFunctions[0].FunctionName)
			assert.Equal(t, expectedTotal, top.TopFunctions[0].CumulativeValue)
			assert.True(t, top.Truncated)
			assert.Equal(t, "Showing top 2 functions. Use filter to narrow results or increase limit (max 50).", top.TruncationHint)
			assert.Equal(t, 1, progress)

			peek, err := navigation.PeekProfile(context.Background(), PeekProfileParams{
				Project: stockProject, ProfileID: "cur", BaseProfileID: baseID,
				FunctionName: "handler.Handle", Limit: 1,
			})
			require.NoError(t, err)
			assert.Equal(t, top.ProfileMeta, peek.ProfileMeta)
			assert.Equal(t, top.ValueType, peek.ValueType)
			assert.Equal(t, PeekFunctionInfo{Name: "myapp/handler.Handle", File: "handler.go", Cumulative: expectedTotal * 8 / 10}, peek.Function)
			require.Len(t, peek.Callers, 1)
			require.Len(t, peek.Callees, 1)
			assert.Equal(t, PeekEntry{Name: "main.main", File: "main.go", Cost: expectedTotal * 8 / 10, Pct: 80}, peek.Callers[0])
			assert.Equal(t, "database/sql.(*DB).Query", peek.Callees[0].Name)
			assert.Equal(t, expectedTotal/2, peek.Callees[0].Cost)
			assert.True(t, peek.CallersTruncated)
			assert.True(t, peek.CalleesTruncated)
			assert.Equal(t, "Showing up to 1 callers/callees. Increase limit (max 30) for more.", peek.TruncationHint)

			flame, err := navigation.FlamegraphProfile(context.Background(), FlamegraphProfileParams{
				Project: stockProject, ProfileID: "cur", BaseProfileID: baseID,
				RootFunction: "handler.Handle", MaxDepth: 1, MinPct: 1,
			})
			require.NoError(t, err)
			assert.Equal(t, top.ProfileMeta, flame.ProfileMeta)
			assert.Equal(t, expectedTotal, flame.TotalValue)
			assert.Equal(t, "myapp/handler.Handle", flame.Root.Name)
			assert.Equal(t, expectedTotal*8/10, flame.Root.Cumulative)
			assert.Equal(t, 80.0, flame.Root.Pct)
			require.Len(t, flame.Root.Children, 2)
			assert.Equal(t, 1, flame.MaxDepth)
			assert.Equal(t, 1.0, flame.MinPct)
			assert.Empty(t, flame.Warning)

			source, err := navigation.TopProfile(context.Background(), TopProfileParams{Project: stockProject, ProfileID: "cur"})
			require.NoError(t, err)
			assert.Equal(t, int64(100), source.TotalValue, "diff must never replace cached source bytes")
			assert.False(t, source.ProfileMeta.IsDiff)
			assert.Equal(t, expectedScans, srv.listCalls())
		})
	}
}

func TestProfileNavigationErrors(t *testing.T) {
	incompatible := buildTestProfile()
	incompatible.SampleType[0].Unit = "bytes"
	q, _ := newStockProfilerQuerier(t,
		stockProfile("cur", 2, encodeProfile(t, buildTestProfile())),
		stockProfile("incompatible", 1, encodeProfile(t, incompatible)),
	)
	cases := []struct {
		name     string
		run      func() error
		contains string
		fetch    bool
	}{
		{"top bad index", func() error {
			_, err := q.TopProfile(context.Background(), TopProfileParams{Project: stockProject, ProfileID: "cur", ValueIndex: 3})
			return err
		}, "value_index 3 out of range", false},
		{"peek bad index", func() error {
			_, err := q.PeekProfile(context.Background(), PeekProfileParams{Project: stockProject, ProfileID: "cur", ValueIndex: 3})
			return err
		}, "value_index 3 out of range", false},
		{"flame bad index", func() error {
			_, err := q.FlamegraphProfile(context.Background(), FlamegraphProfileParams{Project: stockProject, ProfileID: "cur", ValueIndex: 3})
			return err
		}, "value_index 3 out of range", false},
		{"peek absent", func() error {
			_, err := q.PeekProfile(context.Background(), PeekProfileParams{Project: stockProject, ProfileID: "cur", FunctionName: "absent"})
			return err
		}, `no function matching "absent"`, false},
		{"peek ambiguous", func() error {
			_, err := q.PeekProfile(context.Background(), PeekProfileParams{Project: stockProject, ProfileID: "cur", FunctionName: "myapp/"})
			return err
		}, "ambiguous, matches: myapp/handler.Handle, myapp/logger.Log", false},
		{"flame absent", func() error {
			_, err := q.FlamegraphProfile(context.Background(), FlamegraphProfileParams{Project: stockProject, ProfileID: "cur", RootFunction: "absent"})
			return err
		}, `function "absent" not found`, false},
		{"flame ambiguous", func() error {
			_, err := q.FlamegraphProfile(context.Background(), FlamegraphProfileParams{Project: stockProject, ProfileID: "cur", RootFunction: "myapp/"})
			return err
		}, "matches 2 nodes; use a more specific name", false},
		{"missing source", func() error {
			_, err := q.TopProfile(context.Background(), TopProfileParams{Project: stockProject, ProfileID: "missing"})
			return err
		}, "not found", true},
		{"missing current", func() error {
			_, err := q.PeekProfile(context.Background(), PeekProfileParams{Project: stockProject, ProfileID: "missing", BaseProfileID: "cur"})
			return err
		}, "fetching current profile:", true},
		{"missing base", func() error {
			_, err := q.FlamegraphProfile(context.Background(), FlamegraphProfileParams{Project: stockProject, ProfileID: "cur", BaseProfileID: "missing"})
			return err
		}, "fetching base profile:", true},
		{"incompatible diff", func() error {
			_, err := q.TopProfile(context.Background(), TopProfileParams{Project: stockProject, ProfileID: "cur", BaseProfileID: "incompatible"})
			return err
		}, "building diff profile:", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run()
			require.ErrorContains(t, err, tc.contains)
			var fetchErr *ProfileFetchError
			assert.Equal(t, tc.fetch, errors.As(err, &fetchErr), "fetch/diff errors and analysis errors have distinct presentation")
		})
	}
}

func TestProfileNavigationValueTypesAndFiltering(t *testing.T) {
	p := buildTestProfile()
	p.SampleType = append(p.SampleType, &profile.ValueType{Type: "alloc_space", Unit: "bytes"})
	for _, s := range p.Sample {
		s.Value = append(s.Value, s.Value[0]*10)
	}
	q, _ := newStockProfilerQuerier(t, stockProfile("cur", 2, encodeProfile(t, p)))
	top, err := q.TopProfile(context.Background(), TopProfileParams{Project: stockProject, ProfileID: "cur", ValueIndex: 1, SortBy: "self", Filter: "sql.go"})
	require.NoError(t, err)
	assert.Equal(t, ValueTypeInfo{Index: 1, Type: "alloc_space", Unit: "bytes"}, top.ValueType)
	require.Len(t, top.AvailableValues, 2)
	assert.Equal(t, int64(1000), top.TotalValue)
	require.Len(t, top.TopFunctions, 1)
	assert.Equal(t, int64(500), top.TopFunctions[0].SelfValue)
	assert.False(t, top.Truncated)
	assert.Empty(t, top.TruncationHint)
}

func TestProfileNavigationZeroTotalAndBounds(t *testing.T) {
	// A wide, zero-total profile exposes both the warning and the hard node cap
	// without percentage pruning hiding an unbounded traversal.
	p := &profile.Profile{SampleType: []*profile.ValueType{{Type: "cpu", Unit: "nanoseconds"}}}
	for i := range 1100 {
		fn := &profile.Function{ID: uint64(i + 1), Name: fmt.Sprintf("function-%04d", i)}
		loc := &profile.Location{ID: uint64(i + 1), Line: []profile.Line{{Function: fn}}}
		p.Function = append(p.Function, fn)
		p.Location = append(p.Location, loc)
		value := int64(1)
		if i%2 == 1 {
			value = -1
		}
		p.Sample = append(p.Sample, &profile.Sample{Location: []*profile.Location{loc}, Value: []int64{value}})
	}
	q, _ := newStockProfilerQuerier(t, stockProfile("cur", 1, encodeProfile(t, p)))
	result, err := q.FlamegraphProfile(context.Background(), FlamegraphProfileParams{Project: stockProject, ProfileID: "cur", MinPct: -1})
	require.NoError(t, err)
	assert.Zero(t, result.TotalValue)
	assert.Equal(t, 3, result.MaxDepth)
	assert.Equal(t, 1.0, result.MinPct)
	assert.Contains(t, result.Warning, "Total profile value is zero")
	assert.Empty(t, result.Root.Children)
	assert.Equal(t, 1100, result.PrunedNodes)
	// Use a nonzero total to reach the node budget with every node above min_pct.
	for _, sample := range p.Sample {
		sample.Value[0] = 1
	}
	q, _ = newStockProfilerQuerier(t, stockProfile("wide", 2, encodeProfile(t, p)))
	result, err = q.FlamegraphProfile(context.Background(), FlamegraphProfileParams{Project: stockProject, ProfileID: "wide", MaxDepth: 100, MinPct: 0.01})
	require.NoError(t, err)
	assert.Equal(t, 6, result.MaxDepth)
	assert.Len(t, result.Root.Children, 999)
	assert.Equal(t, 101, result.PrunedNodes)
	assert.Empty(t, result.Warning)

	top, err := q.TopProfile(context.Background(), TopProfileParams{Project: stockProject, ProfileID: "wide", Limit: 100})
	require.NoError(t, err)
	assert.Len(t, top.TopFunctions, 50)
	assert.Contains(t, top.TruncationHint, "top 50 functions")
	top, err = q.TopProfile(context.Background(), TopProfileParams{Project: stockProject, ProfileID: "wide"})
	require.NoError(t, err)
	assert.Len(t, top.TopFunctions, 20)
	assert.Contains(t, top.TruncationHint, "top 20 functions")

	// One parent with many children reaches the caller/callee bound as well.
	parent := &profile.Function{ID: 2000, Name: "parent"}
	loc := &profile.Location{ID: 2000, Line: []profile.Line{{Function: parent}}}
	p.Function = append(p.Function, parent)
	p.Location = append(p.Location, loc)
	for _, sample := range p.Sample {
		sample.Location = append(sample.Location, loc)
	}
	q, _ = newStockProfilerQuerier(t, stockProfile("parent", 3, encodeProfile(t, p)))
	peek, err := q.PeekProfile(context.Background(), PeekProfileParams{Project: stockProject, ProfileID: "parent", FunctionName: "parent", Limit: 100})
	require.NoError(t, err)
	assert.Len(t, peek.Callees, 30)
	assert.Contains(t, peek.TruncationHint, "up to 30 callers/callees")
	peek, err = q.PeekProfile(context.Background(), PeekProfileParams{Project: stockProject, ProfileID: "parent", FunctionName: "parent"})
	require.NoError(t, err)
	assert.Len(t, peek.Callees, 10)
	assert.Contains(t, peek.TruncationHint, "up to 10 callers/callees")
}
