package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/modelcontextprotocol/experimental-ext-variants/go/sdk/variants"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"

	"github.com/tolmachov/mcp-gcp-observability/internal/authsrv"
	"github.com/tolmachov/mcp-gcp-observability/internal/gcpclient"
	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
	"github.com/tolmachov/mcp-gcp-observability/internal/tools"
)

// Transport defines the server transport mode.
type Transport string

const (
	// TransportStdio uses standard input/output (default, for Claude Desktop and Claude Code).
	TransportStdio Transport = "stdio"
	// TransportHTTP uses streamable HTTP (for remote deployments).
	TransportHTTP Transport = "http"
)

// serverInstructions is the workflow guidance injected into every MCP server instance.
const serverInstructions = "Recommended workflow: " +
	"1) logs_services — discover available services. " +
	"2) logs_summary — get severity distribution, top errors, and top services for initial triage. " +
	"3) errors_list — list error groups sorted by count. " +
	"4) logs_query or logs_k8s — investigate specific logs with filters. " +
	"5) logs_by_trace — follow a single request across services using a trace ID from logs_find_requests or logs_query results. " +
	"6) trace_list — search for traces by span name, latency, or time range without knowing trace IDs. " +
	"7) trace_get — get detailed span tree for a trace to understand request timing and dependencies. " +
	"Always prefer logs_k8s over logs_query when investigating Kubernetes workloads. " +
	"For metrics analysis: " +
	"1) metrics_list — discover available metrics. " +
	"2) metrics_snapshot — get semantic snapshot with baseline comparison, trend detection, and classification. " +
	"3) metrics_top_contributors — break down by label dimension to find which values contribute most to an anomaly. " +
	"4) metrics_related — check correlated signals configured in the registry. " +
	"5) metrics_compare — compare two arbitrary time windows (e.g. before/after deploy). " +
	"For profiling analysis: " +
	"1) profiler_list — discover available profiles by service and type. " +
	"2) profiler_top — see top functions by resource consumption. " +
	"3) profiler_peek — understand a hotspot's callers and callees. " +
	"4) profiler_flamegraph — view bounded subtree of the call graph. " +
	"5) profiler_compare — compare two profiles to find regressions; pass base_profile_id to top/peek/flamegraph to navigate a diff. " +
	"6) profiler_trends — track how function costs change over time across multiple profiles. " +
	"Use profiler_compare for point-in-time A/B comparison; use profiler_trends for historical cost evolution."

// Server is the MCP server for GCP Observability.
type Server struct {
	completer *promptCompleter
	cfg       *gcpclient.Config
	version   string
	logger    *slog.Logger
	stdin     io.Reader
	stdout    io.Writer
	errOut    io.Writer
	project   tools.ProjectPolicy
	profiler  chan struct{}
}

