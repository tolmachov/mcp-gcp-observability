package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/oauth2"

	"github.com/tolmachov/mcp-gcp-observability/internal/authsrv"
)

const (
	// userPoolIdleTTL is how long a user's assembly (MCP servers + GCP
	// clients) survives without requests before eviction. Sliding: every
	// request resets it. Evicted users are rebuilt transparently on their
	// next request (the client re-initializes its MCP session).
	userPoolIdleTTL = 30 * time.Minute
	// userPoolMaxUsers caps concurrent per-user assemblies. Each holds a
	// full set of GCP API clients; the cap bounds memory and connections.
	userPoolMaxUsers = 100
	// userPoolJanitorInterval is how often idle entries are collected.
	userPoolJanitorInterval = time.Minute
)

// errPoolFull is returned when the pool is at capacity with no idle entry.
var errPoolFull = errors.New("user pool is full")

// userHandlerBuilder builds the complete per-user HTTP assembly: MCP
// server(s) with tools bound to GCP clients authenticated as the user via ts.
// The returned closer tears the assembly down on eviction.
type userHandlerBuilder func(ctx context.Context, user *authsrv.UserIdentity, ts oauth2.TokenSource) (http.Handler, io.Closer, error)

// swappableTokenSource is an oauth2.TokenSource whose backing source is
// replaced on every request. GCP clients consult the source per RPC, so a
// user presenting a refreshed bearer token transparently re-authenticates
// their long-lived client set.
type swappableTokenSource struct {
	mu sync.Mutex
	ts oauth2.TokenSource
}

func (s *swappableTokenSource) Token() (*oauth2.Token, error) {
	s.mu.Lock()
	ts := s.ts
	s.mu.Unlock()
	tok, err := ts.Token()
	if err != nil {
		return nil, fmt.Errorf("user token source: %w", err)
	}
	return tok, nil
}

func (s *swappableTokenSource) set(ts oauth2.TokenSource) {
	s.mu.Lock()
	s.ts = ts
	s.mu.Unlock()
}

// userEntry is one user's pooled assembly.
//
// Lifecycle: building (ready open) → done (ready closed), where done is
// either ready-to-serve (buildErr nil, handler/closer set) or failed
// (buildErr set; the entry has already been removed from the pool map).
// finish/fail are the only two transitions; both are one-shot.
type userEntry struct {
	ready    chan struct{}
	handler  http.Handler
	closer   io.Closer
	ts       *swappableTokenSource
	buildErr error
	lastUsed atomic.Int64 // unix nanos
	inflight atomic.Int64
}

// finish publishes a successful build. Field writes happen strictly before
// close(ready), which is the happens-before edge waiters rely on.
func (e *userEntry) finish(handler http.Handler, closer io.Closer) {
	e.handler = handler
	e.closer = closer
	close(e.ready)
}

// fail publishes a failed build.
func (e *userEntry) fail(err error) {
	e.buildErr = err
	close(e.ready)
}

// done reports whether the build has completed (successfully or not).
func (e *userEntry) done() bool {
	select {
	case <-e.ready:
		return true
	default:
		return false
	}
}

// evictable reports whether the entry can be torn down: build completed
// successfully and no request is using it. The whole eviction invariant
// lives here; both evictors must go through it.
func (e *userEntry) evictable() bool {
	return e.done() && e.buildErr == nil && e.inflight.Load() == 0
}

// userPool caches one HTTP assembly per authenticated user and target
// project, keyed by the stable Google subject plus the grant's project (a
// user who re-authorizes against a different project gets a separate
// assembly). Builds are singleflighted; failed builds are not cached; idle
// entries are evicted (and their GCP clients closed) by the janitor.
type userPool struct {
	mu      sync.Mutex
	entries map[string]*userEntry
	build   userHandlerBuilder
	// baseCtx is the server-lifetime context builds run on, so canceling one
	// request cannot poison a build other requests will share.
	baseCtx  context.Context
	maxUsers int
	now      func() time.Time
	logger   *slog.Logger
}

