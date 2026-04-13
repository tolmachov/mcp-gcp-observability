package gcpdata

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/google/pprof/profile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildTestProfile creates a synthetic profile for testing.
//
// Call tree:
//
//	main (cumulative: 100)
//	├── handler (cumulative: 80)
//	│   ├── db.Query (cumulative: 50, self: 50)
//	│   └── json.Marshal (cumulative: 30, self: 30)
//	└── logger (cumulative: 20, self: 20)
func buildTestProfile() *profile.Profile {
	mainFn := &profile.Function{ID: 1, Name: "main.main", Filename: "main.go"}
	handlerFn := &profile.Function{ID: 2, Name: "myapp/handler.Handle", Filename: "handler.go"}
	dbQueryFn := &profile.Function{ID: 3, Name: "database/sql.(*DB).Query", Filename: "sql.go"}
	jsonMarshalFn := &profile.Function{ID: 4, Name: "encoding/json.Marshal", Filename: "encode.go"}
	loggerFn := &profile.Function{ID: 5, Name: "myapp/logger.Log", Filename: "logger.go"}

	locMain := &profile.Location{ID: 1, Line: []profile.Line{{Function: mainFn}}}
	locHandler := &profile.Location{ID: 2, Line: []profile.Line{{Function: handlerFn}}}
	locDB := &profile.Location{ID: 3, Line: []profile.Line{{Function: dbQueryFn}}}
	locJSON := &profile.Location{ID: 4, Line: []profile.Line{{Function: jsonMarshalFn}}}
	locLogger := &profile.Location{ID: 5, Line: []profile.Line{{Function: loggerFn}}}

	return &profile.Profile{
		SampleType: []*profile.ValueType{
			{Type: "cpu", Unit: "nanoseconds"},
		},
		Sample: []*profile.Sample{
			// Stack: db.Query <- handler <- main (leaf first)
			{Location: []*profile.Location{locDB, locHandler, locMain}, Value: []int64{50}},
			// Stack: json.Marshal <- handler <- main
			{Location: []*profile.Location{locJSON, locHandler, locMain}, Value: []int64{30}},
			// Stack: logger <- main
			{Location: []*profile.Location{locLogger, locMain}, Value: []int64{20}},
		},
		Location: []*profile.Location{locMain, locHandler, locDB, locJSON, locLogger},
		Function: []*profile.Function{mainFn, handlerFn, dbQueryFn, jsonMarshalFn, loggerFn},
	}
}

func TestTopFunctions_Cumulative(t *testing.T) {
	p := buildTestProfile()
	result, total, truncated, err := TopFunctions(p, 0, 10, "cumulative", "")
	require.NoError(t, err)
	assert.False(t, truncated)
	assert.Equal(t, int64(100), total)
	require.GreaterOrEqual(t, len(result), 3)

	// main.main should be first (cumulative 100).
	assert.Equal(t, "main.main", result[0].FunctionName)
	assert.Equal(t, int64(100), result[0].CumulativeValue)
	assert.Equal(t, 100.0, result[0].CumulativePct)
	assert.Equal(t, int64(0), result[0].SelfValue)

	// handler should be second (cumulative 80).
	assert.Equal(t, "myapp/handler.Handle", result[1].FunctionName)
	assert.Equal(t, int64(80), result[1].CumulativeValue)
}

func TestTopFunctions_Self(t *testing.T) {
	p := buildTestProfile()
	result, _, _, err := TopFunctions(p, 0, 10, "self", "")
	require.NoError(t, err)

	// db.Query has highest self cost (50).
	assert.Equal(t, "database/sql.(*DB).Query", result[0].FunctionName)
	assert.Equal(t, int64(50), result[0].SelfValue)
}

func TestTopFunctions_Filter(t *testing.T) {
	p := buildTestProfile()
	result, _, _, err := TopFunctions(p, 0, 10, "cumulative", "myapp")
	require.NoError(t, err)
	assert.Len(t, result, 2) // handler and logger
	for _, f := range result {
		assert.Contains(t, f.FunctionName, "myapp")
	}
}

func TestTopFunctions_Limit(t *testing.T) {
	p := buildTestProfile()
	result, _, truncated, err := TopFunctions(p, 0, 2, "cumulative", "")
	require.NoError(t, err)
	assert.True(t, truncated)
	assert.Len(t, result, 2)
}