// New creates a new MCP server.
func New(cfg *gcpclient.Config, version string, stdin io.Reader, stdout, errOut io.Writer) (*Server, error) {
	project, err := tools.NewProjectPolicy(cfg.DefaultProject)
	if err != nil {
		return nil, err
	}
	completer := &promptCompleter{}

	logger := slog.New(slog.NewJSONHandler(errOut, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	tools.SetNotifyLogger(logger)

	s := &Server{
		completer: completer,
		cfg:       cfg,
		version:   version,
		logger:    logger,
		stdin:     stdin,
		stdout:    stdout,
		errOut:    errOut,
		project:   project,
		profiler:  make(chan struct{}, 2),
	}
	return s, nil
}

// panicRecoveryMiddleware returns a receiving middleware that recovers from
// panics in tool handlers, logging the stack trace and returning an error.
func panicRecoveryMiddleware(logger *slog.Logger) func(mcp.MethodHandler) mcp.MethodHandler {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (result mcp.Result, err error) {
			defer func() {
				if r := recover(); r != nil {
					stack := debug.Stack()
					logger.Error("panic in handler", "method", method, "panic", r, "stack", string(stack))
					err = fmt.Errorf("internal server error: panic in handler for %s: %v", method, r)
				}
			}()
			return next(ctx, method, req)
		}
	}
}

// newMCPInstance creates a fresh *mcp.Server configured with the standard
// instructions, logger, the given completion handler, and panic-recovery
// middleware. Used to create the full, compact, and monitoring variant
// servers; per-user HTTP assemblies pass their own completer.
func (s *Server) newMCPInstance(completer *promptCompleter) *mcp.Server {
	srv := mcp.NewServer(
		&mcp.Implementation{
			Name:    "mcp-gcp-observability",
			Version: s.version,
		},
		&mcp.ServerOptions{
			Instructions:      serverInstructions,
			Logger:            s.logger,
			CompletionHandler: completer.Handle,
		},
	)
	srv.AddReceivingMiddleware(panicRecoveryMiddleware(s.logger))
	srv.AddReceivingMiddleware(authErrorHintMiddleware())
	srv.AddReceivingMiddleware(toolLimitsMiddleware(make(chan struct{}, 4), s.profiler, s.logger))
	return srv
}

const maxEncodedToolResultBytes = 2 << 20
const structuredResultContentNotice = "The complete result is available in structuredContent."

func toolLimitsMiddleware(userCalls, profilerCalls chan struct{}, logger *slog.Logger) func(mcp.MethodHandler) mcp.MethodHandler {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/call" {
				return next(ctx, method, req)
			}
			select {
			case userCalls <- struct{}{}:
				defer func() { <-userCalls }()
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			var releaseProfiler func()
			if call, ok := req.(*mcp.CallToolRequest); ok && call.Params != nil && strings.HasPrefix(call.Params.Name, "profiler_") {
				profilerCtx, cancel := context.WithTimeout(ctx, gcpdata.ProfilerScanTimeout)
				defer cancel()
				ctx = profilerCtx
				if len(profilerCalls) == cap(profilerCalls) {
					logger.Warn("profiler_saturation", "limit", cap(profilerCalls), "tool", call.Params.Name)
				}
				select {
				case profilerCalls <- struct{}{}:
					releaseProfiler = func() { <-profilerCalls }
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			if releaseProfiler != nil {
				defer releaseProfiler()
			}
			result, err := next(ctx, method, req)
			if err != nil || result == nil {
				return result, err
			}
			encoded, marshalErr := json.Marshal(result)
			if marshalErr != nil {
				return nil, fmt.Errorf("serialize tool result: %w", marshalErr)
			}
			// The typed MCP helper mirrors structuredContent into a JSON text
			// content block. For large but valid results that duplication alone
			// can push the wire response over budget. Remove only an exact
			// auto-generated duplicate, then measure the actual response again.
			// Custom human-facing content (for example metric analysis without
			// chart points) is never rewritten.
			if len(encoded) > maxEncodedToolResultBytes && compactDuplicatedStructuredContent(result) {
				encoded, marshalErr = json.Marshal(result)
				if marshalErr != nil {
					return nil, fmt.Errorf("serialize compacted tool result: %w", marshalErr)
				}
			}
			if len(encoded) > maxEncodedToolResultBytes {
				logger.Error("response_budget_violation", "method", method, "bytes", len(encoded), "limit", maxEncodedToolResultBytes)
				return nil, fmt.Errorf("tool result exceeds the %d-byte response budget", maxEncodedToolResultBytes)
			}
			return result, nil
		}
	}
}

func compactDuplicatedStructuredContent(result mcp.Result) bool {
	call, ok := result.(*mcp.CallToolResult)
	if !ok || call.StructuredContent == nil || len(call.Content) != 1 {
		return false
	}
	textContent, ok := call.Content[0].(*mcp.TextContent)
	if !ok {
		return false
	}
	structured, err := json.Marshal(call.StructuredContent)
	if err != nil || textContent.Text != string(structured) {
		return false
	}
	call.Content = []mcp.Content{&mcp.TextContent{Text: structuredResultContentNotice}}
	return true
}

// RunOptions selects the transport and its settings for Server.Run.
type RunOptions struct {
	// Transport is stdio (default) or http.
	Transport Transport
	// HTTPAddr is the listen address for the http transport.
	HTTPAddr string
	// VariantID, when non-empty, forces a specific capability set and
	// bypasses the variants negotiation protocol entirely (the client sees a
	// plain MCP server). Valid values are listed by KnownVariantIDs.
	VariantID string
	// Auth is mandatory for HTTP: MCP requests require a Google-backed bearer
	// token and GCP API calls run under each caller's own identity. It must be
	// nil for stdio, which uses Application Default Credentials.
	Auth *authsrv.Config
}

// Run starts the MCP server using the configured transport.
func (s *Server) Run(ctx context.Context, opts RunOptions) error {
	if opts.VariantID != "" {
		if _, ok := findVariantSpec(opts.VariantID); !ok {
			return fmt.Errorf("unknown variant %q: must be one of %v", opts.VariantID, KnownVariantIDs())
		}
	}
	switch opts.Transport {
	case TransportStdio, "", TransportHTTP:
	default:
		return fmt.Errorf("unsupported transport %q: must be %q or %q", opts.Transport, TransportStdio, TransportHTTP)
	}
	if opts.Auth != nil && opts.Transport != TransportHTTP {
		return fmt.Errorf("auth requires --transport %s", TransportHTTP)
	}
	if opts.Transport == TransportHTTP && opts.Auth == nil {
		return fmt.Errorf("HTTP transport requires Google OAuth and Firestore configuration")
	}

	// LoadRegistry merges user overlay (if any) with embedded GCP defaults.
	registryPath := s.resolveRegistryPath()
	reg, regErr := metrics.LoadRegistry(registryPath)
	if regErr != nil {
		return fmt.Errorf("loading metrics registry: %w", regErr)
	}

	if opts.Transport == TransportHTTP {
		return s.runHTTPWithAuth(ctx, reg, opts)
	}

	client, err := gcpclient.New(ctx, s.cfg)
	if err != nil {
		return fmt.Errorf("creating GCP client: %w", err)
	}
	defer func() {
		if closeErr := client.Close(); closeErr != nil {
			s.logger.Warn("failed to close GCP client", "err", closeErr)
		}
	}()

	s.wireCompleter(s.completer, reg, client)
	deps := s.buildDeps(client, reg)
	defer func() {
		if closeErr := deps.Profiler.Close(); closeErr != nil {
			s.logger.Warn("failed to close profiler cache", "err", closeErr)
		}
	}()

	if opts.VariantID != "" {
		srv, buildErr := s.buildSingleVariantServer(VariantID(opts.VariantID), client, deps, s.completer)
		if buildErr != nil {
			return fmt.Errorf("building variant server: %w", buildErr)
		}
		s.logger.Info("Starting with forced variant", "variant", opts.VariantID)
		return s.runStdio(ctx, srv)
	}

	vs, err := s.buildVariantsServer(client, deps, s.completer)
	if err != nil {
		return fmt.Errorf("building variants server: %w", err)
	}
	return s.runStdio(ctx, vs)
}

// buildDeps assembles the base tools.Deps for one GCP client set; the variant
// builders clone it and set Mode per spec.
func (s *Server) buildDeps(client *gcpclient.Client, reg *metrics.Registry) tools.Deps {
	return tools.Deps{
		Logs:     gcpdata.NewLoggingQuerier(client.LoggingClient()),
		Errors:   gcpdata.NewErrorReportingQuerier(client.ErrorsClient()),
		Traces:   gcpdata.NewCloudTraceQuerier(client.TraceClient()),
		Profiler: gcpdata.NewCloudProfilerQuerier(client.ProfilerService()),
		Querier:  gcpdata.NewMonitoringQuerier(client.MonitoringClient()),
		Registry: reg,
		Project:  s.project,
	}
}

// userAssemblyBuilder returns the builder the user pool uses to construct one
// authenticated user's complete HTTP assembly: GCP clients running under the
// user's token, a fresh completer and profile cache, and the negotiated (or
// forced) variant server(s).
func (s *Server) userAssemblyBuilder(reg *metrics.Registry, variantID string) userHandlerBuilder {
	return func(ctx context.Context, user *authsrv.UserIdentity, ts oauth2.TokenSource) (http.Handler, io.Closer, error) {
		cfg := *s.cfg
		client, err := gcpclient.NewWithTokenSource(ctx, &cfg, ts)
		if err != nil {
			return nil, nil, fmt.Errorf("creating per-user GCP client: %w", err)
		}
		completer := &promptCompleter{}
		s.wireCompleter(completer, reg, client)
		deps := s.buildDeps(client, reg)

		if variantID != "" {
			srv, buildErr := s.buildSingleVariantServer(VariantID(variantID), client, deps, completer)
			if buildErr != nil {
				return nil, nil, errors.Join(fmt.Errorf("building variant server: %w", buildErr), deps.Profiler.Close(), client.Close())
			}
			handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{Stateless: true})
			return handler, multiCloser{deps.Profiler, client}, nil
		}

		vs, buildErr := s.buildVariantsServer(client, deps, completer)
		if buildErr != nil {
			return nil, nil, errors.Join(fmt.Errorf("building variants server: %w", buildErr), deps.Profiler.Close(), client.Close())
		}
		return variants.NewStreamableHTTPHandler(vs, &mcp.StreamableHTTPOptions{Stateless: true}), multiCloser{vs, deps.Profiler, client}, nil
	}
}

