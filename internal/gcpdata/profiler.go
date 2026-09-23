package gcpdata

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/cloudprofiler/apiv2/cloudprofilerpb"
	"github.com/google/pprof/profile"
	"google.golang.org/api/iterator"
)

// ProfilerScanTimeout is the time budget of one profiler_* tool call, which
// the server applies to every such call once it holds a profiler slot. The
// Cloud Profiler Export API returns profile bytes inline and offers no
// server-side filter (see ListProfiles), so a call must page through and
// download many profiles client-side and can legitimately run for minutes on
// large projects. The tool layer keeps the MCP client's request alive across
// this window with progress heartbeats, and maxScan still bounds the total
// work examined.
const ProfilerScanTimeout = 8 * time.Minute

// ErrProfilerScanBudget is the cause the server gives a profiler tool call's
// context when ProfilerScanTimeout runs out. Scans report it in their error
// (see scanError). It wraps context.DeadlineExceeded.
var ErrProfilerScanBudget = fmt.Errorf("profiler scan used up its %s time budget: %w", ProfilerScanTimeout, context.DeadlineExceeded)

// scanError returns err from a profiler scan prefixed by the cause of ctx's
// end when the caller gave one (such as ErrProfilerScanBudget), so callers
// see why the scan stopped rather than a bare deadline error.
func scanError(ctx context.Context, err error) error {
	if cause := context.Cause(ctx); cause != nil && !errors.Is(ctx.Err(), cause) {
		return fmt.Errorf("%w: %w", cause, err)
	}
	return err
}

const (
	maxCompressedProfileBytes   = 16 << 20
	maxDecompressedProfileBytes = 64 << 20
)

// ProfileTypes lists the profile types supported by Cloud Profiler.
var ProfileTypes = []string{"CPU", "WALL", "HEAP", "THREADS", "CONTENTION", "PEAK_HEAP", "HEAP_ALLOC"}

// ListProfilesParams bundles the filter and paging arguments for ListProfiles.
// StartTime/EndTime are time.Time (zero = no bound), matching TraceQuerier's
// ListTraces, so RFC3339 parsing and its error message stay at the tool boundary.
type ListProfilesParams struct {
	Project     string
	ProfileType string
	Target      string
	StartTime   time.Time
	EndTime     time.Time
	PageSize    int
	PageToken   string
}

type profileCursor struct {
	Version     int    `json:"v"`
	APIToken    string `json:"p,omitempty"`
	Offset      int    `json:"o,omitempty"`
	Fingerprint string `json:"f"`
}

const (
	profileCursorPrefix  = "mcp_pc_v2_"
	profileCursorVersion = 2
)

