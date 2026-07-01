package server

import (
	"fmt"
	"runtime/debug"

	"github.com/modelcontextprotocol/experimental-ext-variants/go/sdk/variants"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpclient"
	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
	"github.com/tolmachov/mcp-gcp-observability/internal/tools"
)

// VariantID identifies a server capability variant.
type VariantID string

// Supported variant IDs. Each must have a corresponding entry in variantSpecs.
const (
	VariantFull       VariantID = "full"
	VariantCompact    VariantID = "compact"
	VariantMonitoring VariantID = "monitoring"
)

// allToolsCount is the number of tools registerAllTools registers. Single
// source of truth for the "full"/"compact" variants' tool-count claim;
// pinned by TestRegisterAllToolsCount.
const allToolsCount = 24

// profileCacheSize is the number of parsed profiles the profiler querier keeps
// in memory. Diff profiles produced by profiler_compare live only here, so this
// also bounds how many diffs remain navigable via diff_id.
const profileCacheSize = 10

// variantSpec declares one capability set: a register function (signature
// shared with registerAllTools / tools.RegisterCore), the mode it should
// register tools with, and the metadata exposed during variants negotiation.
type variantSpec struct {
	id          VariantID
	description string
	hints       map[string]string
	status      variants.VariantStatus
	register    func(srv *mcp.Server, d tools.Deps)
	mode        tools.RegistrationMode
}

// variantSpecs lists every supported variant in negotiation-priority order.
// Adding a variant here automatically wires it into --variant validation,
// the forced-variant build path, and the variants-protocol negotiation —
// the table is the single source of truth, so the slice and dispatch cannot
// drift apart. Built via buildVariantSpecs so the description strings can
// interpolate allToolsCount / tools.CoreToolsCount.
var variantSpecs = buildVariantSpecs()

func buildVariantSpecs() []variantSpec {
	return []variantSpec{
		{
			id:          VariantFull,
			description: fmt.Sprintf("All GCP observability tools (%d) with complete descriptions. Optimized for interactive incident investigation.", allToolsCount),
			hints:       map[string]string{variants.HintUseCase: "human-assistant", variants.HintContextSize: "standard"},
			status:      variants.Stable,
			register:    registerAllTools,
			mode:        tools.ModeStandard,
		},
		{
			id:          VariantCompact,
			description: fmt.Sprintf("All GCP observability tools (%d) with concise descriptions (~50%% shorter). Optimized for autonomous agents and tight context budgets.", allToolsCount),
			hints:       map[string]string{variants.HintUseCase: "autonomous-agent", variants.HintContextSize: "compact"},
			status:      variants.Stable,
			register:    registerAllTools,
			mode:        tools.ModeCompact,
		},
		{
			id:          VariantMonitoring,
			description: fmt.Sprintf("Core GCP tools only (%d): logs_summary, logs_services, errors_list/get, metrics_snapshot/top_contributors, trace_list/get, profiler_list/top. For automated monitoring bots and scheduled health checks.", tools.CoreToolsCount),
			hints:       map[string]string{variants.HintUseCase: "autonomous-agent", variants.HintContextSize: "compact"},
			status:      variants.Experimental,
			register:    tools.RegisterCore,
			mode:        tools.ModeCompact,
		},
	}
}

// KnownVariantIDs returns a copy of the supported variant IDs in negotiation
// priority order.
func KnownVariantIDs() []string {
	out := make([]string, len(variantSpecs))
	for i, v := range variantSpecs {
		out[i] = string(v.id)
	}
	return out
}

// findVariantSpec returns the spec for id (case-sensitive), or false if unknown.
func findVariantSpec(id string) (variantSpec, bool) {
	for _, v := range variantSpecs {
		if string(v.id) == id {
			return v, true
		}
	}
	return variantSpec{}, false
}