func TestTopFunctions_InvalidValueIndex(t *testing.T) {
	p := buildTestProfile()
	_, _, _, err := TopFunctions(p, 5, 10, "cumulative", "")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "value_index 5 out of range")
}

func TestPeekFunction(t *testing.T) {
	p := buildTestProfile()
	funcInfo, callers, callees, err := PeekFunction(p, "myapp/handler.Handle", 0, 10)
	require.NoError(t, err)

	assert.Equal(t, "myapp/handler.Handle", funcInfo.Name)
	assert.Equal(t, int64(0), funcInfo.Self)
	assert.Equal(t, int64(80), funcInfo.Cumulative)

	// Caller should be main.main.
	require.Len(t, callers, 1)
	assert.Equal(t, "main.main", callers[0].Name)
	assert.Equal(t, int64(80), callers[0].Cost)

	// Callees should be db.Query and json.Marshal.
	require.Len(t, callees, 2)
	// Sorted by cost desc.
	assert.Equal(t, "database/sql.(*DB).Query", callees[0].Name)
	assert.Equal(t, int64(50), callees[0].Cost)
	assert.Equal(t, "encoding/json.Marshal", callees[1].Name)
	assert.Equal(t, int64(30), callees[1].Cost)
}

func TestPeekFunction_NotFound(t *testing.T) {
	p := buildTestProfile()
	_, _, _, err := PeekFunction(p, "nonexistent.Function", 0, 10)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no function matching")
}

func TestPeekFunction_Ambiguous(t *testing.T) {
	p := buildTestProfile()
	// "myapp/" matches both handler.Handle and logger.Log.
	_, _, _, err := PeekFunction(p, "myapp/", 0, 10)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "ambiguous")
}

func TestFlamegraph_FullTree(t *testing.T) {
	p := buildTestProfile()
	root, total, pruned, err := Flamegraph(p, "", 0, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(100), total)
	assert.Equal(t, 0, pruned)
	assert.Equal(t, "(root)", root.Name)

	// Root should have one child: main.main.
	require.Len(t, root.Children, 1)
	mainNode := root.Children[0]
	assert.Equal(t, "main.main", mainNode.Name)
	assert.Equal(t, int64(100), mainNode.Cumulative)

	// main should have two children: handler and logger.
	require.Len(t, mainNode.Children, 2)
}

func TestFlamegraph_DepthLimit(t *testing.T) {
	p := buildTestProfile()
	root, _, pruned, err := Flamegraph(p, "", 0, 1, 0)
	require.NoError(t, err)

	// Depth 1 means root has children but children don't.
	require.Len(t, root.Children, 1)
	mainNode := root.Children[0]
	assert.Empty(t, mainNode.Children)
	assert.Greater(t, pruned, 0) // handler and logger pruned
}

func TestFlamegraph_MinPct(t *testing.T) {
	p := buildTestProfile()
	root, _, pruned, err := Flamegraph(p, "", 0, 10, 25)
	require.NoError(t, err)

	// logger (20%) should be pruned. handler (80%) should remain.
	mainNode := root.Children[0]
	assert.Len(t, mainNode.Children, 1) // only handler
	assert.Equal(t, "myapp/handler.Handle", mainNode.Children[0].Name)
	assert.Greater(t, pruned, 0)
}

func TestFlamegraph_Subtree(t *testing.T) {
	p := buildTestProfile()
	root, _, _, err := Flamegraph(p, "handler.Handle", 0, 10, 0)
	require.NoError(t, err)

	assert.Equal(t, "myapp/handler.Handle", root.Name)
	assert.Equal(t, int64(80), root.Cumulative)
	require.Len(t, root.Children, 2) // db.Query and json.Marshal
}

func TestFlamegraph_SubtreeNotFound(t *testing.T) {
	p := buildTestProfile()
	_, _, _, err := Flamegraph(p, "nonexistent", 0, 10, 0)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found in profile call tree")
}

func TestScanFunctionCosts_WithFilter(t *testing.T) {
	p := buildTestProfile()
	costs, total, err := scanFunctionCosts(p, 0, "handler", nil)
	require.NoError(t, err)
	assert.Equal(t, int64(100), total)
	require.Contains(t, costs, "myapp/handler.Handle")
	assert.Equal(t, int64(0), costs["myapp/handler.Handle"].self)
	assert.Equal(t, int64(80), costs["myapp/handler.Handle"].cumulative)
	// Other functions not matching "handler" should be absent.
	assert.NotContains(t, costs, "main.main")
}

