package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"

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
	"5) profiler_compare — compare two profiles to find regressions (use diff_id with top/peek/flamegraph). " +
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
}

// New creates a new MCP server.
func New(cfg *gcpclient.Config, version string, stdin io.Reader, stdout, errOut io.Writer) (*Server, error) {
	completer := &promptCompleter{}

	logger := slog.New(slog.NewTextHandler(errOut, &slog.HandlerOptions{
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
	return srv
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
	// Auth, when non-nil, enables the embedded OAuth authorization server on
	// the http transport: MCP requests require a bearer token, and GCP API
	// calls run under each caller's own Google identity. nil serves
	// unauthenticated HTTP (put an authenticating proxy in front) and uses
	// Application Default Credentials, like stdio.
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

	// LoadRegistry merges user overlay (if any) with embedded GCP defaults.
	registryPath := s.resolveRegistryPath()
	reg, regErr := metrics.LoadRegistry(registryPath)
	if regErr != nil {
		return fmt.Errorf("loading metrics registry: %w", regErr)
	}

	if opts.Auth != nil {
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

	if opts.VariantID != "" {
		srv, buildErr := s.buildSingleVariantServer(VariantID(opts.VariantID), client, deps, s.completer)
		if buildErr != nil {
			return fmt.Errorf("building variant server: %w", buildErr)
		}
		s.logger.Info("Starting with forced variant", "variant", opts.VariantID)
		if opts.Transport == TransportHTTP {
			return s.runMCPHTTP(ctx, srv, opts.HTTPAddr)
		}
		return s.runStdio(ctx, srv)
	}

	vs, err := s.buildVariantsServer(client, deps, s.completer)
	if err != nil {
		return fmt.Errorf("building variants server: %w", err)
	}
	if opts.Transport == TransportHTTP {
		return s.runHTTP(ctx, vs, opts.HTTPAddr)
	}
	return s.runStdio(ctx, vs)
}

// buildDeps assembles the base tools.Deps for one GCP client set; the variant
// builders clone it and set Mode per spec.
func (s *Server) buildDeps(client *gcpclient.Client, reg *metrics.Registry) tools.Deps {
	return tools.Deps{
		Logs:           gcpdata.NewLoggingQuerier(client.LoggingClient()),
		Errors:         gcpdata.NewErrorReportingQuerier(client.ErrorsClient()),
		Traces:         gcpdata.NewCloudTraceQuerier(client.TraceClient()),
		Profiler:       gcpdata.NewCloudProfilerQuerier(client.ProfilerService(), profileCacheSize),
		Querier:        gcpdata.NewMonitoringQuerier(client.MonitoringClient()),
		Registry:       reg,
		DefaultProject: client.Config().DefaultProject,
		LogsMaxLimit:   s.cfg.LogsMaxLimit,
		ErrorsMaxLimit: s.cfg.ErrorsMaxLimit,
	}
}

// userAssemblyBuilder returns the builder the user pool uses to construct one
// authenticated user's complete HTTP assembly: GCP clients running under the
// user's token, a fresh completer and profile cache, and the negotiated (or
// forced) variant server(s).
func (s *Server) userAssemblyBuilder(reg *metrics.Registry, variantID string) userHandlerBuilder {
	return func(ctx context.Context, user *authsrv.UserIdentity, ts oauth2.TokenSource) (http.Handler, io.Closer, error) {
		// The grant may be bound to a user-chosen project (project-choice
		// mode); it becomes this assembly's default project. With a pinned
		// project the two values coincide.
		cfg := *s.cfg
		if user.Project != "" {
			cfg.DefaultProject = user.Project
		}
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
				return nil, nil, errors.Join(fmt.Errorf("building variant server: %w", buildErr), client.Close())
			}
			handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
			return handler, client, nil
		}

		vs, buildErr := s.buildVariantsServer(client, deps, completer)
		if buildErr != nil {
			return nil, nil, errors.Join(fmt.Errorf("building variants server: %w", buildErr), client.Close())
		}
		return variants.NewStreamableHTTPHandler(vs, nil), multiCloser{vs, client}, nil
	}
}

// wireCompleter binds a completer to a registry and a GCP client set. The
// service-list cache lives inside the closure, so each completer (one per
// user in HTTP auth mode) caches independently.
func (s *Server) wireCompleter(c *promptCompleter, reg *metrics.Registry, client *gcpclient.Client) {
	defaultProject := client.Config().DefaultProject
	c.registry = reg
	c.defaultProject = defaultProject
	c.loadServices = newCachedServiceLister(func(ctx context.Context) (*gcpdata.ServiceList, error) {
		return gcpdata.ListServices(ctx, client.LoggingClient(), defaultProject, "")
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