func newUserPool(baseCtx context.Context, build userHandlerBuilder, logger *slog.Logger) *userPool {
	return &userPool{
		entries:  map[string]*userEntry{},
		build:    build,
		baseCtx:  baseCtx,
		maxUsers: userPoolMaxUsers,
		now:      time.Now,
		logger:   logger,
	}
}

// ServeHTTP dispatches the request to the caller's assembly, building it on
// first use. It must run behind auth.RequireBearerToken, which is what
// populates the identity in the request context.
func (p *userPool) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	user, okUser := authsrv.Identity(r.Context())
	ts, okTS := authsrv.GoogleTokenSource(r.Context())
	if !okUser || !okTS || user.Subject == "" {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}

	entry, err := p.entryFor(r.Context(), user, ts)
	if entry != nil {
		defer p.release(entry)
	}
	switch {
	case errors.Is(err, errPoolFull):
		p.logger.Warn("user pool at capacity, rejecting request", "user", user.Email, "cap", p.maxUsers)
		w.Header().Set("Retry-After", "60")
		http.Error(w, "server is at capacity, retry later", http.StatusServiceUnavailable)
		return
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// The caller went away while waiting for the build; nothing to send.
		return
	case err != nil:
		p.logger.Error("building user assembly failed", "user", user.Email, "err", err)
		http.Error(w, "failed to initialize GCP clients", http.StatusServiceUnavailable)
		return
	}
	entry.handler.ServeHTTP(w, r)
}

// entryFor returns the caller's entry with inflight already incremented (the
// caller must release a non-nil entry), building the assembly on first use.
// Waiting on a concurrent build is bounded by ctx (the request context): a
// canceled caller stops waiting, while the build itself continues on the
// pool's base context for the next request to reuse.
// poolKey identifies one assembly: subject plus project (the same user may
// hold grants for different projects concurrently).
func poolKey(user *authsrv.UserIdentity) string {
	return user.Subject + "\x00" + user.Project
}

func (p *userPool) entryFor(ctx context.Context, user *authsrv.UserIdentity, ts oauth2.TokenSource) (*userEntry, error) {
	key := poolKey(user)
	p.mu.Lock()
	if e, ok := p.entries[key]; ok {
		e.inflight.Add(1)
		e.lastUsed.Store(p.now().UnixNano())
		p.mu.Unlock()
		select {
		case <-e.ready:
		case <-ctx.Done():
			return e, fmt.Errorf("waiting for user assembly build: %w", ctx.Err())
		}
		if e.buildErr != nil {
			return e, e.buildErr
		}
		e.ts.set(ts)
		return e, nil
	}

	var evicted *userEntry
	var evictedKey string
	if len(p.entries) >= p.maxUsers {
		evictedKey, evicted = p.takeOneEvictableLocked()
		if evicted == nil {
			p.mu.Unlock()
			return nil, errPoolFull
		}
	}
	e := &userEntry{ready: make(chan struct{}), ts: &swappableTokenSource{ts: ts}}
	e.inflight.Add(1)
	e.lastUsed.Store(p.now().UnixNano())
	p.entries[key] = e
	p.mu.Unlock()

	if evicted != nil {
		p.logger.Info("evicted least-recently-used user assembly to make room", "user_sub", evictedKey)
		p.closeEntry(evictedKey, evicted)
	}

	if err := p.runBuild(e, user); err != nil {
		return e, err
	}
	p.logger.Info("built per-user MCP assembly", "user", user.Email, "pool_size", p.size())
	return e, nil
}

