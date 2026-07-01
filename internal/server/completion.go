package server

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
)

// maxCompletionValues caps the number of values returned for a single
// completion request. The MCP spec limits completions to 100 entries; when more
// match we truncate and signal HasMore so the client can refine its prefix.
const maxCompletionValues = 100

// serviceCompletionTimeout bounds the one-time log scan used to discover
// service names for completion, so a slow Cloud Logging call cannot stall the
// editor while the user is typing an argument.
const serviceCompletionTimeout = 5 * time.Second

// promptCompleter provides autocomplete for prompt and resource-template
// arguments. Per the MCP spec, completion/complete serves suggestions for
// prompt arguments (ref/prompt) and resource-template arguments (ref/resource)
// — never for tool arguments.
type promptCompleter struct {
	registry *metrics.Registry
	// loadServices lazily discovers service names (by scanning recent logs) and
	// caches them for the session. nil until a GCP client is wired in Run.
	loadServices func(ctx context.Context) []string
	// defaultProject is the configured GCP project, offered when completing the
	// {project} argument of resource templates.
	defaultProject string
}

// defaultMetricCandidates are common GCP metric types shown when the registry is empty.
var defaultMetricCandidates = []string{
	"compute.googleapis.com/instance/cpu/utilization",
	"compute.googleapis.com/instance/disk/read_bytes_count",
	"compute.googleapis.com/instance/network/received_bytes_count",
	"loadbalancing.googleapis.com/https/request_count",
	"loadbalancing.googleapis.com/https/total_latencies",
	"run.googleapis.com/request_count",
	"run.googleapis.com/request_latencies",
	"cloudsql.googleapis.com/database/cpu/utilization",
	"cloudsql.googleapis.com/database/memory/utilization",
	"storage.googleapis.com/api/request_count",
	"pubsub.googleapis.com/topic/send_request_count",
	"appengine.googleapis.com/http/server/response_latencies",
}

// profileTypeCandidates are the Cloud Profiler profile types accepted by the
// profile_type argument of the investigate-profile prompt.
var profileTypeCandidates = []string{
	"CPU", "HEAP", "HEAP_ALLOC", "WALL", "CONTENTION", "THREADS", "PEAK_HEAP",
}

func (p *promptCompleter) Handle(ctx context.Context, req *mcp.CompleteRequest) (*mcp.CompleteResult, error) {
	var values []string
	if req.Params.Ref != nil {
		candidates := p.candidatesFor(ctx, req.Params.Ref.Type, req.Params.Ref.Name, req.Params.Argument.Name)
		prefix := strings.ToLower(req.Params.Argument.Value)
		for _, c := range candidates {
			if prefix == "" || strings.Contains(strings.ToLower(c), prefix) {
				values = append(values, c)
			}
		}
	}

	total := len(values)
	if total > maxCompletionValues {
		values = values[:maxCompletionValues]
	}
	return &mcp.CompleteResult{
		Completion: mcp.CompletionResultDetails{
			Values:  values,
			HasMore: total > maxCompletionValues,
		},
	}, nil
}

// candidatesFor returns the unfiltered candidate list for a completion ref, or
// nil when the argument has no completion source. Completion is scoped to the
// arguments each prompt actually declares (so an unknown prompt or undeclared
// argument yields nothing), while resource templates share the {project} source.
func (p *promptCompleter) candidatesFor(ctx context.Context, refType, refName, argName string) []string {
	switch refType {
	case "ref/prompt":
		return p.promptArgCandidates(ctx, refName, argName)
	case "ref/resource":
		if argName == "project" && p.defaultProject != "" {
			return []string{p.defaultProject}
		}
	}
	return nil
}

// promptArgCandidates maps a (prompt, argument) pair to its completion source.
// The pairs mirror the arguments declared in registerPrompts: metric_type on
// investigate-metrics, profile_type on investigate-profile, and service on the
// three prompts that accept a service filter.
func (p *promptCompleter) promptArgCandidates(ctx context.Context, prompt, arg string) []string {
	switch {
	case prompt == "investigate-metrics" && arg == "metric_type":
		return p.metricCandidates()
	case arg == "service" && (prompt == "investigate-errors" || prompt == "investigate-metrics" || prompt == "investigate-profile"):
		if p.loadServices == nil {
			return nil
		}
		return p.loadServices(ctx)
	case prompt == "investigate-profile" && arg == "profile_type":
		return profileTypeCandidates
	}
	return nil
}

// newCachedServiceLister wraps a service-discovery fetch in a session-lifetime
// cache so completion never re-scans logs on every keystroke. The first call
// runs fetch under a bounded timeout; the result (including an empty list on
// error) is cached for all subsequent calls.
func newCachedServiceLister(fetch func(ctx context.Context) (*gcpdata.ServiceList, error), logger *slog.Logger) func(ctx context.Context) []string {
	var (
		mu     sync.Mutex
		loaded bool
		cached []string
	)
	return func(ctx context.Context) []string {
		mu.Lock()
		defer mu.Unlock()
		if loaded {
			return cached
		}
		loaded = true // cache even on failure to avoid re-scanning logs while typing
		cctx, cancel := context.WithTimeout(ctx, serviceCompletionTimeout)
		defer cancel()
		list, err := fetch(cctx)
		if err != nil {
			logger.Warn("service completion: discovery failed, caching empty result", "err", err)
			return nil
		}
		names := make([]string, 0, len(list.Services))
		for _, svc := range list.Services {
			if svc.Name != "" {
				names = append(names, svc.Name)
			}
		}
		cached = names
		return cached
	}
}

func (p *promptCompleter) metricCandidates() []string {
	if p.registry != nil && p.registry.Count() > 0 {
		entries := p.registry.List("", metrics.MetricKind(""))
		candidates := make([]string, 0, len(entries))
		for _, e := range entries {
			candidates = append(candidates, e.MetricType)
		}
		return candidates
	}
	return defaultMetricCandidates
}