func TestScanFunctionCosts_WithTargets(t *testing.T) {
	p := buildTestProfile()
	targets := map[string]bool{"main.main": true, "myapp/logger.Log": true}
	costs, total, err := scanFunctionCosts(p, 0, "", targets)
	require.NoError(t, err)
	assert.Equal(t, int64(100), total)
	require.Len(t, costs, 2)
	assert.Equal(t, int64(100), costs["main.main"].cumulative)
	assert.Equal(t, int64(20), costs["myapp/logger.Log"].self)
}

func TestProfileValueTypes(t *testing.T) {
	p := &profile.Profile{
		SampleType: []*profile.ValueType{
			{Type: "alloc_space", Unit: "bytes"},
			{Type: "alloc_objects", Unit: "count"},
		},
	}
	types := ProfileValueTypes(p)
	require.Len(t, types, 2)
	assert.Equal(t, "alloc_space", types[0].Type)
	assert.Equal(t, "bytes", types[0].Unit)
	assert.Equal(t, 0, types[0].Index)
	assert.Equal(t, "alloc_objects", types[1].Type)
	assert.Equal(t, 1, types[1].Index)
}

func TestSafePercent(t *testing.T) {
	assert.Equal(t, 50.0, safePercent(50, 100))
	assert.Equal(t, 0.0, safePercent(50, 0))
	assert.Equal(t, 33.33, safePercent(1, 3))
	assert.Equal(t, -25.0, safePercent(-25, 100))
}

func TestScanFunctionCosts_InvalidValueIndex(t *testing.T) {
	p := buildTestProfile()
	_, _, err := scanFunctionCosts(p, 5, "", nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "value_index 5 out of range")
}

func TestValidateTimeFilters(t *testing.T) {
	// Both empty.
	s, e, err := validateTimeFilters("", "")
	require.NoError(t, err)
	assert.True(t, s.IsZero())
	assert.True(t, e.IsZero())

	// Valid start, empty end.
	s, _, err = validateTimeFilters("2024-01-15T00:00:00Z", "")
	require.NoError(t, err)
	assert.Equal(t, 2024, s.Year())

	// Empty start, valid end.
	_, e, err = validateTimeFilters("", "2024-12-31T23:59:59Z")
	require.NoError(t, err)
	assert.Equal(t, 12, int(e.Month()))

	// Invalid start.
	_, _, err = validateTimeFilters("not-a-date", "")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid start_time")

	// Invalid end.
	_, _, err = validateTimeFilters("", "also-bad")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid end_time")
}

func TestMatchesProfileFilter(t *testing.T) {
	meta := ProfileMeta{
		ProfileType: "CPU",
		Target:      "my-service",
		StartTime:   "2024-06-15T12:00:00Z",
	}
	startT, _ := time.Parse(time.RFC3339, "2024-06-01T00:00:00Z")
	endT, _ := time.Parse(time.RFC3339, "2024-06-30T00:00:00Z")
	zero := time.Time{}

	check := func(wantMatch, wantParseErr bool, profileType, target string, s, e time.Time, m ProfileMeta) {
		t.Helper()
		match, parseErr := matchesProfileFilter(m, profileType, target, s, e)
		assert.Equal(t, wantMatch, match, "match")
		assert.Equal(t, wantParseErr, parseErr, "parseErr")
	}

	// No filters — always matches.
	check(true, false, "", "", zero, zero, meta)

	// Profile type filter (case-insensitive).
	check(true, false, "cpu", "", zero, zero, meta)
	check(true, false, "CPU", "", zero, zero, meta)
	check(false, false, "HEAP", "", zero, zero, meta)

	// Target filter (exact match).
	check(true, false, "", "my-service", zero, zero, meta)
	check(false, false, "", "other-service", zero, zero, meta)

	// Time range filter — within range.
	check(true, false, "", "", startT, endT, meta)

	// Time range filter — before start.
	afterStart, _ := time.Parse(time.RFC3339, "2024-07-01T00:00:00Z")
	check(false, false, "", "", afterStart, zero, meta)

	// Time range filter — after end.
	beforeEnd, _ := time.Parse(time.RFC3339, "2024-06-01T00:00:00Z")
	check(false, false, "", "", zero, beforeEnd, meta)

	// Unparseable StartTime is excluded when time filter is active.
	badMeta := ProfileMeta{ProfileType: "CPU", StartTime: "not-a-timestamp"}
	check(false, true, "", "", startT, zero, badMeta)

	// Unparseable StartTime passes when no time filter is active.
	check(true, false, "", "", zero, zero, badMeta)
}