// runBuild executes the builder for a freshly inserted entry, converting
// panics into build errors. Whatever happens, the entry's ready channel is
// closed and failed entries are removed from the map — a builder panic must
// not leave waiters blocked forever with the janitor unable to intervene.
func (p *userPool) runBuild(e *userEntry, user *authsrv.UserIdentity) (retErr error) {
	completed := false
	defer func() {
		if completed {
			return
		}
		if r := recover(); r != nil {
			retErr = fmt.Errorf("user assembly build panic: %v", r)
			p.logger.Error("user assembly build panic", "user", user.Email, "panic", r, "stack", string(debug.Stack()))
		} else if retErr == nil {
			retErr = fmt.Errorf("user assembly build aborted")
		}
		e.fail(retErr)
		p.mu.Lock()
		delete(p.entries, poolKey(user))
		p.mu.Unlock()
	}()

	handler, closer, err := p.build(p.baseCtx, user, e.ts)
	if err != nil {
		return err
	}
	e.finish(handler, closer)
	completed = true
	return nil
}

// release decrements the entry's inflight counter.
func (p *userPool) release(e *userEntry) {
	e.inflight.Add(-1)
}

func (p *userPool) size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries)
}

// takeOneEvictableLocked removes and returns the least-recently-used
// evictable entry, or nil when every entry is busy or still building. The
// caller closes it outside the pool lock.
func (p *userPool) takeOneEvictableLocked() (string, *userEntry) {
	var oldestKey string
	var oldest *userEntry
	for k, e := range p.entries {
		if !e.evictable() {
			continue
		}
		if oldest == nil || e.lastUsed.Load() < oldest.lastUsed.Load() {
			oldestKey, oldest = k, e
		}
	}
	if oldest == nil {
		return "", nil
	}
	delete(p.entries, oldestKey)
	return oldestKey, oldest
}

// janitor evicts idle entries until ctx is canceled.
func (p *userPool) janitor(ctx context.Context) {
	t := time.NewTicker(userPoolJanitorInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.evictIdle()
		}
	}
}

// evictIdle removes every evictable entry that has been idle past
// userPoolIdleTTL. In-flight requests (including hanging SSE streams) hold
// inflight > 0 and are never evicted. Teardown (network I/O on gRPC clients)
// runs after the pool lock is released so a slow close cannot stall other
// requests.
func (p *userPool) evictIdle() {
	cutoff := p.now().Add(-userPoolIdleTTL).UnixNano()
	type victim struct {
		key   string
		entry *userEntry
	}
	var victims []victim

	p.mu.Lock()
	for k, e := range p.entries {
		if e.evictable() && e.lastUsed.Load() < cutoff {
			delete(p.entries, k)
			victims = append(victims, victim{k, e})
		}
	}
	p.mu.Unlock()

	for _, v := range victims {
		p.logger.Info("evicted idle user assembly", "user_sub", v.key)
		p.closeEntry(v.key, v.entry)
	}
}

// closeEntry tears down an evicted entry's assembly.
func (p *userPool) closeEntry(key string, e *userEntry) {
	if e.closer == nil {
		return
	}
	if err := e.closer.Close(); err != nil {
		p.logger.Warn("closing evicted user assembly failed", "user_sub", key, "err", err)
	}
}

// Close tears down every pooled assembly. Called after the HTTP server's
// graceful drain; the drain is bounded (see serveHTTP), so requests that
// outlive it — hanging SSE streams at shutdown — are force-closed first and
// may observe closed GCP clients. That is the accepted shutdown trade-off.
func (p *userPool) Close() error {
	p.mu.Lock()
	entries := p.entries
	p.entries = map[string]*userEntry{}
	p.mu.Unlock()

	var errs []error
	for _, e := range entries {
		if e.done() && e.closer != nil {
			errs = append(errs, e.closer.Close())
		}
	}
	return errors.Join(errs...)
}

// multiCloser closes several closers as one, joining errors.
type multiCloser []io.Closer

func (m multiCloser) Close() error {
	var errs []error
	for _, c := range m {
		errs = append(errs, c.Close())
	}
	return errors.Join(errs...)
}
