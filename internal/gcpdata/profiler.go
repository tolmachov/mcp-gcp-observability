package gcpdata

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/google/pprof/profile"
	cloudprofiler "google.golang.org/api/cloudprofiler/v2"
)

const profilerQueryTimeout = 60 * time.Second

// validProfileTypes is the set of profile types supported by Cloud Profiler.
var validProfileTypes = map[string]bool{
	"CPU": true, "WALL": true, "HEAP": true, "THREADS": true,
	"CONTENTION": true, "PEAK_HEAP": true, "HEAP_ALLOC": true,
}

// ValidateProfileType returns an error if profileType is non-empty and not a known type.
func ValidateProfileType(profileType string) error {
	if profileType == "" {
		return nil
	}
	upper := strings.ToUpper(profileType)
	if !validProfileTypes[upper] {
		return fmt.Errorf("invalid profile_type %q. Valid types: CPU, WALL, HEAP, THREADS, CONTENTION, PEAK_HEAP, HEAP_ALLOC", profileType)
	}
	return nil
}

// ListProfiles lists profile metadata without downloading profile bytes.
// As of 2026-04, the Cloud Profiler API does not support server-side filtering
// by profile_type, target, or time range, so filtering is applied client-side. To ensure the
// caller receives up to pageSize matching results, this function paginates
// internally through API pages until enough matches are found.
func ListProfiles(
	ctx context.Context,
	svc *cloudprofiler.Service,
	project string,
	profileType, target, startTime, endTime string,
	pageSize int,
	pageToken string,
) (*ProfileListResult, error) {
	ctx, cancel := context.WithTimeout(ctx, profilerQueryTimeout)
	defer cancel()

	startT, endT, err := validateTimeFilters(startTime, endTime)
	if err != nil {
		return nil, err
	}

	needsFilter := profileType != "" || target != "" || startTime != "" || endTime != ""

	result := &ProfileListResult{
		Summary: ProfileSummary{
			CountByType:   make(map[string]int),
			CountByTarget: make(map[string]int),
		},
	}

	// When filtering client-side, fetch larger API pages to reduce round-trips.
	apiPageSize := int64(pageSize)
	if needsFilter && apiPageSize < 1000 {
		apiPageSize = 1000
	}

	const maxListPages = 20
	currentToken := pageToken
	pagesScanned := 0
	for range maxListPages {
		pagesScanned++
		call := svc.Projects.Profiles.List("projects/" + project).
			PageSize(apiPageSize).
			Context(ctx)
		if currentToken != "" {
			call = call.PageToken(currentToken)
		}

		resp, err := call.Do()
		if err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("listing profiles: timed out after %d pages (%d matches so far); try narrowing filters: %w", pagesScanned, len(result.Profiles), err)
			}
			return nil, fmt.Errorf("listing profiles: %w", err)
		}

		for _, p := range resp.Profiles {
			meta := profileFromAPI(p)

			ok, wasParseErr := matchesProfileFilter(meta, profileType, target, startT, endT)
			if !ok {
				if wasParseErr {
					result.ExcludedCount++
				}
				continue
			}

			result.Profiles = append(result.Profiles, meta)
			result.Summary.CountByType[meta.ProfileType]++
			result.Summary.CountByTarget[meta.Target]++
		}

		// Trim to exactly pageSize to honour the caller's contract.
		if len(result.Profiles) >= pageSize {
			result.Profiles = result.Profiles[:pageSize]
			// Recompute summary for trimmed set.
			result.Summary.CountByType = make(map[string]int)
			result.Summary.CountByTarget = make(map[string]int)
			for _, m := range result.Profiles {
				result.Summary.CountByType[m.ProfileType]++
				result.Summary.CountByTarget[m.Target]++
			}
			if resp.NextPageToken != "" {
				result.NextPageToken = resp.NextPageToken
			}
			result.Count = len(result.Profiles)
			return result, nil
		}

		if resp.NextPageToken == "" {
			break
		}
		currentToken = resp.NextPageToken
	}

	if needsFilter && currentToken != "" && len(result.Profiles) < pageSize {
		result.Warning = fmt.Sprintf("Scanned %d API pages without finding %d matching profiles (found %d). Try narrowing your filters or increasing limit.", maxListPages, pageSize, len(result.Profiles))
	}

	result.Count = len(result.Profiles)
	return result, nil
}