func TestBuildDiffProfile(t *testing.T) {
	// Create two independent profiles with the same structure but different values.
	// Profiles must be serialized and re-parsed to fully initialize internal state
	// required by profile.Merge.
	makeSingleFuncProfile := func(value int64) *profile.Profile {
		fn := &profile.Function{ID: 1, Name: "worker", Filename: "worker.go"}
		loc := &profile.Location{ID: 1, Line: []profile.Line{{Function: fn}}}
		p := &profile.Profile{
			SampleType: []*profile.ValueType{{Type: "cpu", Unit: "nanoseconds"}},
			Sample:     []*profile.Sample{{Location: []*profile.Location{loc}, Value: []int64{value}}},
			Location:   []*profile.Location{loc},
			Function:   []*profile.Function{fn},
		}
		var buf bytes.Buffer
		require.NoError(t, p.Write(&buf))
		parsed, err := profile.Parse(&buf)
		require.NoError(t, err)
		return parsed
	}

	current := makeSingleFuncProfile(100)
	base := makeSingleFuncProfile(60)

	diff, err := buildDiffProfile(current, base)
	require.NoError(t, err)
	require.NotNil(t, diff)

	// The diff should show the delta (100 - 60 = 40).
	topFuncs, total, _, err := TopFunctions(diff, 0, 10, "cumulative", "")
	require.NoError(t, err)
	assert.Equal(t, int64(40), total)
	require.Len(t, topFuncs, 1)
	assert.Equal(t, "worker", topFuncs[0].FunctionName)
	assert.Equal(t, int64(40), topFuncs[0].CumulativeValue)

	// Verify the original base was not mutated.
	assert.Equal(t, int64(60), base.Sample[0].Value[0])
}

func TestValidateProfileType(t *testing.T) {
	// Empty is valid (no filter).
	assert.NoError(t, ValidateProfileType(""))

	// Valid types (case-insensitive).
	assert.NoError(t, ValidateProfileType("CPU"))
	assert.NoError(t, ValidateProfileType("cpu"))
	assert.NoError(t, ValidateProfileType("Heap"))

	// Invalid type.
	err := ValidateProfileType("GARBAGE")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid profile_type")
}

func TestValidateValueIndex_EmptySampleTypes(t *testing.T) {
	p := &profile.Profile{}
	err := validateValueIndex(p, 0)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "profile has no sample types")
}

func TestTopFunctions_EmptyProfile(t *testing.T) {
	p := &profile.Profile{
		SampleType: []*profile.ValueType{{Type: "cpu", Unit: "nanoseconds"}},
	}
	result, total, truncated, err := TopFunctions(p, 0, 10, "cumulative", "")
	require.NoError(t, err)
	assert.Empty(t, result)
	assert.Equal(t, int64(0), total)
	assert.False(t, truncated)
}

func TestTopFunctions_NegativeValues(t *testing.T) {
	// Simulate a diff profile where some values are negative.
	fn1 := &profile.Function{ID: 1, Name: "improved", Filename: "a.go"}
	fn2 := &profile.Function{ID: 2, Name: "regressed", Filename: "b.go"}
	loc1 := &profile.Location{ID: 1, Line: []profile.Line{{Function: fn1}}}
	loc2 := &profile.Location{ID: 2, Line: []profile.Line{{Function: fn2}}}
	p := &profile.Profile{
		SampleType: []*profile.ValueType{{Type: "cpu", Unit: "nanoseconds"}},
		Sample: []*profile.Sample{
			{Location: []*profile.Location{loc1}, Value: []int64{-30}},
			{Location: []*profile.Location{loc2}, Value: []int64{50}},
		},
		Location: []*profile.Location{loc1, loc2},
		Function: []*profile.Function{fn1, fn2},
	}

	result, total, _, err := TopFunctions(p, 0, 10, "cumulative", "")
	require.NoError(t, err)
	assert.Equal(t, int64(20), total) // -30 + 50 = 20

	// Sorted by absolute cumulative, so regressed (|50|) comes first.
	require.Len(t, result, 2)
	assert.Equal(t, "regressed", result[0].FunctionName)
	assert.Equal(t, int64(50), result[0].CumulativeValue)
	assert.Equal(t, "improved", result[1].FunctionName)
	assert.Equal(t, int64(-30), result[1].CumulativeValue)
}