func encodeProfileCursor(c profileCursor) (string, error) {
	b, err := json.Marshal(c) // #nosec G117 -- APIToken is an opaque pagination position, not a credential.
	if err != nil {
		return "", fmt.Errorf("encoding profiler cursor: %w", err)
	}
	return profileCursorPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

func decodeProfileCursor(raw, fingerprint string) (profileCursor, error) {
	if raw == "" {
		return profileCursor{Version: profileCursorVersion, Fingerprint: fingerprint}, nil
	}
	raw, ok := strings.CutPrefix(raw, profileCursorPrefix)
	if !ok {
		return profileCursor{}, fmt.Errorf("invalid profiler cursor")
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return profileCursor{}, fmt.Errorf("invalid profiler cursor")
	}
	var c profileCursor
	if err := json.Unmarshal(b, &c); err != nil || c.Version != profileCursorVersion || c.Offset < 0 || c.Fingerprint != fingerprint {
		return profileCursor{}, fmt.Errorf("invalid profiler cursor or cursor/filter mismatch")
	}
	return c, nil
}

// ListProfiles lists profile metadata without downloading profile bytes. The
// Cloud Profiler API does not support server-side filtering by profile_type,
// target, or time range, so filtering is applied client-side: ListProfiles
// advances through the Export API page by page until PageSize matches are
// found or the scan limit is reached. Its opaque cursor can resume inside an
// API page and is bound to the exact filter set.
func (q *CloudProfilerQuerier) ListProfiles(ctx context.Context, params ListProfilesParams) (*ProfileListResult, error) {
	filter := newProfileFilter(params)
	fingerprint := filter.fingerprint(params.Project)
	cursor, err := decodeProfileCursor(params.PageToken, fingerprint)
	if err != nil {
		return nil, err
	}
	result := &ProfileListResult{Summary: ProfileSummary{CountByType: map[string]int{}, CountByTarget: map[string]int{}}}
	targetsSeen := map[string]int{}
	const maxScan = 20_000
	pageStart := cursor.APIToken
	offset := cursor.Offset
	resumeToken, resumeOffset := pageStart, offset
	scanned := 0
	for scanned < maxScan {
		it := q.svc.ListProfiles(ctx, &cloudprofilerpb.ListProfilesRequest{Parent: "projects/" + params.Project, PageSize: 1000, PageToken: pageStart})
		pager := iterator.NewPager(it, 1000, pageStart)
		var page []*cloudprofilerpb.Profile
		nextToken, pageErr := pager.NextPage(&page)
		if pageErr != nil {
			return nil, scanError(ctx, fmt.Errorf("listing profiles: %w", pageErr))
		}
		if offset > len(page) {
			return nil, fmt.Errorf("invalid profiler cursor offset")
		}
		for i := offset; i < len(page) && scanned < maxScan; i++ {
			scanned++
			if i+1 < len(page) {
				resumeToken, resumeOffset = pageStart, i+1
			} else {
				resumeToken, resumeOffset = nextToken, 0
			}
			meta := profileFromAPI(page[i])
			if meta.Target != "" {
				targetsSeen[meta.Target]++
			}
			match, parseErr := filter.match(meta)
			if parseErr {
				result.ExcludedCount++
			}
			if !match {
				continue
			}
			result.Profiles = append(result.Profiles, meta)
			result.Summary.CountByType[meta.ProfileType]++
			result.Summary.CountByTarget[meta.Target]++
			if len(result.Profiles) == params.PageSize {
				next := profileCursor{Version: profileCursorVersion, Fingerprint: fingerprint}
				if i+1 < len(page) {
					next.APIToken, next.Offset = pageStart, i+1
				} else if nextToken != "" {
					next.APIToken = nextToken
				} else {
					result.Count = len(result.Profiles)
					result.Truncated = false
					return result, nil
				}
				result.NextPageToken, err = encodeProfileCursor(next)
				if err != nil {
					return nil, err
				}
				result.Count = len(result.Profiles)
				result.Truncated = true
				return result, nil
			}
		}
		if nextToken == "" {
			break
		}
		pageStart, offset = nextToken, 0
	}
	result.Count = len(result.Profiles)
	result.Truncated = scanned >= maxScan && (resumeToken != "" || resumeOffset != 0)
	if result.Truncated {
		result.NextPageToken, err = encodeProfileCursor(profileCursor{Version: profileCursorVersion, APIToken: resumeToken, Offset: resumeOffset, Fingerprint: fingerprint})
		if err != nil {
			return nil, err
		}
		result.Warning = fmt.Sprintf("Scan stopped at the %d-profile safety limit; continue with next_page_token.", maxScan)
	} else if params.Target != "" && result.Count == 0 {
		result.Warning = fmt.Sprintf("No profiles matched target %q%s.", params.Target, availableTargetsHint(targetsSeen))
	}
	return result, nil
}

// availableTargetsHint renders a short ", available targets: a, b, c" clause
// from the distinct targets seen during a scan, ordered by frequency then name
// and capped so the message stays readable. It returns "" when no targets were
// seen (nothing useful to suggest). The leading comma lets callers splice it
// directly into a sentence.
func availableTargetsHint(seen map[string]int) string {
	if len(seen) == 0 {
		return ""
	}
	const maxList = 20
	names := topNBy(seen, maxList, func(name string, _ int) string { return name })
	hint := ", available targets: " + strings.Join(names, ", ")
	if len(seen) > maxList {
		hint += fmt.Sprintf(" (+%d more)", len(seen)-maxList)
	}
	return hint
}

// normalizeIdent lowercases s and strips every non-alphanumeric rune. Service
// names that differ only in separators or case then compare equal — e.g.
// "crypto-steam", "Crypto_Steam" and "cryptosteam" all normalize to
// "cryptosteam". Used for fuzzy target matching, since the Cloud Profiler Export
// API exposes no server-side target filter (ListProfilesRequest has only
// parent/page_size/page_token), so filtering is client-side and users rarely
// type the deployment target string with the exact separators.
func normalizeIdent(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// profileFilter is the client-side filter ListProfiles applies to every scanned
// profile. The target is normalized once per call rather than per profile.
type profileFilter struct {
	profileType string
	target      string // normalizeIdent of the requested target; "" matches all
	start, end  time.Time
}

func newProfileFilter(params ListProfilesParams) profileFilter {
	return profileFilter{
		profileType: params.ProfileType,
		target:      normalizeIdent(params.Target),
		start:       params.StartTime,
		end:         params.EndTime,
	}
}

// fingerprint identifies the filter set within project, binding a pagination
// cursor to the query that produced it.
func (f profileFilter) fingerprint(project string) string {
	payload := strings.Join([]string{project, f.profileType, f.target, f.start.UTC().Format(time.RFC3339Nano), f.end.UTC().Format(time.RFC3339Nano)}, "\x00")
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:16])
}