// validateTimeFilters parses and validates time filter strings.
// Returns zero-value times for empty strings.
func validateTimeFilters(startTime, endTime string) (time.Time, time.Time, error) {
	var startT, endT time.Time
	if startTime != "" {
		var err error
		startT, err = time.Parse(time.RFC3339, startTime)
		if err != nil {
			return startT, endT, fmt.Errorf("invalid start_time %q: must be RFC3339 format (e.g. 2024-01-15T00:00:00Z)", startTime)
		}
	}
	if endTime != "" {
		var err error
		endT, err = time.Parse(time.RFC3339, endTime)
		if err != nil {
			return startT, endT, fmt.Errorf("invalid end_time %q: must be RFC3339 format (e.g. 2024-01-15T23:59:59Z)", endTime)
		}
	}
	return startT, endT, nil
}

// matchesProfileFilter returns whether the profile matches all filters.
// parseErr is true when the profile was excluded due to an unparseable timestamp
// (so the caller can track how many were excluded for user feedback).
func matchesProfileFilter(meta ProfileMeta, profileType, target string, startT, endT time.Time) (match, parseErr bool) {
	if profileType != "" && !strings.EqualFold(meta.ProfileType, profileType) {
		return false, false
	}
	if target != "" && meta.Target != target {
		return false, false
	}
	hasTimeFilter := !startT.IsZero() || !endT.IsZero()
	if hasTimeFilter && meta.StartTime == "" {
		return false, true // exclude: no timestamp to compare against
	}
	if hasTimeFilter {
		mt, err := time.Parse(time.RFC3339, meta.StartTime)
		if err != nil {
			// Exclude profiles with unparseable timestamps when a time filter
			// is active — including them would silently bypass the filter.
			return false, true
		}
		if !startT.IsZero() && mt.Before(startT) {
			return false, false
		}
		if !endT.IsZero() && mt.After(endT) {
			return false, false
		}
	}
	return true, false
}

// GetOrFetchProfile retrieves a parsed profile from cache or downloads it.
func GetOrFetchProfile(
	ctx context.Context,
	svc *cloudprofiler.Service,
	cache *ProfileCache,
	project, profileName string,
) (*profile.Profile, ProfileMeta, error) {
	key := project + "/" + profileName
	if p, meta, ok := cache.Get(key); ok {
		return p, meta, nil
	}

	// Diff profiles only exist in the in-memory cache. If we missed,
	// the user needs to re-run profiler.compare to regenerate.
	if strings.HasPrefix(profileName, "diff:") {
		return nil, ProfileMeta{}, fmt.Errorf("diff profile %q not in cache (expired or evicted). Re-run profiler.compare to regenerate it", profileName)
	}

	ctx, cancel := context.WithTimeout(ctx, profilerQueryTimeout)
	defer cancel()

	// The profile name from the API is "projects/{project}/profiles/{id}".
	// If the user provides just the ID, construct the full resource name.
	resourceName := profileName
	if !strings.HasPrefix(profileName, "projects/") {
		resourceName = "projects/" + project + "/profiles/" + profileName
	}

	// As of Cloud Profiler API v2 (checked 2026-04), there is no direct GET
	// endpoint for a single profile by ID, so we paginate through list results
	// and request profileBytes explicitly via Fields(). If the API adds a direct
	// GET, replace this scan with a direct fetch.
	const maxPages = 10
	var found *cloudprofiler.Profile
	pageToken := ""
	for range maxPages {
		call := svc.Projects.Profiles.List("projects/" + project).
			PageSize(1000).
			Fields("profiles(name,profileType,deployment,startTime,duration,labels,profileBytes)", "nextPageToken").
			Context(ctx)
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}
		resp, err := call.Do()
		if err != nil {
			return nil, ProfileMeta{}, fmt.Errorf("fetching profile: %w", err)
		}
		for _, p := range resp.Profiles {
			if p.Name == resourceName || p.Name == profileName {
				found = p
				break
			}
		}
		if found != nil || resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}

	if found == nil {
		return nil, ProfileMeta{}, fmt.Errorf("profile %q not found in project %q within the first %d API pages (~%d profiles). The profile may exist further back; try a more recent profile from profiler.list results", profileName, project, maxPages, maxPages*1000)
	}

	if found.ProfileBytes == "" {
		return nil, ProfileMeta{}, fmt.Errorf("profile %q has no profile bytes (metadata-only response)", profileName)
	}

	data, err := base64.StdEncoding.DecodeString(found.ProfileBytes)
	if err != nil {
		return nil, ProfileMeta{}, fmt.Errorf("decoding profile bytes: %w", err)
	}

	p, err := profile.Parse(bytes.NewReader(data))
	if err != nil {
		return nil, ProfileMeta{}, fmt.Errorf("parsing pprof data: %w", err)
	}

	meta := profileFromAPI(found)
	cache.Put(key, p, meta)
	return p, meta, nil
}

