package server

import (
	"fmt"
	"runtime/debug"
	"strings"

	"github.com/modelcontextprotocol/experimental-ext-variants/go/sdk/variants"
	"github.com/modelcontextprotocol/go-sdk/mcp"

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

// toolSpec declares one tool: its registration function, whether the
// monitoring variant includes it, and whether it scans Cloud Profiler (such
// calls share the process-wide profiler concurrency limit and run under
// gcpdata.ProfilerScanTimeout, see toolLimitsMiddleware). name must equal the
// name register gives the tool; TestToolSpecsMatchRegisteredTools pins it.
type toolSpec struct {
	name        string
	register    func(*mcp.Server, tools.Deps)
	core        bool
	profileScan bool
}

// toolSpecs is the single table of tools. registerAllTools, registerCoreTools,
// the variant descriptions and the profiler limit all derive from it.
var toolSpecs = []toolSpec{
	// Logs
	{name: "logs_query", register: tools.RegisterLogsQuery},
	{name: "logs_by_trace", register: tools.RegisterLogsByTrace},
	{name: "logs_by_request_id", register: tools.RegisterLogsByRequestID},
	{name: "logs_find_requests", register: tools.RegisterLogsFindRequests},
	{name: "logs_k8s", register: tools.RegisterLogsK8s},
	{name: "logs_services", register: tools.RegisterLogsServices, core: true},
	{name: "logs_summary", register: tools.RegisterLogsSummary, core: true},
	// Errors
	{name: "errors_list", register: tools.RegisterErrorsList, core: true},
	{name: "errors_get", register: tools.RegisterErrorsGet, core: true},
	{name: "errors_trends", register: tools.RegisterErrorsTrends},
	// Traces
	{name: "trace_get", register: tools.RegisterTraceGet, core: true},
	{name: "trace_list", register: tools.RegisterTraceList, core: true},
	{name: "trace_find_from_logs", register: tools.RegisterTraceFindFromLogs},
	// Metrics
	{name: "metrics_list", register: tools.RegisterMetricsList},
	{name: "metrics_snapshot", register: tools.RegisterMetricsSnapshot, core: true},
	{name: "metrics_top_contributors", register: tools.RegisterMetricsTop, core: true},
	{name: "metrics_related", register: tools.RegisterMetricsRelated},
	{name: "metrics_compare", register: tools.RegisterMetricsCompare},
	// Profiler
	{name: "profiler_list", register: tools.RegisterProfilerList, core: true, profileScan: true},
	{name: "profiler_top", register: tools.RegisterProfilerTop, core: true, profileScan: true},
	{name: "profiler_peek", register: tools.RegisterProfilerPeek, profileScan: true},
	{name: "profiler_flamegraph", register: tools.RegisterProfilerFlamegraph, profileScan: true},
	{name: "profiler_compare", register: tools.RegisterProfilerCompare, profileScan: true},
	{name: "profiler_trends", register: tools.RegisterProfilerTrends, profileScan: true},
}

// coreToolNames lists the monitoring variant's tools in table order.
var coreToolNames = func() []string {
	var names []string
	for _, t := range toolSpecs {
		if t.core {
			names = append(names, t.name)
		}
	}
	return names
}()

// profileScanTools is the set of tools subject to the profiler limits.
var profileScanTools = func() map[string]bool {
	set := make(map[string]bool)
	for _, t := range toolSpecs {
		if t.profileScan {
			set[t.name] = true
		}
	}
	return set
}()

// registerAllTools registers every tool in toolSpecs on srv. The Mode field
// of d controls description verbosity (Standard vs Compact).
func registerAllTools(srv *mcp.Server, d tools.Deps) {
	for _, t := range toolSpecs {
		t.register(srv, d)
	}
}

// registerCoreTools registers the monitoring variant's tools (core entries of
// toolSpecs) on srv.
func registerCoreTools(srv *mcp.Server, d tools.Deps) {
	for _, t := range toolSpecs {
		if t.core {
			t.register(srv, d)
		}
	}
}

// variantSpec declares one capability set: a register function (signature
// shared with registerAllTools / registerCoreTools), the mode it should
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
// interpolate the tool counts and core tool names from toolSpecs.
var variantSpecs = buildVariantSpecs()

func buildVariantSpecs() []variantSpec {
	return []variantSpec{
		{
			id:          VariantFull,
			description: fmt.Sprintf("All GCP observability tools (%d) with complete descriptions. Optimized for interactive incident investigation.", len(toolSpecs)),
			hints:       map[string]string{variants.HintUseCase: "human-assistant", variants.HintContextSize: "standard"},
			status:      variants.Stable,
			register:    registerAllTools,
			mode:        tools.ModeStandard,
		},
		{
			id:          VariantCompact,
			description: fmt.Sprintf("All GCP observability tools (%d) with concise descriptions (~50%% shorter). Optimized for autonomous agents and tight context budgets.", len(toolSpecs)),
			hints:       map[string]string{variants.HintUseCase: "autonomous-agent", variants.HintContextSize: "compact"},
			status:      variants.Stable,
			register:    registerAllTools,
			mode:        tools.ModeCompact,
		},
		{
			id:          VariantMonitoring,
			description: fmt.Sprintf("Core GCP tools only (%d): %s. For automated monitoring bots and scheduled health checks.", len(coreToolNames), strings.Join(coreToolNames, ", ")),
			hints:       map[string]string{variants.HintUseCase: "autonomous-agent", variants.HintContextSize: "compact"},
			status:      variants.Experimental,
			register:    registerCoreTools,
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

// recoverRegistrationPanic converts a panic during tool registration into a
// returned error so server startup stays non-fatal. Deferred by both variant
// builders. variant is "" for the multi-variant path.
func (s *Server) recoverRegistrationPanic(variant string, retErr *error) {
	if r := recover(); r != nil {
		stack := debug.Stack()
		if variant != "" {
			s.logger.Error("tool registration panic", "variant", variant, "panic", r, "stack", string(stack))
		} else {
			s.logger.Error("tool registration panic", "panic", r, "stack", string(stack))
		}
		*retErr = fmt.Errorf("tool registration panic: %v", r)
	}
}

// buildSingleVariantServer builds a single *mcp.Server for the given variant ID.
// Used when --variant is specified to bypass the variants negotiation protocol.
// Any panic during registration is caught, the stack is logged, and the panic
// is converted to an error so server startup stays non-fatal.
func (s *Server) buildSingleVariantServer(
	variantID VariantID,
	deps tools.Deps,
	completer *promptCompleter,
) (result *mcp.Server, retErr error) {
	defer s.recoverRegistrationPanic(string(variantID), &retErr)

	spec, ok := findVariantSpec(string(variantID))
	if !ok {
		return nil, fmt.Errorf("unknown variant %q: must be one of %v", variantID, KnownVariantIDs())
	}

	srv := s.newMCPInstance(completer)
	spec.register(srv, deps.WithMode(spec.mode))
	s.registerResources(srv, deps)
	s.registerPrompts(srv)
	return srv, nil
}

// buildVariantsServer constructs a variants.Server with one *mcp.Server per
// entry in variantSpecs (in declaration order, with priority = index).
// Any panic during registration is caught, the stack is logged, and the panic
// is converted to an error so server startup stays non-fatal.
func (s *Server) buildVariantsServer(
	deps tools.Deps,
	completer *promptCompleter,
) (result *variants.Server, retErr error) {
	defer s.recoverRegistrationPanic("", &retErr)

	impl := &mcp.Implementation{Name: "mcp-gcp-observability", Version: s.version}
	vs := variants.NewServer(impl)

	for i, spec := range variantSpecs {
		srv := s.newMCPInstance(completer)
		spec.register(srv, deps.WithMode(spec.mode))
		s.registerResources(srv, deps)
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
