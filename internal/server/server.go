package server

import (
	"context"
	"fmt"
	"io"
	"log"

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
		tools.NewLogsFindRequestsHandler(client),
		tools.NewErrorsListHandler(client),
		// Phase 2
		tools.NewLogsK8sHandler(client),
		tools.NewErrorsGetHandler(client),
		tools.NewLogsServicesHandler(client),
		// Phase 3
		tools.NewLogsSummaryHandler(client),
	})

	errLogger := log.New(s.errOut, "[mcp-gcp-observability] ", log.LstdFlags)
	stdioServer := server.NewStdioServer(s.mcpServer)
	stdioServer.SetErrorLogger(errLogger)

	if err := stdioServer.Listen(ctx, s.stdin, s.stdout); err != nil {
		return fmt.Errorf("stdio server: %w", err)
	}
	return nil
}
