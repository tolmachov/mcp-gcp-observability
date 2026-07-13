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
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/authsrv"
	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
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

// withCrossOriginProtection wraps handler in the stdlib cross-origin
// protection, exempting the given path patterns.
//
// go-sdk v1.6.0 disabled built-in cross-origin protection by default
// (previously on, now gated behind the enableoriginverification MCPGODEBUG
// flag until v1.8.0). Restore it explicitly via the stdlib middleware so the
// HTTP transport is not exposed to DNS-rebinding / cross-origin attacks.
// Re-evaluate this when upgrading go-sdk past v1.8.0, where the built-in
// protection returns and this middleware may become redundant.
func withCrossOriginProtection(handler http.Handler, bypass ...string) http.Handler {
	protection := http.NewCrossOriginProtection()
	for _, pattern := range bypass {
		protection.AddInsecureBypassPattern(pattern)
	}
	return protection.Handler(handler)
}

// serveHTTP runs an http.Server with graceful shutdown on ctx cancellation.
// Extracted to avoid duplication between the HTTP run modes. corsBypass
// patterns are exempted from cross-origin protection (used for OAuth
// endpoints that legitimately receive cross-origin form POSTs).
//
// The graceful drain is bounded at 5 seconds. MCP streamable clients hold
// hanging SSE GETs that never finish on their own, so hitting the bound is
// the NORMAL shutdown path with any connected client — it is logged and the
// remaining connections are force-closed, not reported as an error.
func (s *Server) serveHTTP(ctx context.Context, handler http.Handler, addr string, corsBypass ...string) error {
	s.logger.Info("Starting streamable HTTP server", "addr", addr)
	srv := &http.Server{
		Addr:    addr,
		Handler: withCrossOriginProtection(handler, corsBypass...),
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
			err := srv.Shutdown(shutdownCtx)
			if errors.Is(err, context.DeadlineExceeded) {
				// Expected with connected streamable clients (see doc
				// comment); force-close the stragglers.
				s.logger.Info("graceful drain timed out (long-lived streams), force-closing connections")
				err = srv.Close()
			}
			shutdownDone <- err
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

// authCORSBypassPaths are the OAuth endpoints exempted from cross-origin
// protection: they receive legitimate cross-origin POSTs (Electron and
// browser-based MCP clients send an Origin header on token exchange). The
// MCP endpoint stays protected — it is additionally guarded by the bearer
// token.
var authCORSBypassPaths = []string{"/token", "/register", "/revoke"}

// buildAuthMux assembles the authenticated HTTP surface: the OAuth endpoints
// (unauthenticated by nature) plus the MCP handler behind RequireBearerToken.
// Extracted from runHTTPWithAuth so tests can drive the exact production
// wiring.
func buildAuthMux(as *authsrv.AuthServer, issuerURL string, mcpHandler http.Handler) *http.ServeMux {
	mux := http.NewServeMux()
	as.Routes(mux)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	requireBearer := auth.RequireBearerToken(as.Verifier(), &auth.RequireBearerTokenOptions{
		ResourceMetadataURL: issuerURL + authsrv.ProtectedResourceMetadataPath,
	})
	mux.Handle("/", requireBearer(mcpHandler))
	return mux
}

// runHTTPWithAuth serves the MCP endpoint behind the embedded OAuth
// authorization server. Every authenticated user gets their own MCP assembly
// (from the user pool) whose GCP clients run under the user's own Google
// token, so IAM is enforced per user. The auth endpoints themselves are
// mounted unauthenticated; everything else requires a bearer token.
func (s *Server) runHTTPWithAuth(ctx context.Context, reg *metrics.Registry, opts RunOptions) (retErr error) {
	defer func() {
		if r := recover(); r != nil {
			stack := debug.Stack()
			s.logger.Error("auth HTTP init panic", "panic", r, "stack", string(stack))
			retErr = fmt.Errorf("auth HTTP init panic: %v", r)
		}
	}()

	as, err := authsrv.New(opts.Auth, s.logger)
	if err != nil {
		return fmt.Errorf("building auth server: %w", err)
	}

	pool := newUserPool(ctx, s.userAssemblyBuilder(reg, opts.VariantID), s.logger)
	defer func() {
		if closeErr := pool.Close(); closeErr != nil {
			s.logger.Warn("failed to close user pool", "err", closeErr)
		}
	}()
	go pool.janitor(ctx)

	s.logger.Info("Starting with per-user authentication", "issuer", opts.Auth.IssuerURL)
	mux := buildAuthMux(as, opts.Auth.IssuerURL, pool)
	return s.serveHTTP(ctx, mux, opts.HTTPAddr, authCORSBypassPaths...)
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