func TestPeekFunction_NegativeValues(t *testing.T) {
	// Diff profile with negative costs.
	fn1 := &profile.Function{ID: 1, Name: "caller", Filename: "a.go"}
	fn2 := &profile.Function{ID: 2, Name: "target", Filename: "b.go"}
	loc1 := &profile.Location{ID: 1, Line: []profile.Line{{Function: fn1}}}
	loc2 := &profile.Location{ID: 2, Line: []profile.Line{{Function: fn2}}}
	p := &profile.Profile{
		SampleType: []*profile.ValueType{{Type: "cpu", Unit: "nanoseconds"}},
		Sample: []*profile.Sample{
			{Location: []*profile.Location{loc2, loc1}, Value: []int64{-40}},
		},
		Location: []*profile.Location{loc1, loc2},
		Function: []*profile.Function{fn1, fn2},
	}

	info, callers, _, err := PeekFunction(p, "target", 0, 10)
	require.NoError(t, err)
	assert.Equal(t, int64(-40), info.Cumulative)
	assert.Equal(t, int64(-40), info.Self)
	require.Len(t, callers, 1)
	assert.Equal(t, "caller", callers[0].Name)
	assert.Equal(t, int64(-40), callers[0].Cost)
}

func TestPeekFunction_ManyAmbiguous(t *testing.T) {
	// Create a profile with >10 functions matching "func".
	var funcs []*profile.Function
	var locs []*profile.Location
	var samples []*profile.Sample
	for i := 1; i <= 15; i++ {
		fn := &profile.Function{ID: uint64(i), Name: fmt.Sprintf("pkg.func%d", i), Filename: "a.go"}
		loc := &profile.Location{ID: uint64(i), Line: []profile.Line{{Function: fn}}}
		funcs = append(funcs, fn)
		locs = append(locs, loc)
		samples = append(samples, &profile.Sample{Location: []*profile.Location{loc}, Value: []int64{int64(i)}})
	}
	p := &profile.Profile{
		SampleType: []*profile.ValueType{{Type: "cpu", Unit: "nanoseconds"}},
		Sample:     samples,
		Location:   locs,
		Function:   funcs,
	}

	_, _, _, err := PeekFunction(p, "func", 0, 10)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "matches 15 functions")
	assert.Contains(t, err.Error(), "first 10")
}

func TestFlamegraph_SelfCosts(t *testing.T) {
	p := buildTestProfile()
	root, _, _, err := Flamegraph(p, "", 0, 10, 0)
	require.NoError(t, err)

	mainNode := root.Children[0]
	assert.Equal(t, int64(0), mainNode.Self, "main has no self cost")

	// Find handler node.
	var handlerNode *FlamegraphNode
	for i := range mainNode.Children {
		if mainNode.Children[i].Name == "myapp/handler.Handle" {
			handlerNode = &mainNode.Children[i]
			break
		}
	}
	require.NotNil(t, handlerNode, "handler node should exist")
	assert.Equal(t, int64(0), handlerNode.Self, "handler has no self cost")

	// Find db.Query under handler.
	for _, child := range handlerNode.Children {
		if child.Name == "database/sql.(*DB).Query" {
			assert.Equal(t, int64(50), child.Self, "db.Query is a leaf with self=50")
		}
	}
}

func TestFlamegraph_NegativeValues(t *testing.T) {
	fn1 := &profile.Function{ID: 1, Name: "root", Filename: "a.go"}
	fn2 := &profile.Function{ID: 2, Name: "child", Filename: "b.go"}
	loc1 := &profile.Location{ID: 1, Line: []profile.Line{{Function: fn1}}}
	loc2 := &profile.Location{ID: 2, Line: []profile.Line{{Function: fn2}}}
	p := &profile.Profile{
		SampleType: []*profile.ValueType{{Type: "cpu", Unit: "nanoseconds"}},
		Sample: []*profile.Sample{
			{Location: []*profile.Location{loc2, loc1}, Value: []int64{-20}},
		},
		Location: []*profile.Location{loc1, loc2},
		Function: []*profile.Function{fn1, fn2},
	}

	root, total, _, err := Flamegraph(p, "", 0, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(-20), total)
	require.Len(t, root.Children, 1)
	assert.Equal(t, int64(-20), root.Children[0].Cumulative)
}