// TopFunctions computes a flat ranking of functions by self or cumulative cost.
// Returns the ranked functions, the total value, whether results were truncated, and any error.
func TopFunctions(p *profile.Profile, valueIndex, limit int, sortBy, filter string) ([]TopFunction, int64, bool, error) {
	if err := validateValueIndex(p, valueIndex); err != nil {
		return nil, 0, false, err
	}

	type funcStats struct {
		name       string
		file       string
		self       int64
		cumulative int64
	}
	stats := make(map[string]*funcStats)
	var total int64

	for _, sample := range p.Sample {
		value := sample.Value[valueIndex]
		total += value
		seen := make(map[string]bool)
		for i, loc := range sample.Location {
			for _, line := range loc.Line {
				if line.Function == nil {
					continue
				}
				fname := line.Function.Name
				s, ok := stats[fname]
				if !ok {
					s = &funcStats{name: fname, file: line.Function.Filename}
					stats[fname] = s
				}
				if i == 0 { // leaf
					s.self += value
				}
				if !seen[fname] {
					s.cumulative += value
					seen[fname] = true
				}
			}
		}
	}

	var result []TopFunction
	for _, s := range stats {
		if filter != "" && !strings.Contains(s.name, filter) && !strings.Contains(s.file, filter) {
			continue
		}
		result = append(result, TopFunction{
			FunctionName:    s.name,
			File:            s.file,
			SelfValue:       s.self,
			SelfPct:         safePercent(s.self, total),
			CumulativeValue: s.cumulative,
			CumulativePct:   safePercent(s.cumulative, total),
		})
	}

	switch sortBy {
	case "self":
		sort.Slice(result, func(i, j int) bool {
			return absInt64(result[i].SelfValue) > absInt64(result[j].SelfValue)
		})
	default: // "cumulative"
		sort.Slice(result, func(i, j int) bool {
			return absInt64(result[i].CumulativeValue) > absInt64(result[j].CumulativeValue)
		})
	}

	truncated := limit > 0 && len(result) > limit
	if truncated {
		result = result[:limit]
	}

	return result, total, truncated, nil
}

// PeekFunction finds callers and callees of a named function.
func PeekFunction(p *profile.Profile, functionName string, valueIndex, limit int) (*PeekFunctionInfo, []PeekEntry, []PeekEntry, error) {
	if err := validateValueIndex(p, valueIndex); err != nil {
		return nil, nil, nil, err
	}

	matches := findMatchingFunctions(p, functionName)
	if len(matches) == 0 {
		return nil, nil, nil, fmt.Errorf("no function matching %q found in profile", functionName)
	}
	if len(matches) > 10 {
		names := matches[:10]
		return nil, nil, nil, fmt.Errorf("function name %q is ambiguous, matches %d functions (first 10: %s). Use a more specific name", functionName, len(matches), strings.Join(names, ", "))
	}
	if len(matches) > 1 {
		return nil, nil, nil, fmt.Errorf("function name %q is ambiguous, matches: %s. Use a more specific name", functionName, strings.Join(matches, ", "))
	}

	targetName := matches[0]
	var targetFile string

	type costEntry struct {
		name string
		file string
		cost int64
	}
	callerCosts := make(map[string]*costEntry)
	calleeCosts := make(map[string]*costEntry)
	var selfCost, cumulativeCost, total int64

	for _, sample := range p.Sample {
		value := sample.Value[valueIndex]
		total += value
		for i, loc := range sample.Location {
			for _, line := range loc.Line {
				if line.Function == nil || line.Function.Name != targetName {
					continue
				}
				if targetFile == "" && line.Function.Filename != "" {
					targetFile = line.Function.Filename
				}
				cumulativeCost += value
				if i == 0 {
					selfCost += value
				}
				// Caller is at i+1 (closer to root in leaf-first stacks).
				if i+1 < len(sample.Location) {
					for _, callerLine := range sample.Location[i+1].Line {
						if callerLine.Function != nil {
							cn := callerLine.Function.Name
							e, ok := callerCosts[cn]
							if !ok {
								e = &costEntry{name: cn, file: callerLine.Function.Filename}
								callerCosts[cn] = e
							}
							e.cost += value
						}
					}
				}
				// Callee is at i-1 (closer to leaf).
				if i > 0 {
					for _, calleeLine := range sample.Location[i-1].Line {
						if calleeLine.Function != nil {
							cn := calleeLine.Function.Name
							e, ok := calleeCosts[cn]
							if !ok {
								e = &costEntry{name: cn, file: calleeLine.Function.Filename}
								calleeCosts[cn] = e
							}
							e.cost += value
						}
					}
				}
				break
			}
		}
	}

	funcInfo := &PeekFunctionInfo{
		Name:       targetName,
		File:       targetFile,
		Self:       selfCost,
		Cumulative: cumulativeCost,
	}

	makePeekEntries := func(costs map[string]*costEntry) []PeekEntry {
		var entries []PeekEntry
		for _, e := range costs {
			entries = append(entries, PeekEntry{
				Name: e.name,
				File: e.file,
				Cost: e.cost,
				Pct:  safePercent(e.cost, total),
			})
		}
		sort.Slice(entries, func(i, j int) bool {
			return absInt64(entries[i].Cost) > absInt64(entries[j].Cost)
		})
		if limit > 0 && len(entries) > limit {
			entries = entries[:limit]
		}
		return entries
	}

	return funcInfo, makePeekEntries(callerCosts), makePeekEntries(calleeCosts), nil
}