// match returns whether the profile matches all filters. The target compares
// case- and separator-insensitively and as a substring, so partial service
// names ("steam") still discover profiles. parseErr is true when the profile
// was excluded due to an unparseable timestamp (so the caller can track how
// many were excluded for user feedback).
func (f profileFilter) match(meta ProfileMeta) (match, parseErr bool) {
	if f.profileType != "" && meta.ProfileType != f.profileType {
		return false, false
	}
	if f.target != "" && !strings.Contains(normalizeIdent(meta.Target), f.target) {
		return false, false
	}
	hasTimeFilter := !f.start.IsZero() || !f.end.IsZero()
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
		if !f.start.IsZero() && mt.Before(f.start) {
			return false, false
		}
		if !f.end.IsZero() && mt.After(f.end) {
			return false, false
		}
	}
	return true, false
}

// GetOrFetchProfile retrieves a parsed profile from cache or downloads it.
// Cloud Profiler API v2 has no direct GET endpoint; profiles are found by
// scanning list results. Prefetched bytes are parsed on first use here and
// dropped from the cache when they fail; downloaded bytes are cached only
// after they parse.
func (q *CloudProfilerQuerier) GetOrFetchProfile(
	ctx context.Context,
	project, profileName string,
) (*profile.Profile, ProfileMeta, error) {
	return q.loadProfile(ctx, project, profileName, true)
}

// loadProfile is GetOrFetchProfile; cacheFetched says whether a downloaded
// profile is cached. ComputeTrends passes false so that its downloads cannot
// evict prefetched profiles it has yet to analyze.
func (q *CloudProfilerQuerier) loadProfile(
	ctx context.Context,
	project, profileName string,
	cacheFetched bool,
) (*profile.Profile, ProfileMeta, error) {
	key := profileCacheKey(project, profileName)
	if data, meta, ok := q.cache.Get(key); ok {
		p, err := parseSourceProfile(data)
		if err != nil {
			q.cache.Delete(key)
			return nil, ProfileMeta{}, fmt.Errorf("decoding cached profile %q: %w", profileName, err)
		}
		return p, meta, nil
	}

	// The profile name from the API is "projects/{project}/profiles/{id}".
	// If the user provides just the ID, construct the full resource name.
	resourceName := profileName
	if !strings.HasPrefix(profileName, "projects/") {
		resourceName = "projects/" + project + "/profiles/" + profileName
	}

	// Scan up to maxScan profiles to find the one matching by name.
	const maxScan = 10_000
	var found *cloudprofilerpb.Profile
	var foundMeta ProfileMeta
	scanned := 0
	exhausted := false

	it := q.svc.ListProfiles(ctx, &cloudprofilerpb.ListProfilesRequest{
		Parent:   "projects/" + project,
		PageSize: 1000,
	})
	for scanned < maxScan {
		p, err := it.Next()
		if errors.Is(err, iterator.Done) {
			exhausted = true
			break
		}
		if err != nil {
			return nil, ProfileMeta{}, scanError(ctx, fmt.Errorf("scanning profiles after %d: %w", scanned, err))
		}
		scanned++
		// Match by resource name (when the API populates it) or by the ID we
		// surface in profiler_list — which is p.Name when non-empty, otherwise
		// the synthetic ID from profileFromAPI. Computing the meta here lets
		// synthetic IDs round-trip and avoids recomputing it once found.
		meta := profileFromAPI(p)
		if p.Name == resourceName || p.Name == profileName || meta.ProfileID == profileName {
			found = p
			foundMeta = meta
			break
		}
	}

	if found == nil {
		if exhausted {
			return nil, ProfileMeta{}, fmt.Errorf("profile %q not found in project %q (scanned all %d available profiles)", profileName, project, scanned)
		}
		return nil, ProfileMeta{}, fmt.Errorf("profile %q not found in project %q within the first %d profiles scanned; the profile may exist further back — try a more recent profile from profiler_list results", profileName, project, maxScan)
	}

	if len(found.ProfileBytes) == 0 {
		return nil, ProfileMeta{}, fmt.Errorf("profile %q has no profile bytes", profileName)
	}

	p, err := parseSourceProfile(found.ProfileBytes)
	if err != nil {
		return nil, ProfileMeta{}, fmt.Errorf("decoding profile %q: %w", profileName, err)
	}

	if !cacheFetched {
		return p, foundMeta, nil
	}
	if err := q.cache.Put(key, found.ProfileBytes, foundMeta, nil); err != nil {
		q.logger.Warn("profile_not_cached", "project", project, "profile", foundMeta.ProfileID, "reason", err.Error())
	}
	return p, foundMeta, nil
}