func TestBuildDiffProfile_CurrentNotMutated(t *testing.T) {
	makeSingleFuncProfile := func(value int64) *profile.Profile {
		fn := &profile.Function{ID: 1, Name: "worker", Filename: "worker.go"}
		loc := &profile.Location{ID: 1, Line: []profile.Line{{Function: fn}}}
		p := &profile.Profile{
			SampleType: []*profile.ValueType{{Type: "cpu", Unit: "nanoseconds"}},
			Sample:     []*profile.Sample{{Location: []*profile.Location{loc}, Value: []int64{value}}},
			Location:   []*profile.Location{loc},
			Function:   []*profile.Function{fn},
		}
		var buf bytes.Buffer
		require.NoError(t, p.Write(&buf))
		parsed, err := profile.Parse(&buf)
		require.NoError(t, err)
		return parsed
	}

	current := makeSingleFuncProfile(100)
	base := makeSingleFuncProfile(60)

	_, err := buildDiffProfile(current, base)
	require.NoError(t, err)

	// Verify neither profile was mutated.
	assert.Equal(t, int64(100), current.Sample[0].Value[0], "current must not be mutated")
	assert.Equal(t, int64(60), base.Sample[0].Value[0], "base must not be mutated")
}

func TestCompareProfiles_DeltaComputation(t *testing.T) {
	// Build two profiles with overlapping functions but different costs.
	// current: funcA=100cum, funcB=50cum
	// base:    funcA=80cum,  funcC=30cum
	// Expected: funcA delta=+20 (regression), funcB delta=+50 (new), funcC delta=-30 (improvement)
	makeProfile := func(funcs map[string]int64) *profile.Profile {
		var fns []*profile.Function
		var locs []*profile.Location
		var samples []*profile.Sample
		id := uint64(1)
		for name, val := range funcs {
			fn := &profile.Function{ID: id, Name: name, Filename: name + ".go"}
			loc := &profile.Location{ID: id, Line: []profile.Line{{Function: fn}}}
			fns = append(fns, fn)
			locs = append(locs, loc)
			samples = append(samples, &profile.Sample{Location: []*profile.Location{loc}, Value: []int64{val}})
			id++
		}
		p := &profile.Profile{
			SampleType: []*profile.ValueType{{Type: "cpu", Unit: "nanoseconds"}},
			Sample:     samples,
			Location:   locs,
			Function:   fns,
		}
		var buf bytes.Buffer
		if err := p.Write(&buf); err != nil {
			t.Fatal(err)
		}
		parsed, err := profile.Parse(&buf)
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}

	currentProfile := makeProfile(map[string]int64{"funcA": 100, "funcB": 50})
	baseProfile := makeProfile(map[string]int64{"funcA": 80, "funcC": 30})

	// Put profiles in cache so CompareProfiles can find them.
	cache := NewProfileCache(10)
	cache.Put("proj/current", currentProfile, ProfileMeta{ProfileID: "current"})
	cache.Put("proj/base", baseProfile, ProfileMeta{ProfileID: "base"})

	// Call the internal comparison logic directly by analyzing profiles manually.
	// (CompareProfiles needs a real API service; test the delta logic via TopFunctions + manual delta.)
	currentTop, currentTotal, _, err := TopFunctions(currentProfile, 0, 0, "cumulative", "")
	require.NoError(t, err)
	baseTop, baseTotal, _, err := TopFunctions(baseProfile, 0, 0, "cumulative", "")
	require.NoError(t, err)

	assert.Equal(t, int64(150), currentTotal)
	assert.Equal(t, int64(110), baseTotal)

	// Build delta map (same logic as CompareProfiles).
	type deltaEntry struct {
		name  string
		delta int64
	}
	deltas := make(map[string]*deltaEntry)
	for _, f := range currentTop {
		deltas[f.FunctionName] = &deltaEntry{name: f.FunctionName, delta: f.CumulativeValue}
	}
	for _, f := range baseTop {
		e, ok := deltas[f.FunctionName]
		if !ok {
			e = &deltaEntry{name: f.FunctionName}
			deltas[f.FunctionName] = e
		}
		e.delta -= f.CumulativeValue
	}

	// funcA: 100 - 80 = +20 (regression)
	assert.Equal(t, int64(20), deltas["funcA"].delta)
	// funcB: 50 - 0 = +50 (new/regression)
	assert.Equal(t, int64(50), deltas["funcB"].delta)
	// funcC: 0 - 30 = -30 (improvement)
	assert.Equal(t, int64(-30), deltas["funcC"].delta)
}