// trieNode is an internal node for building call trees.
type trieNode struct {
	name     string
	file     string
	self     int64
	cumulative int64
	children map[string]*trieNode
}

// Flamegraph builds a bounded call tree rooted at a function (or the profile root),
// pruned by maxDepth and minPct. Returns the tree, total profile value, count of
// pruned nodes, and any error.
func Flamegraph(p *profile.Profile, rootFunction string, valueIndex, maxDepth int, minPct float64) (*FlamegraphNode, int64, int, error) {
	if err := validateValueIndex(p, valueIndex); err != nil {
		return nil, 0, 0, err
	}

	newNode := func(name, file string) *trieNode {
		return &trieNode{name: name, file: file, children: make(map[string]*trieNode)}
	}

	root := newNode("(root)", "")
	var totalValue int64

	for _, sample := range p.Sample {
		value := sample.Value[valueIndex]
		totalValue += value

		// Reverse the stack: root first, leaf last.
		n := len(sample.Location)
		node := root
		for i := n - 1; i >= 0; i-- {
			loc := sample.Location[i]
			for _, line := range loc.Line {
				if line.Function == nil {
					continue
				}
				fname := line.Function.Name
				child, ok := node.children[fname]
				if !ok {
					child = newNode(fname, line.Function.Filename)
					node.children[fname] = child
				}
				child.cumulative += value
				if i == 0 { // leaf
					child.self += value
				}
				node = child
			}
		}
	}

	// If root_function is specified, find that subtree.
	var subtreeRoot *trieNode
	if rootFunction != "" {
		var err error
		subtreeRoot, err = findInTrie(root, rootFunction)
		if err != nil {
			return nil, 0, 0, err
		}
		if subtreeRoot == nil {
			return nil, 0, 0, fmt.Errorf("function %q not found in profile call tree", rootFunction)
		}
	} else {
		subtreeRoot = root
	}

	// Convert trie to FlamegraphNode with depth/pct pruning.
	pruned := 0
	var convert func(node *trieNode, depth int) FlamegraphNode
	convert = func(node *trieNode, depth int) FlamegraphNode {
		fn := FlamegraphNode{
			Name:       node.name,
			File:       node.file,
			Self:       node.self,
			Cumulative: node.cumulative,
			Pct:        safePercent(node.cumulative, totalValue),
		}
		if depth < maxDepth {
			for _, child := range sortedChildren(node) {
				childPct := safePercent(child.cumulative, totalValue)
				if math.Abs(childPct) < minPct {
					pruned++
					continue
				}
				childNode := convert(child, depth+1)
				fn.Children = append(fn.Children, childNode)
			}
		} else {
			pruned += len(node.children)
		}
		return fn
	}

	result := convert(subtreeRoot, 0)
	return &result, totalValue, pruned, nil
}

