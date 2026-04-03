package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpclient"
	"github.com/tolmachov/mcp-gcp-observability/internal/tools"
)

// Server represents the MCP server for GCP Observability.
type Server struct {
	mcpServer *server.MCPServer
	cfg       *gcpclient.Config
	stdin     io.Reader
	stdout    io.Writer
	errOut    io.Writer
}

// New creates a new MCP server.
func New(cfg *gcpclient.Config, version string, stdin io.Reader, stdout, errOut io.Writer) (*Server, error) {
	mcpServer := server.NewMCPServer(
		"mcp-gcp-observability",
		version,
		server.WithToolCapabilities(true),
		server.WithRecovery(),
		server.WithLogging(),
		server.WithInstructions("Recommended workflow: "+
			"1) logs.services — discover available services. "+
			"2) logs.summary — get severity distribution, top errors, and top services for initial triage. "+
			"3) errors.list — list error groups sorted by count. "+
			"4) logs.query or logs.k8s — investigate specific logs with filters. "+
			"5) logs.by_trace — follow a single request across services using a trace ID from logs.find_requests or logs.query results. "+
			"6) trace.get — get detailed span tree for a trace to understand request timing and dependencies. "+
			"Always prefer logs.k8s over logs.query when investigating Kubernetes workloads."),
	)

	return &Server{
		mcpServer: mcpServer,
		cfg:       cfg,
		stdin:     stdin,
		stdout:    stdout,
		errOut:    errOut,
	}, nil
}

// Run starts the MCP server over stdio.
func (s *Server) Run(ctx context.Context) error {
	client, err := gcpclient.New(ctx, s.cfg)
	if err != nil {
		return fmt.Errorf("creating GCP client: %w", err)
	}
	defer func() {
		if closeErr := client.Close(); closeErr != nil {
			log.New(s.errOut, "[mcp-gcp-observability] ", log.LstdFlags).
				Printf("warning: failed to close GCP client: %v", closeErr)
		}
	}()

	tools.RegisterTools(s.mcpServer, []tools.Handler{
		// Phase 1
		tools.NewLogsQueryHandler(client),
		tools.NewLogsByTraceHandler(client),
		tools.NewLogsByRequestIDHandler(client),
		tools.NewLogsFindRequestsHandler(client),
		tools.NewErrorsListHandler(client),
		// Phase 2
		tools.NewLogsK8sHandler(client),
		tools.NewErrorsGetHandler(client),
		tools.NewLogsServicesHandler(client),
		// Phase 3
		tools.NewLogsSummaryHandler(client),
		// Traces
		tools.NewTraceGetHandler(client),
	})

	s.registerResources(client)
	s.registerPrompts()

	errLogger := log.New(s.errOut, "[mcp-gcp-observability] ", log.LstdFlags)
	stdioServer := server.NewStdioServer(s.mcpServer)
	stdioServer.SetErrorLogger(errLogger)

	if err := stdioServer.Listen(ctx, s.stdin, s.stdout); err != nil {
		return fmt.Errorf("stdio server: %w", err)
	}
	return nil
}

// registerResources adds MCP resources to the server.
func (s *Server) registerResources(client *gcpclient.Client) {
	cfg := client.Config()
	configJSON, err := json.Marshal(map[string]any{
		"default_project":  cfg.DefaultProject,
		"logs_max_limit":   cfg.LogsMaxLimit,
		"errors_max_limit": cfg.ErrorsMaxLimit,
	})
	if err != nil {
		configJSON = []byte(fmt.Sprintf(`{"error": "failed to marshal config: %v"}`, err))
	}

	s.mcpServer.AddResource(
		mcp.NewResource(
			"config://project",
			"Project Configuration",
			mcp.WithResourceDescription("Current GCP project configuration: default project ID and query limits"),
			mcp.WithMIMEType("application/json"),
		),
		func(_ context.Context, _ mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
			return []mcp.ResourceContents{
				mcp.TextResourceContents{
					URI:      "config://project",
					MIMEType: "application/json",
					Text:     string(configJSON),
				},
			}, nil
		},
	)
}

// registerPrompts adds MCP prompts for common observability workflows.
func (s *Server) registerPrompts() {
	s.mcpServer.AddPrompt(mcp.NewPrompt("investigate-errors",
		mcp.WithPromptDescription("Investigate top errors: list error groups, get details for the worst one, and find related logs"),
		mcp.WithArgument("service",
			mcp.ArgumentDescription("Optional service name to filter errors"),
		),
	), func(_ context.Context, request mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		service := request.Params.Arguments["service"]
		msg := "Investigate the top errors in the project:\n" +
			"1. Use errors.list to find the most frequent error groups"
		if service != "" {
			msg += fmt.Sprintf(" (filter by service: %s)", service)
		}
		msg += "\n2. Use errors.get on the top error group to see stack traces and individual events" +
			"\n3. Use logs.query or logs.k8s to find related logs around the same time" +
			"\n4. If trace IDs are available, use logs.by_trace to follow the request flow" +
			"\n5. Summarize the root cause and suggest next steps"
		return &mcp.GetPromptResult{
			Messages: []mcp.PromptMessage{
				{Role: mcp.RoleUser, Content: mcp.TextContent{Text: msg}},
			},
		}, nil
	})

	s.mcpServer.AddPrompt(mcp.NewPrompt("trace-request",
		mcp.WithPromptDescription("Trace a specific HTTP request end-to-end: find it by URL, follow its trace, and analyze spans"),
		mcp.WithArgument("url_pattern",
			mcp.ArgumentDescription("URL pattern to search for (e.g. '/api/users')"),
			mcp.RequiredArgument(),
		),
	), func(_ context.Context, request mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		urlPattern := request.Params.Arguments["url_pattern"]
		msg := fmt.Sprintf("Trace a request matching URL pattern %q:\n", urlPattern) +
			"1. Use logs.find_requests to find matching HTTP requests with their trace IDs\n" +
			"2. Pick the most interesting request (e.g. slowest or with an error status)\n" +
			"3. Use trace.get to see the full span tree and identify slow spans\n" +
			"4. Use logs.by_trace to see all logs associated with that trace\n" +
			"5. Summarize the request flow, highlighting any issues or bottlenecks"
		return &mcp.GetPromptResult{
			Messages: []mcp.PromptMessage{
				{Role: mcp.RoleUser, Content: mcp.TextContent{Text: msg}},
			},
		}, nil
	})

	s.mcpServer.AddPrompt(mcp.NewPrompt("service-health",
		mcp.WithPromptDescription("Check the health of services: discover services, summarize logs, and identify issues"),
	), func(_ context.Context, _ mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		msg := "Check the health of services in the project:\n" +
			"1. Use logs.services to discover all available services\n" +
			"2. Use logs.summary to get an overview of severity distribution and top errors\n" +
			"3. Use errors.list to see the most frequent error groups\n" +
			"4. For any concerning services, use logs.k8s or logs.query to investigate further\n" +
			"5. Provide a health summary with any issues found and recommended actions"
		return &mcp.GetPromptResult{
			Messages: []mcp.PromptMessage{
				{Role: mcp.RoleUser, Content: mcp.TextContent{Text: msg}},
			},
		}, nil
	})
}