func TestMatchesProfileFilter_EmptyStartTimeExcludedWithTimeFilter(t *testing.T) {
	startT, _ := time.Parse(time.RFC3339, "2024-06-01T00:00:00Z")
	zero := time.Time{}

	meta := ProfileMeta{ProfileType: "CPU", Target: "svc", StartTime: ""}

	// Empty StartTime with time filter → excluded (parseErr=true).
	match, parseErr := matchesProfileFilter(meta, "", "", startT, zero)
	assert.False(t, match, "empty StartTime should be excluded when time filter is active")
	assert.True(t, parseErr, "should report parseErr for empty StartTime")

	// Empty StartTime without time filter → included.
	match, parseErr = matchesProfileFilter(meta, "", "", zero, zero)
	assert.True(t, match, "empty StartTime should pass when no time filter")
	assert.False(t, parseErr)
}

func TestFlamegraph_SubtreeAmbiguous(t *testing.T) {
	// Profile with two functions both containing "Marshal".
	fn1 := &profile.Function{ID: 1, Name: "root", Filename: "a.go"}
	fn2 := &profile.Function{ID: 2, Name: "json.Marshal", Filename: "b.go"}
	fn3 := &profile.Function{ID: 3, Name: "xml.Marshal", Filename: "c.go"}
	loc1 := &profile.Location{ID: 1, Line: []profile.Line{{Function: fn1}}}
	loc2 := &profile.Location{ID: 2, Line: []profile.Line{{Function: fn2}}}
	loc3 := &profile.Location{ID: 3, Line: []profile.Line{{Function: fn3}}}
	p := &profile.Profile{
		SampleType: []*profile.ValueType{{Type: "cpu", Unit: "nanoseconds"}},
		Sample: []*profile.Sample{
			{Location: []*profile.Location{loc2, loc1}, Value: []int64{50}},
			{Location: []*profile.Location{loc3, loc1}, Value: []int64{30}},
		},
		Location: []*profile.Location{loc1, loc2, loc3},
		Function: []*profile.Function{fn1, fn2, fn3},
	}

	_, _, _, err := Flamegraph(p, "Marshal", 0, 10, 0)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "matches 2 nodes")
	assert.Contains(t, err.Error(), "json.Marshal")
	assert.Contains(t, err.Error(), "xml.Marshal")
}

func TestFlamegraph_ZeroTotalValue(t *testing.T) {
	// Diff profile where values cancel out: +50 and -50.
	fn1 := &profile.Function{ID: 1, Name: "root", Filename: "a.go"}
	fn2 := &profile.Function{ID: 2, Name: "child", Filename: "b.go"}
	loc1 := &profile.Location{ID: 1, Line: []profile.Line{{Function: fn1}}}
	loc2 := &profile.Location{ID: 2, Line: []profile.Line{{Function: fn2}}}
	p := &profile.Profile{
		SampleType: []*profile.ValueType{{Type: "cpu", Unit: "nanoseconds"}},
		Sample: []*profile.Sample{
			{Location: []*profile.Location{loc2, loc1}, Value: []int64{50}},
			{Location: []*profile.Location{loc2, loc1}, Value: []int64{-50}},
		},
		Location: []*profile.Location{loc1, loc2},
		Function: []*profile.Function{fn1, fn2},
	}

	root, total, _, err := Flamegraph(p, "", 0, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(0), total)
	// Root should exist even with zero total.
	assert.Equal(t, "(root)", root.Name)
}

func TestValidateProfileType_CaseInsensitive(t *testing.T) {
	// Ensure validation normalizes to uppercase internally.
	assert.NoError(t, ValidateProfileType("wall"))
	assert.NoError(t, ValidateProfileType("Wall"))
	assert.NoError(t, ValidateProfileType("WALL"))
	assert.NoError(t, ValidateProfileType("heap_alloc"))
	assert.Error(t, ValidateProfileType("INVALID_TYPE"))
}