func parseSourceProfile(data []byte) (*profile.Profile, error) {
	if len(data) > maxCompressedProfileBytes {
		return nil, fmt.Errorf("compressed profile is %d bytes; maximum is %d", len(data), maxCompressedProfileBytes)
	}
	var reader io.Reader = bytes.NewReader(data)
	if len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b {
		gz, err := gzip.NewReader(reader)
		if err != nil {
			return nil, fmt.Errorf("opening compressed profile: %w", err)
		}
		defer func() { _ = gz.Close() }()
		reader = gz
	}
	raw, err := io.ReadAll(io.LimitReader(reader, maxDecompressedProfileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading profile data: %w", err)
	}
	if len(raw) > maxDecompressedProfileBytes {
		return nil, fmt.Errorf("decompressed profile exceeds %d bytes", maxDecompressedProfileBytes)
	}
	p, err := profile.ParseData(raw)
	if err != nil {
		return nil, fmt.Errorf("parsing profile data: %w", err)
	}
	return p, nil
}

// TopFunctions computes a flat ranking of functions by self or cumulative cost.
// filter is a substring matched against the function name or file.
// Returns the ranked functions, the total value, whether results were truncated, and any error.
func TopFunctions(p *profile.Profile, valueIndex, limit int, sortBy, filter string) ([]TopFunction, int64, bool, error) {
	var rank func(TopFunction) int64
	switch sortBy {
	case "cumulative":
		rank = func(f TopFunction) int64 { return absInt64(f.CumulativeValue) }
	case "self":
		rank = func(f TopFunction) int64 { return absInt64(f.SelfValue) }
	default:
		return nil, 0, false, fmt.Errorf("invalid sort_by %q: must be \"self\" or \"cumulative\"", sortBy)
	}
	costs, total, err := scanFunctionCosts(p, valueIndex, "", nil)
	if err != nil {
		return nil, 0, false, err
	}

	var result []TopFunction
	for name, c := range costs {
		if filter != "" && !strings.Contains(name, filter) && !strings.Contains(c.file, filter) {
			continue
		}
		result = append(result, TopFunction{
			FunctionName:    name,
			File:            c.file,
			SelfValue:       c.self,
			SelfPct:         safePercent(c.self, total),
			CumulativeValue: c.cumulative,
			CumulativePct:   safePercent(c.cumulative, total),
		})
	}

	slices.SortFunc(result, func(a, b TopFunction) int {
		return cmp.Or(cmp.Compare(rank(b), rank(a)), cmp.Compare(a.FunctionName, b.FunctionName))
	})

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
	name       string
	file       string
	self       int64
	cumulative int64
	children   map[string]*trieNode
}

// Flamegraph builds a bounded call tree rooted at a function (or the profile root),
// pruned by maxDepth, minPct and a budget of maxNodes returned nodes (the root
// included; children are kept depth-first in descending cost order). Returns the
// tree, total profile value, count of pruned children, and any error.
func Flamegraph(p *profile.Profile, rootFunction string, valueIndex, maxDepth, maxNodes int, minPct float64) (*FlamegraphNode, int64, int, error) {
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

	// Convert trie to FlamegraphNode with depth/pct/node-budget pruning.
	pruned := 0
	remaining := maxNodes - 1 // the subtree root is always returned
	var convert func(node *trieNode, depth int) FlamegraphNode
	convert = func(node *trieNode, depth int) FlamegraphNode {
		fn := FlamegraphNode{
			Name:       node.name,
			File:       node.file,
			Self:       node.self,
			Cumulative: node.cumulative,
			Pct:        safePercent(node.cumulative, totalValue),
		}
		if depth >= maxDepth {
			pruned += len(node.children)
			return fn
		}
		for _, child := range sortedChildren(node) {
			if remaining <= 0 || math.Abs(safePercent(child.cumulative, totalValue)) < minPct {
				pruned++
				continue
			}
			remaining--
			fn.Children = append(fn.Children, convert(child, depth+1))
		}
		return fn
	}

	result := convert(subtreeRoot, 0)
	return &result, totalValue, pruned, nil
}

// CompareProfiles ranks per-function cumulative deltas (current - base).
func (q *CloudProfilerQuerier) CompareProfiles(
	ctx context.Context,
	project, currentID, baseID string,
	valueIndex, topN int,
) (*ProfileCompareResult, error) {
	currentProfile, baseProfile, currentMeta, baseMeta, err := q.fetchProfilePair(ctx, project, currentID, baseID)
	if err != nil {
		return nil, err
	}

	if err := validateValueIndex(currentProfile, valueIndex); err != nil {
		return nil, err
	}
	if err := validateValueIndex(baseProfile, valueIndex); err != nil {
		return nil, fmt.Errorf("base profile: %w", err)
	}

	// Validate that both profiles have compatible sample types at the given index.
	curVT := currentProfile.SampleType[valueIndex]
	baseVT := baseProfile.SampleType[valueIndex]
	if curVT.Type != baseVT.Type || curVT.Unit != baseVT.Unit {
		return nil, fmt.Errorf("incompatible sample types at value_index %d: current has %s/%s but base has %s/%s",
			valueIndex, curVT.Type, curVT.Unit, baseVT.Type, baseVT.Unit)
	}

	currentCosts, currentTotal, err := scanFunctionCosts(currentProfile, valueIndex, "", nil)
	if err != nil {
		return nil, fmt.Errorf("analyzing current profile: %w", err)
	}
	baseCosts, baseTotal, err := scanFunctionCosts(baseProfile, valueIndex, "", nil)
	if err != nil {
		return nil, fmt.Errorf("analyzing base profile: %w", err)
	}

	// Build delta map.
	type deltaEntry struct {
		name  string
		file  string
		delta int64
	}
	deltas := make(map[string]*deltaEntry)
	for name, c := range currentCosts {
		deltas[name] = &deltaEntry{name: name, file: c.file, delta: c.cumulative}
	}
	for name, c := range baseCosts {
		e, ok := deltas[name]
		if !ok {
			e = &deltaEntry{name: name, file: c.file}
			deltas[name] = e
		}
		e.delta -= c.cumulative
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
		result.TruncationHint = fmt.Sprintf("Showing top %d regressions and improvements. Pass both profile_id and base_profile_id to profiler_top for the complete diff ranking.", topN)
	}
	return result, nil
}

// GetProfileOrDiff computes a request-local diff when baseID is present.
func (q *CloudProfilerQuerier) GetProfileOrDiff(ctx context.Context, project, profileID, baseID string) (*profile.Profile, ProfileMeta, error) {
	if baseID == "" {
		return q.GetOrFetchProfile(ctx, project, profileID)
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

// fetchProfilePair fetches the current profile, then the base profile. The
// fetches stay sequential because each uncached one is a paginated Export API
// scan, and the server's profiler concurrency limit counts one scan per call.
func (q *CloudProfilerQuerier) fetchProfilePair(
	ctx context.Context,
	project, currentID, baseID string,
) (current, base *profile.Profile, currentMeta, baseMeta ProfileMeta, err error) {
	current, currentMeta, err = q.GetOrFetchProfile(ctx, project, currentID)
	if err != nil {
		return nil, nil, ProfileMeta{}, ProfileMeta{}, fmt.Errorf("fetching current profile: %w", err)
	}
	base, baseMeta, err = q.GetOrFetchProfile(ctx, project, baseID)
	if err != nil {
		return nil, nil, ProfileMeta{}, ProfileMeta{}, fmt.Errorf("fetching base profile: %w", err)
	}
	return current, base, currentMeta, baseMeta, nil
}

// buildDiffProfile merges current with the negated base, as pprof -diff_base
// does. Merge returns a new profile and leaves its inputs untouched; only base
// is negated in place, so it is copied first to keep the caller's profile intact.
func buildDiffProfile(current, base *profile.Profile) (*profile.Profile, error) {
	negBase := base.Copy()
	negBase.Scale(-1)
	merged, err := profile.Merge([]*profile.Profile{current, negBase})
	if err != nil {
		return nil, fmt.Errorf("merging profiles: %w", err)
	}
	return merged, nil
}

// prefetchResult summarizes a bulk prefetch so ComputeTrends can report why
// profiles had to be fetched individually.
type prefetchResult struct {
	Cached   int   // wanted profiles in the cache when the prefetch ended
	Skipped  int   // wanted profiles found but not cached
	LastSkip error // why the last skipped profile was not cached
	Stopped  error // why the scan ended early: the list call failed or the cache is full
}

// prefetchProfiles does a single paginated scan of the List API with profileBytes
// included in the response, caching the compressed bytes of each profile in the
// wanted set. This avoids O(n) individual List scans when ComputeTrends needs many
// profiles that were already discovered via a metadata-only ListProfiles call.
// Profiles are not parsed here; GetOrFetchProfile parses them on first use and
// drops undecodable ones from the cache. Inserts never evict a wanted profile,
// and the prefetch stops once the cache can take no more of them, since those
// would then have to be fetched individually after all.
func (q *CloudProfilerQuerier) prefetchProfiles(
	ctx context.Context,
	project string,
	wanted map[string]bool,
) prefetchResult {
	const maxScan = 20_000
	var res prefetchResult
	remaining := len(wanted)
	keep := make(map[string]bool, len(wanted))
	for id := range wanted {
		keep[profileCacheKey(project, id)] = true
	}

	it := q.svc.ListProfiles(ctx, &cloudprofilerpb.ListProfilesRequest{
		Parent:   "projects/" + project,
		PageSize: 1000,
	})
	for scanned := 0; scanned < maxScan && remaining > 0 && ctx.Err() == nil; scanned++ {
		p, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			res.Stopped = fmt.Errorf("list API call failed: %w", err)
			return res
		}
		// Key on the ID surfaced by profiler_list (p.Name when non-empty,
		// otherwise the synthetic ID) so the cache entry matches what
		// GetOrFetchProfile later looks up. Keying on the raw p.Name would
		// collide every empty-named profile onto a single entry.
		meta := profileFromAPI(p)
		if !wanted[meta.ProfileID] {
			continue
		}
		remaining--
		key := profileCacheKey(project, meta.ProfileID)
		if _, _, ok := q.cache.Get(key); ok {
			res.Cached++
			continue
		}
		err = q.cache.Put(key, p.ProfileBytes, meta, keep)
		switch {
		case err == nil:
			res.Cached++
		case errors.Is(err, errWouldEvict), errors.Is(err, errProcessCacheFull):
			res.Stopped = err
			return res
		default:
			res.Skipped++
			res.LastSkip = fmt.Errorf("profile %s: %w", meta.ProfileID, err)
		}
	}
	return res
}

// warning explains why the prefetch left profiles to individual fetches, or
// returns "" when nothing went wrong. total is the number of wanted profiles.
func (r prefetchResult) warning(total int) string {
	var causes []string
	if r.Stopped != nil {
		causes = append(causes, "stopped: "+r.Stopped.Error())
	}
	if r.Skipped > 0 {
		causes = append(causes, fmt.Sprintf("%d not cached, last: %v", r.Skipped, r.LastSkip))
	}
	if len(causes) == 0 {
		return ""
	}
	return fmt.Sprintf("Bulk prefetch cached %d/%d profiles (%s); the rest were fetched individually.", r.Cached, total, strings.Join(causes, "; "))
}

// ProfileValueTypes returns available value types for a profile.
func ProfileValueTypes(p *profile.Profile) []ValueTypeInfo {
	var types []ValueTypeInfo
	for i, vt := range p.SampleType {
		types = append(types, ValueTypeInfo{Index: i, Type: vt.Type, Unit: vt.Unit})
	}
	return types
}

// ComputeTrendsParams bundles the query arguments for ComputeTrends. The
// progress callback stays a separate parameter because it is behavior, not
// data.
type ComputeTrendsParams struct {
	Project        string
	ProfileType    string
	Target         string
	FunctionFilter string
	ValueIndex     int
	MaxProfiles    int
	MaxFunctions   int
}

// ComputeTrends tracks how specific functions' costs change over time across
// multiple profiles — the same data shown in Cloud Profiler UI's "Profile history".
//
// FunctionFilter is a substring match on function name. When set, only matching
// functions are tracked (cheap: single pass over samples per profile). When empty,
// the first non-empty profile is used to discover the top MaxFunctions functions,
// which are then tracked across all profiles.
func (q *CloudProfilerQuerier) ComputeTrends(
	ctx context.Context,
	params ComputeTrendsParams,
	progressFn func(current, total int, msg string),
) (*ProfileTrendsResult, error) {
	project, profileType, target, functionFilter := params.Project, params.ProfileType, params.Target, params.FunctionFilter
	valueIndex, maxProfiles, maxFunctions := params.ValueIndex, params.MaxProfiles, params.MaxFunctions

	profiles, err := q.ListProfiles(ctx, ListProfilesParams{
		Project:     project,
		ProfileType: profileType,
		Target:      target,
		PageSize:    maxProfiles,
	})
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
	pf := q.prefetchProfiles(ctx, project, wanted)

	// Without function_filter, the top functions of the first non-empty profile
	// become the tracked set; that profile is analyzed in the same loop iteration.
	discover := functionFilter == ""
	targetFunctions := map[string]bool{} // empty = track all matching functionFilter

	type funcInfo struct {
		file string
		peak int64
	}
	funcTimeline := make(map[string][]TrendsDataPoint)
	funcMeta := make(map[string]*funcInfo)
	var resolvedValueType *ValueTypeInfo
	var failed int
	var lastErr error

	total := len(profiles.Profiles)
	for i, meta := range profiles.Profiles {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("stopped after %d/%d profiles: %w", i, total, context.Cause(ctx))
		}
		if progressFn != nil {
			progressFn(i, total, fmt.Sprintf("Analyzing profile %d/%d...", i+1, total))
		}

		p, _, err := q.loadProfile(ctx, project, meta.ProfileID, false)
		if err != nil {
			failed++
			lastErr = err
			continue
		}

		if resolvedValueType == nil {
			vts := ProfileValueTypes(p)
			if valueIndex < len(vts) {
				resolvedValueType = &vts[valueIndex]
			}
		}

		if discover && len(targetFunctions) == 0 {
			topFuncs, _, _, err := TopFunctions(p, valueIndex, maxFunctions, "cumulative", "")
			if err != nil {
				// Analysis errors (e.g. invalid valueIndex) are deterministic —
				// retrying with another profile won't help.
				return nil, fmt.Errorf("analyzing profile for function discovery: %w", err)
			}
			if len(topFuncs) == 0 {
				continue // empty profile, discover from the next one
			}
			for _, f := range topFuncs {
				targetFunctions[f.FunctionName] = true
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
				ProfileID:       meta.ProfileID,
				Timestamp:       meta.StartTime,
				SelfValue:       c.self,
				SelfPct:         selfPct,
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

	analyzed := total - failed
	if analyzed == 0 {
		return nil, fmt.Errorf("failed to fetch or decode any of %d profiles (last error: %w)", total, lastErr)
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

	result := &ProfileTrendsResult{
		Target:         target,
		ProfileType:    profileType,
		ValueType:      vt,
		ProfileCount:   total,
		AnalyzedCount:  analyzed,
		TimeRangeStart: timeRangeStart,
		TimeRangeEnd:   timeRangeEnd,
		Functions:      functions,
		FailedProfiles: failed,
		Truncated:      truncated,
	}
	if lastErr != nil {
		result.LastError = lastErr.Error()
	}
	var warnings []string
	if discover && len(targetFunctions) == 0 {
		warnings = append(warnings, fmt.Sprintf("All %d analyzed profiles had no samples for value_index %d; there are no functions to track.", analyzed, valueIndex))
	}
	if analyzed < total {
		warnings = append(warnings, fmt.Sprintf("Only %d/%d profiles analyzed successfully (%d could not be fetched or decoded, see last_error); trend data may be incomplete.", analyzed, total, failed))
	}
	if w := pf.warning(total); w != "" {
		warnings = append(warnings, w)
	}
	result.Warning = strings.Join(warnings, " ")
	if truncated {
		result.TruncationHint = fmt.Sprintf("Showing top %d of %d functions. Use function_filter to narrow results.", maxFunctions, len(funcMeta))
	}

	return result, nil
}

// scanFunctionCosts does a single pass over profile samples, computing self and
// cumulative costs per function. When targets is non-empty only those functions
// are accumulated; otherwise filter (a function-name substring, "" = all) applies.
// Skipping non-matching functions during accumulation keeps tracking a small
// subset cheap.
func scanFunctionCosts(p *profile.Profile, valueIndex int, filter string, targets map[string]bool) (map[string]*funcCost, int64, error) {
	if err := validateValueIndex(p, valueIndex); err != nil {
		return nil, 0, err
	}
	costs := make(map[string]*funcCost)
	var total int64

	useTargets := len(targets) > 0
	seen := make(map[string]bool) // functions already counted in the current sample
	for _, sample := range p.Sample {
		value := sample.Value[valueIndex]
		total += value
		clear(seen)
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

func profileFromAPI(p *cloudprofilerpb.Profile) ProfileMeta {
	meta := ProfileMeta{
		ProfileID: p.Name,
	}
	// ProfileType is a proto enum — convert to the string name (e.g. "CPU", "HEAP").
	// Only accept values that are in the known set; unrecognized numeric values
	// (returned as their decimal string, e.g. "999") are treated as unset.
	pt := p.ProfileType.String()
	if slices.Contains(ProfileTypes, pt) {
		meta.ProfileType = pt
	}
	if p.Duration != nil {
		meta.Duration = p.Duration.AsDuration().String()
	}
	if p.StartTime != nil {
		meta.StartTime = p.StartTime.AsTime().UTC().Format(time.RFC3339Nano)
	}
	if p.Deployment != nil {
		meta.Target = p.Deployment.Target
		meta.DeploymentLabels = p.Deployment.Labels
	}
	if len(p.Labels) > 0 {
		meta.Labels = p.Labels
	}
	// The Cloud Profiler Export API leaves Profile.name empty for some
	// deployments, so there is no server-assigned ID to address a specific
	// profile with. Fall back to a stable ID derived from the profile's
	// identifying metadata so profiler_top/peek/flamegraph can round-trip the
	// value shown by profiler_list. Computed last, since it depends on the
	// fields populated above.
	if meta.ProfileID == "" {
		meta.ProfileID = syntheticProfileID(meta)
	}
	return meta
}

// syntheticProfileID derives a compact, deterministic ID from the fields that
// together identify a single profile: type, target, exact start time (nanosecond
// precision), duration, and the collecting instance. Two profiles from the same
// service can only share all of these if they are the same profile, so the hash
// is unique enough to address one while staying stable across processes (a user
// can copy it from one call and pass it to the next). The "syn:" prefix keeps it
// distinct from real resource names.
func syntheticProfileID(m ProfileMeta) string {
	h := sha256.New()
	for _, field := range []string{
		m.ProfileType,
		m.Target,
		m.StartTime,
		m.Duration,
		m.Labels["instance"],
	} {
		h.Write([]byte(field))
		h.Write([]byte{0}) // separator so field boundaries can't be forged
	}
	return "syn:" + hex.EncodeToString(h.Sum(nil))[:16]
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

// findInTrie returns the only non-root node whose name contains name, nil when
// none does, or an error listing the candidates when several do.
func findInTrie(root *trieNode, name string) (*trieNode, error) {
	var matches []*trieNode
	stack := slices.Collect(maps.Values(root.children))
	for len(stack) > 0 {
		node := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if strings.Contains(node.name, name) {
			matches = append(matches, node)
		}
		stack = slices.AppendSeq(stack, maps.Values(node.children))
	}
	switch len(matches) {
	case 0:
		return nil, nil
	case 1:
		return matches[0], nil
	default:
		slices.SortFunc(matches, compareByCost)
		names := make([]string, 0, 10)
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
	return slices.SortedFunc(maps.Values(node.children), compareByCost)
}

// compareByCost orders trie nodes by descending absolute cumulative cost, then
// by name, so equal-cost siblings keep a deterministic order.
func compareByCost(a, b *trieNode) int {
	return cmp.Or(cmp.Compare(absInt64(b.cumulative), absInt64(a.cumulative)), cmp.Compare(a.name, b.name))
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