// CompareProfiles creates a diff profile (current - base) and returns comparison results.
func CompareProfiles(
	ctx context.Context,
	svc *cloudprofiler.Service,
	cache *ProfileCache,
	project, currentID, baseID string,
	valueIndex, topN int,
) (*ProfileCompareResult, *profile.Profile, error) {
	currentProfile, currentMeta, err := GetOrFetchProfile(ctx, svc, cache, project, currentID)
	if err != nil {
		return nil, nil, fmt.Errorf("fetching current profile: %w", err)
	}
	baseProfile, baseMeta, err := GetOrFetchProfile(ctx, svc, cache, project, baseID)
	if err != nil {
		return nil, nil, fmt.Errorf("fetching base profile: %w", err)
	}

	if err := validateValueIndex(currentProfile, valueIndex); err != nil {
		return nil, nil, err
	}
	if err := validateValueIndex(baseProfile, valueIndex); err != nil {
		return nil, nil, fmt.Errorf("base profile: %w", err)
	}

	// Validate that both profiles have compatible sample types at the given index.
	curVT := currentProfile.SampleType[valueIndex]
	baseVT := baseProfile.SampleType[valueIndex]
	if curVT.Type != baseVT.Type || curVT.Unit != baseVT.Unit {
		return nil, nil, fmt.Errorf("incompatible sample types at value_index %d: current has %s/%s but base has %s/%s",
			valueIndex, curVT.Type, curVT.Unit, baseVT.Type, baseVT.Unit)
	}

	currentTop, currentTotal, _, err := TopFunctions(currentProfile, valueIndex, 0, "cumulative", "")
	if err != nil {
		return nil, nil, fmt.Errorf("analyzing current profile: %w", err)
	}
	baseTop, baseTotal, _, err := TopFunctions(baseProfile, valueIndex, 0, "cumulative", "")
	if err != nil {
		return nil, nil, fmt.Errorf("analyzing base profile: %w", err)
	}

	// Build delta map.
	type deltaEntry struct {
		name  string
		file  string
		delta int64
	}
	deltas := make(map[string]*deltaEntry)
	for _, f := range currentTop {
		deltas[f.FunctionName] = &deltaEntry{name: f.FunctionName, file: f.File, delta: f.CumulativeValue}
	}
	for _, f := range baseTop {
		e, ok := deltas[f.FunctionName]
		if !ok {
			e = &deltaEntry{name: f.FunctionName, file: f.File}
			deltas[f.FunctionName] = e
		}
		e.delta -= f.CumulativeValue
	}

	var regressions, improvements []CompareTopEntry
	var functionsRegressed, functionsImproved int
	for _, e := range deltas {
		pct := safePercent(e.delta, baseTotal)
		entry := CompareTopEntry{Name: e.name, File: e.file, Delta: e.delta, DeltaPct: pct}
		if e.delta > 0 {
			regressions = append(regressions, entry)
			functionsRegressed++
		} else if e.delta < 0 {
			improvements = append(improvements, entry)
			functionsImproved++
		}
	}

	sort.Slice(regressions, func(i, j int) bool { return regressions[i].Delta > regressions[j].Delta })
	sort.Slice(improvements, func(i, j int) bool { return improvements[i].Delta < improvements[j].Delta })

	truncated := false
	if topN > 0 {
		if len(regressions) > topN {
			regressions = regressions[:topN]
			truncated = true
		}
		if len(improvements) > topN {
			improvements = improvements[:topN]
			truncated = true
		}
	}

	totalDelta := currentTotal - baseTotal
	result := &ProfileCompareResult{
		CurrentMeta: currentMeta,
		BaseMeta:    baseMeta,
		ValueType:   ProfileValueTypes(currentProfile)[valueIndex],
		Summary: CompareSummary{
			TotalCurrent:       currentTotal,
			TotalBase:          baseTotal,
			TotalDelta:         totalDelta,
			TotalDeltaPct:      safePercent(totalDelta, baseTotal),
			FunctionsRegressed: functionsRegressed,
			FunctionsImproved:  functionsImproved,
		},
		TopRegressions:  regressions,
		TopImprovements: improvements,
		Truncated:       truncated,
	}
	if truncated {
		result.TruncationHint = fmt.Sprintf("Showing top %d regressions and improvements. Use profiler.top with the diff_id for full function ranking.", topN)
	}

	// Build a diff profile for use with top/peek/flamegraph.
	diffProfile, err := buildDiffProfile(currentProfile, baseProfile)
	if err != nil {
		// Return comparison stats (still useful) but no diff profile.
		result.Warning = fmt.Sprintf("Comparison stats are valid but diff profile could not be built: %v. Use profiler.top on each profile individually to drill down.", err)
		return result, nil, nil
	}

	result.DiffID = fmt.Sprintf("diff:%s:%s", currentID, baseID)
	return result, diffProfile, nil
}

// buildDiffProfile creates a diff profile by combining current and negated base samples.
// Both profiles are copied first: base is negated in-place before merging, and both
// originals may come from the shared cache, so we must not modify them.
func buildDiffProfile(current, base *profile.Profile) (*profile.Profile, error) {
	currentCopy := current.Copy()
	baseCopy := base.Copy()
	for _, s := range baseCopy.Sample {
		for i := range s.Value {
			s.Value[i] = -s.Value[i]
		}
	}

	merged, err := profile.Merge([]*profile.Profile{currentCopy, baseCopy})
	if err != nil {
		return nil, fmt.Errorf("merging profiles: %w", err)
	}

	return merged, nil
}

// prefetchResult summarises the outcome of a bulk prefetch so the caller
// can emit a diagnostic warning when the optimisation fails.
type prefetchResult struct {
	Cached int
	Errors int
	Last   error
}