// wireCompleter binds a completer to a registry and a GCP client set. The
// service-list cache lives inside the closure, so each completer (one per
// user in HTTP auth mode) caches independently.
func (s *Server) wireCompleter(c *promptCompleter, reg *metrics.Registry, client *gcpclient.Client) {
	c.registry = reg
	c.project = s.project
	if !s.project.Pinned() {
		c.loadServices = nil
		return
	}
	c.loadServices = newCachedServiceLister(func(ctx context.Context) (*gcpdata.ServiceList, error) {
		return gcpdata.ListServices(ctx, client.LoggingClient(), s.project.Project(), "")
	}, s.logger)
}

// resolveRegistryPath returns the metrics registry overlay path to load: the
// explicitly configured file, or an auto-probed .mcp/metrics_registry.yaml in
// the working directory when no file is configured. Returns "" when neither is
// available, in which case only the embedded defaults are used.
func (s *Server) resolveRegistryPath() string {
	if s.cfg.MetricsRegistryFile != "" {
		return s.cfg.MetricsRegistryFile
	}
	cwd, cwdErr := os.Getwd()
	if cwdErr != nil {
		s.logger.Warn("could not determine working directory for registry auto-probe", "err", cwdErr)
		return ""
	}
	candidate := filepath.Join(cwd, ".mcp", "metrics_registry.yaml")
	if _, statErr := os.Stat(candidate); statErr != nil {
		if !errors.Is(statErr, fs.ErrNotExist) {
			s.logger.Warn("could not stat registry candidate, skipping auto-probe", "path", candidate, "err", statErr)
		}
		return ""
	}
	s.logger.Info("auto-loaded metrics registry overlay", "path", candidate)
	return candidate
}
