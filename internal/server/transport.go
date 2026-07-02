package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/modelcontextprotocol/experimental-ext-variants/go/sdk/variants"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// nopWriteCloser wraps an io.Writer with a no-op Close method.
type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// mcpRunner is satisfied by both *mcp.Server and *variants.Server, which share
// the same Run(ctx, mcp.Transport) error signature.
type mcpRunner interface {
	Run(ctx context.Context, t mcp.Transport) error
}

// runStdio starts a stdio transport for any MCP runner (plain server or variants server).
func (s *Server) runStdio(ctx context.Context, runner mcpRunner) error {
	s.logger.Info("Starting stdio server")
	if err := runner.Run(ctx, &mcp.IOTransport{
		Reader: io.NopCloser(s.stdin),
		Writer: nopWriteCloser{s.stdout},
	}); err != nil {
		return fmt.Errorf("stdio server: %w", err)
	}
	return nil
}

// serveHTTP runs an http.Server with graceful shutdown on ctx cancellation.
// Extracted to avoid duplication between runHTTP and runMCPHTTP.
func (s *Server) serveHTTP(ctx context.Context, handler http.Handler, addr string) error {
	s.logger.Info("Starting streamable HTTP server", "addr", addr)
	// go-sdk v1.6.0 disabled built-in cross-origin protection by default
	// (previously on, now gated behind the enableoriginverification MCPGODEBUG
	// flag until v1.8.0). Restore it explicitly via the stdlib middleware so the
	// HTTP transport is not exposed to DNS-rebinding / cross-origin attacks.
	// Re-evaluate this when upgrading go-sdk past v1.8.0, where the built-in
	// protection returns and this middleware may become redundant.
	srv := &http.Server{
		Addr:    addr,
		Handler: http.NewCrossOriginProtection().Handler(handler),
		// Bound the header-read phase to blunt Slowloris-style slow-header attacks.
		ReadHeaderTimeout: 10 * time.Second,
	}
	shutdownDone := make(chan error, 1)
	serverExited := make(chan struct{})
	// G118: the shutdown goroutine deliberately detaches from ctx (see below);
	// it is a server-lifecycle goroutine, not a request-scoped one.
	go func() { //nolint:gosec // G118: intentional detached shutdown context
		select {
		case <-ctx.Done():
			// Shutdown deliberately starts from a fresh Background context: ctx
			// is already canceled here, so reusing it would abort the graceful
			// drain immediately.
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			shutdownDone <- srv.Shutdown(shutdownCtx)
		case <-serverExited:
			// Server exited early before ctx.Done(); send nil to unblock receiver.
			shutdownDone <- nil
		}
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		close(serverExited)
		return fmt.Errorf("http server: %w", err)
	}
	close(serverExited)
	if shutdownErr := <-shutdownDone; shutdownErr != nil {
		s.logger.Error("HTTP server shutdown failed", "err", shutdownErr)
		return fmt.Errorf("http server shutdown: %w", shutdownErr)
	}
	return nil
}

// runHTTP starts a streamable HTTP server for the variants.Server.
// Closes vs on return; the deferred recover wraps the whole method, so
// panics from variants.NewStreamableHTTPHandler or HTTP setup are converted
// to errors with a logged stack trace.
func (s *Server) runHTTP(ctx context.Context, vs *variants.Server, addr string) (retErr error) {
	defer func() {
		if err := vs.Close(); err != nil {
			s.logger.Warn("failed to close variants server", "err", err)
		}
	}()
	defer func() {
		if r := recover(); r != nil {
			stack := debug.Stack()
			s.logger.Error("variants HTTP init panic", "panic", r, "stack", string(stack))
			retErr = fmt.Errorf("variants HTTP init panic: %v", r)
		}
	}()
	return s.serveHTTP(ctx, variants.NewStreamableHTTPHandler(vs, nil), addr)
}

// runMCPHTTP serves a single *mcp.Server over streamable HTTP (used when
// the variants protocol is bypassed via --variant). The deferred recover
// mirrors runHTTP for symmetry — same panic risk surface.
func (s *Server) runMCPHTTP(ctx context.Context, srv *mcp.Server, addr string) (retErr error) {
	defer func() {
		if r := recover(); r != nil {
			stack := debug.Stack()
			s.logger.Error("MCP HTTP init panic", "panic", r, "stack", string(stack))
			retErr = fmt.Errorf("MCP HTTP init panic: %v", r)
		}
	}()
	handler := mcp.NewStreamableHTTPHandler(
		func(_ *http.Request) *mcp.Server { return srv },
		nil,
	)
	return s.serveHTTP(ctx, handler, addr)
}