// prefetchProfiles does a single paginated scan of the List API with profileBytes
// included in the response, parsing and caching each profile that appears in the
// wanted set. This avoids O(n) individual List scans when ComputeTrends needs many
// profiles that were already discovered via a metadata-only ListProfiles call.
func prefetchProfiles(
	ctx context.Context,
	svc *cloudprofiler.Service,
	cache *ProfileCache,
	project string,
	wanted map[string]bool,
) prefetchResult {
	const maxPages = 20
	var res prefetchResult
	remaining := len(wanted)
	pageToken := ""
	for range maxPages {
		if remaining <= 0 || ctx.Err() != nil {
			return res
		}
		call := svc.Projects.Profiles.List("projects/"+project).
			PageSize(1000).
			Fields("profiles(name,profileType,deployment,startTime,duration,labels,profileBytes)", "nextPageToken").
			Context(ctx)
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}
		resp, err := call.Do()
		if err != nil {
			res.Errors++
			res.Last = fmt.Errorf("list API call failed: %w", err)
			return res
		}
		for _, p := range resp.Profiles {
			if !wanted[p.Name] {
				continue
			}
			key := project + "/" + p.Name
			if _, _, ok := cache.Get(key); ok {
				remaining--
				res.Cached++
				continue
			}
			if p.ProfileBytes == "" {
				res.Errors++
				res.Last = fmt.Errorf("profile %s: empty profile bytes", p.Name)
				continue
			}
			data, err := base64.StdEncoding.DecodeString(p.ProfileBytes)
			if err != nil {
				res.Errors++
				res.Last = fmt.Errorf("profile %s: base64 decode: %w", p.Name, err)
				continue
			}
			parsed, err := profile.Parse(bytes.NewReader(data))
			if err != nil {
				res.Errors++
				res.Last = fmt.Errorf("profile %s: pprof parse: %w", p.Name, err)
				continue
			}
			cache.Put(key, parsed, profileFromAPI(p))
			remaining--
			res.Cached++
		}
		if resp.NextPageToken == "" {
			return res
		}
		pageToken = resp.NextPageToken
	}
	return res
}

// ProfileValueTypes returns available value types for a profile.
func ProfileValueTypes(p *profile.Profile) []ValueTypeInfo {
	var types []ValueTypeInfo
	for i, vt := range p.SampleType {
		types = append(types, ValueTypeInfo{Index: i, Type: vt.Type, Unit: vt.Unit})
	}
	return types
}