// buildSingleVariantServer builds a single *mcp.Server for the given variant ID.
// Used when --variant is specified to bypass the variants negotiation protocol.
// Any panic during registration is caught, the stack is logged, and the panic
// is converted to an error so server startup stays non-fatal.
func (s *Server) buildSingleVariantServer(
	variantID string,
	client *gcpclient.Client,
	reg *metrics.Registry,
	deps tools.Deps,
) (result *mcp.Server, retErr error) {
	defer func() {
		if r := recover(); r != nil {
			stack := debug.Stack()
			s.logger.Error("tool registration panic", "variant", variantID, "panic", r, "stack", string(stack))
			retErr = fmt.Errorf("tool registration panic: %v", r)
		}
	}()

	spec, ok := findVariantSpec(variantID)
	if !ok {
		return nil, fmt.Errorf("unknown variant %q: must be one of %v", variantID, KnownVariantIDs())
	}

	srv := s.newMCPInstance()
	deps.Mode = spec.mode
	spec.register(srv, deps)
	if err := s.registerResources(srv, client, reg); err != nil {
		return nil, err
	}
	s.registerPrompts(srv)
	return srv, nil
}

// registerAllTools registers every GCP observability tool on srv. The Mode
// field of d controls description verbosity (Standard vs Compact). Count is
// allToolsCount; TestRegisterAllToolsCount asserts the two match.
func registerAllTools(srv *mcp.Server, d tools.Deps) {
	// Logs
	tools.RegisterLogsQuery(srv, d)
	tools.RegisterLogsByTrace(srv, d)
	tools.RegisterLogsByRequestID(srv, d)
	tools.RegisterLogsFindRequests(srv, d)
	tools.RegisterLogsK8s(srv, d)
	tools.RegisterLogsServices(srv, d)
	tools.RegisterLogsSummary(srv, d)
	// Errors
	tools.RegisterErrorsList(srv, d)
	tools.RegisterErrorsGet(srv, d)
	tools.RegisterErrorsTrends(srv, d)
	// Traces
	tools.RegisterTraceGet(srv, d)
	tools.RegisterTraceList(srv, d)
	tools.RegisterTraceFindFromLogs(srv, d)
	// Metrics
	tools.RegisterMetricsList(srv, d)
	tools.RegisterMetricsSnapshot(srv, d)
	tools.RegisterMetricsTop(srv, d)
	tools.RegisterMetricsRelated(srv, d)
	tools.RegisterMetricsCompare(srv, d)
	// Profiler
	tools.RegisterProfilerList(srv, d)
	tools.RegisterProfilerTop(srv, d)
	tools.RegisterProfilerPeek(srv, d)
	tools.RegisterProfilerFlamegraph(srv, d)
	tools.RegisterProfilerCompare(srv, d)
	tools.RegisterProfilerTrends(srv, d)
}

// buildVariantsServer constructs a variants.Server with one *mcp.Server per
// entry in variantSpecs (in declaration order, with priority = index).
// Any panic during registration is caught, the stack is logged, and the panic
// is converted to an error so server startup stays non-fatal.
func (s *Server) buildVariantsServer(
	client *gcpclient.Client,
	reg *metrics.Registry,
	deps tools.Deps,
) (result *variants.Server, retErr error) {
	defer func() {
		if r := recover(); r != nil {
			stack := debug.Stack()
			s.logger.Error("tool registration panic", "panic", r, "stack", string(stack))
			retErr = fmt.Errorf("tool registration panic: %v", r)
		}
	}()

	impl := &mcp.Implementation{Name: "mcp-gcp-observability", Version: s.version}
	vs := variants.NewServer(impl)

	for i, spec := range variantSpecs {
		srv := s.newMCPInstance()
		deps.Mode = spec.mode
		spec.register(srv, deps)
		if err := s.registerResources(srv, client, reg); err != nil {
			return nil, err
		}
		s.registerPrompts(srv)

		vs = vs.WithVariant(variants.ServerVariant{
			ID:          string(spec.id),
			Description: spec.description,
			Hints:       spec.hints,
			Status:      spec.status,
		}, srv, i)
	}
	return vs, nil
}