// ComputeTrends tracks how specific functions' costs change over time across
// multiple profiles — the same data shown in Cloud Profiler UI's "Profile history".
//
// functionFilter is a substring match on function name. When set, only matching
// functions are tracked (cheap: single pass over samples per profile). When empty,
// the first successfully-downloaded profile is used to discover the top maxFunctions
// functions, which are then tracked across all profiles.
func ComputeTrends(
	ctx context.Context,
	svc *cloudprofiler.Service,
	cache *ProfileCache,
	project, profileType, target, functionFilter string,
	valueIndex, maxProfiles, maxFunctions int,
	progressFn func(current, total int, msg string),
) (*ProfileTrendsResult, error) {
	// Overall timeout to bound the total wall time when downloading many profiles.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	profiles, err := ListProfiles(ctx, svc, project, profileType, target, "", "", maxProfiles, "")
	if err != nil {
		return nil, fmt.Errorf("listing profiles: %w", err)
	}
	if profiles.Count == 0 {
		return nil, fmt.Errorf("no profiles found for type=%q target=%q in project %q", profileType, target, project)
	}

	// Chronological order (oldest first).
	sort.Slice(profiles.Profiles, func(i, j int) bool {
		return profiles.Profiles[i].StartTime < profiles.Profiles[j].StartTime
	})

	// Pre-fetch all profiles in a single paginated scan to avoid O(n) individual
	// List API calls. This is best-effort; individual GetOrFetchProfile calls
	// below will retry on cache misses.
	wanted := make(map[string]bool, len(profiles.Profiles))
	for _, meta := range profiles.Profiles {
		wanted[meta.ProfileID] = true
	}
	pf := prefetchProfiles(ctx, svc, cache, project, wanted)

	// If no function_filter, discover target functions from the first profile.
	targetFunctions := map[string]bool{} // empty = track all matching functionFilter
	if functionFilter == "" {
		var discovered bool
		var lastErr error
		for _, meta := range profiles.Profiles {
			p, _, err := GetOrFetchProfile(ctx, svc, cache, project, meta.ProfileID)
			if err != nil {
				lastErr = err
				continue
			}
			topFuncs, _, _, err := TopFunctions(p, valueIndex, maxFunctions, "cumulative", "")
			if err != nil {
				// Analysis errors (e.g. invalid valueIndex) are deterministic —
				// retrying with another profile won't help.
				return nil, fmt.Errorf("analyzing profile for function discovery: %w", err)
			}
			if len(topFuncs) == 0 {
				continue // empty profile, try the next one
			}
			for _, f := range topFuncs {
				targetFunctions[f.FunctionName] = true
			}
			discovered = true
			break
		}
		if !discovered {
			return nil, fmt.Errorf("failed to discover top functions: could not download or analyze any profile (last error: %v)", lastErr)
		}
	}

	type funcInfo struct {
		file string
		peak int64
	}
	funcTimeline := make(map[string][]TrendsDataPoint)
	funcMeta := make(map[string]*funcInfo)
	var resolvedValueType *ValueTypeInfo
	var downloadErrors int
	var lastDownloadErr error

	total := len(profiles.Profiles)
	for i, meta := range profiles.Profiles {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("cancelled after %d/%d profiles: %w", i, total, ctx.Err())
		}
		if progressFn != nil {
			progressFn(i, total, fmt.Sprintf("Analyzing profile %d/%d...", i+1, total))
		}

		p, _, err := GetOrFetchProfile(ctx, svc, cache, project, meta.ProfileID)
		if err != nil {
			downloadErrors++
			lastDownloadErr = err
			continue
		}

		if resolvedValueType == nil {
			vts := ProfileValueTypes(p)
			if valueIndex < len(vts) {
				resolvedValueType = &vts[valueIndex]
			}
		}

		costs, profileTotal, err := scanFunctionCosts(p, valueIndex, functionFilter, targetFunctions)
		if err != nil {
			return nil, fmt.Errorf("analyzing profile %s: %w", meta.ProfileID, err)
		}

		for fname, c := range costs {
			selfPct := safePercent(c.self, profileTotal)
			cumPct := safePercent(c.cumulative, profileTotal)
			dp := TrendsDataPoint{
				ProfileID: meta.ProfileID,
				Timestamp: meta.StartTime,
				SelfValue: c.self,
				SelfPct:   selfPct,
				CumulativeValue: c.cumulative,
				CumulativePct:   cumPct,
			}
			funcTimeline[fname] = append(funcTimeline[fname], dp)

			fi, ok := funcMeta[fname]
			if !ok {
				fi = &funcInfo{file: c.file}
				funcMeta[fname] = fi
			}
			if absInt64(c.cumulative) > fi.peak {
				fi.peak = absInt64(c.cumulative)
			}
		}
	}

	if len(funcMeta) == 0 && downloadErrors > 0 {
		return nil, fmt.Errorf("failed to analyze any profiles: %d/%d downloads failed (last error: %v)", downloadErrors, total, lastDownloadErr)
	}

	// Rank by peak cumulative, take top N.
	type rankedFunc struct {
		name string
		peak int64
	}
	var ranked []rankedFunc
	for name, fi := range funcMeta {
		ranked = append(ranked, rankedFunc{name: name, peak: fi.peak})
	}
	sort.Slice(ranked, func(i, j int) bool { return ranked[i].peak > ranked[j].peak })

	truncated := false
	if maxFunctions > 0 && len(ranked) > maxFunctions {
		ranked = ranked[:maxFunctions]
		truncated = true
	}

	var functions []TrendsFunctionSeries
	for _, r := range ranked {
		functions = append(functions, TrendsFunctionSeries{
			Name:       r.name,
			File:       funcMeta[r.name].file,
			DataPoints: funcTimeline[r.name],
		})
	}

	var timeRangeStart, timeRangeEnd string
	if len(profiles.Profiles) > 0 {
		timeRangeStart = profiles.Profiles[0].StartTime
		timeRangeEnd = profiles.Profiles[len(profiles.Profiles)-1].StartTime
	}

	vt := ValueTypeInfo{Index: valueIndex}
	if resolvedValueType != nil {
		vt = *resolvedValueType
	}

	analyzed := total - downloadErrors
	result := &ProfileTrendsResult{
		Target:         target,
		ProfileType:    profileType,
		ValueType:      vt,
		ProfileCount:   total,
		AnalyzedCount:  analyzed,
		TimeRangeStart: timeRangeStart,
		TimeRangeEnd:   timeRangeEnd,
		Functions:      functions,
		DownloadErrors: downloadErrors,
		Truncated:      truncated,
	}
	if lastDownloadErr != nil {
		result.LastDownloadError = lastDownloadErr.Error()
	}
	if pf.Errors > 0 && downloadErrors > 0 {
		result.Warning = fmt.Sprintf("Bulk prefetch failed (%d errors, last: %v), fell back to individual fetches. %d/%d profiles analyzed successfully.", pf.Errors, pf.Last, analyzed, total)
	} else if pf.Errors > 0 {
		result.Warning = fmt.Sprintf("Bulk prefetch encountered %d errors (last: %v) but all profiles were fetched individually.", pf.Errors, pf.Last)
	} else if downloadErrors > 0 {
		result.Warning = fmt.Sprintf("Only %d/%d profiles analyzed successfully; trend data may be incomplete.", analyzed, total)
	}
	if truncated {
		result.TruncationHint = fmt.Sprintf("Showing top %d of %d functions. Use function_filter to narrow results.", maxFunctions, len(funcMeta))
	}

	return result, nil
}

// scanFunctionCosts does a single pass over profile samples, computing self and
// cumulative costs only for functions that match the filter or target set. Uses
// less memory and fewer map operations than TopFunctions when tracking a small
// subset of functions, since non-matching functions are skipped during accumulation.
func scanFunctionCosts(p *profile.Profile, valueIndex int, filter string, targets map[string]bool) (map[string]*funcCost, int64, error) {
	if err := validateValueIndex(p, valueIndex); err != nil {
		return nil, 0, err
	}
	costs := make(map[string]*funcCost)
	var total int64

	useTargets := len(targets) > 0
	for _, sample := range p.Sample {
		value := sample.Value[valueIndex]
		total += value
		seen := make(map[string]bool)
		for i, loc := range sample.Location {
			for _, line := range loc.Line {
				if line.Function == nil {
					continue
				}
				fname := line.Function.Name

				// Skip functions we don't care about.
				if useTargets {
					if !targets[fname] {
						continue
					}
				} else if filter != "" {
					if !strings.Contains(fname, filter) {
						continue
					}
				}

				c, ok := costs[fname]
				if !ok {
					c = &funcCost{file: line.Function.Filename}
					costs[fname] = c
				}
				if i == 0 {
					c.self += value
				}
				if !seen[fname] {
					c.cumulative += value
					seen[fname] = true
				}
			}
		}
	}
	return costs, total, nil
}

type funcCost struct {
	file       string
	self       int64
	cumulative int64
}

func profileFromAPI(p *cloudprofiler.Profile) ProfileMeta {
	meta := ProfileMeta{
		ProfileID:   p.Name,
		ProfileType: p.ProfileType,
		Duration:    p.Duration,
		StartTime:   p.StartTime,
	}
	if p.Deployment != nil {
		meta.Target = p.Deployment.Target
		meta.DeploymentLabels = p.Deployment.Labels
	}
	if len(p.Labels) > 0 {
		meta.Labels = p.Labels
	}
	return meta
}

func validateValueIndex(p *profile.Profile, index int) error {
	if len(p.SampleType) == 0 {
		return fmt.Errorf("profile has no sample types")
	}
	if index < 0 || index >= len(p.SampleType) {
		types := make([]string, len(p.SampleType))
		for i, vt := range p.SampleType {
			types[i] = fmt.Sprintf("%d: %s/%s", i, vt.Type, vt.Unit)
		}
		return fmt.Errorf("value_index %d out of range [0, %d). Available: %s", index, len(p.SampleType), strings.Join(types, ", "))
	}
	return nil
}

func findMatchingFunctions(p *profile.Profile, substring string) []string {
	seen := make(map[string]bool)
	var matches []string
	for _, loc := range p.Location {
		for _, line := range loc.Line {
			if line.Function != nil && strings.Contains(line.Function.Name, substring) {
				if !seen[line.Function.Name] {
					seen[line.Function.Name] = true
					matches = append(matches, line.Function.Name)
				}
			}
		}
	}
	sort.Strings(matches)
	return matches
}

func findInTrie(root *trieNode, name string) (*trieNode, error) {
	// BFS to find nodes matching name (substring match).
	// Uses sortedChildren for deterministic traversal order.
	var matches []*trieNode
	queue := []*trieNode{root}
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		if strings.Contains(node.name, name) && node != root {
			matches = append(matches, node)
		}
		for _, child := range sortedChildren(node) {
			queue = append(queue, child)
		}
	}
	switch len(matches) {
	case 0:
		return nil, nil
	case 1:
		return matches[0], nil
	default:
		names := make([]string, 0, len(matches))
		for _, m := range matches {
			if len(names) >= 10 {
				break
			}
			names = append(names, m.name)
		}
		hint := strings.Join(names, ", ")
		if len(matches) > 10 {
			hint += fmt.Sprintf(" ... and %d more", len(matches)-10)
		}
		return nil, fmt.Errorf("root_function %q matches %d nodes; use a more specific name. Candidates: %s", name, len(matches), hint)
	}
}

func sortedChildren(node *trieNode) []*trieNode {
	children := make([]*trieNode, 0, len(node.children))
	for _, child := range node.children {
		children = append(children, child)
	}
	sort.Slice(children, func(i, j int) bool {
		return absInt64(children[i].cumulative) > absInt64(children[j].cumulative)
	})
	return children
}

func safePercent(value, total int64) float64 {
	if total == 0 {
		return 0
	}
	return math.Round(float64(value)/float64(total)*10000) / 100
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
