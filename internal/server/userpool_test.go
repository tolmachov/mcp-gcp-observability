package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"

	"github.com/tolmachov/mcp-gcp-observability/internal/authsrv"
)

// poolTestVerifier returns an auth.TokenVerifier that treats the bearer token
// as "<subject>" and fabricates the TokenInfo the pool consumes, using the
// same constructor as authsrv's real Verifier output.
func poolTestVerifier() auth.TokenVerifier {
	return func(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		return authsrv.NewTokenInfoForTesting(
			token, token+"@example.com", "example.com", "", "ya29."+token, time.Now().Add(time.Hour)), nil
	}
}

// countingBuilder records builds and closes per user.
type countingBuilder struct {
	mu     sync.Mutex
	builds map[string]int
	closes map[string]int
	block  chan struct{} // when non-nil, builds wait on it
	fail   bool
}

func newCountingBuilder() *countingBuilder {
	return &countingBuilder{builds: map[string]int{}, closes: map[string]int{}}
}

type recordingCloser struct{ f func() }

func (r recordingCloser) Close() error { r.f(); return nil }

func (b *countingBuilder) builder() userHandlerBuilder {
	return func(_ context.Context, user *authsrv.UserIdentity, _ oauth2.TokenSource) (http.Handler, io.Closer, error) {
		if b.block != nil {
			<-b.block
		}
		b.mu.Lock()
		b.builds[user.Email]++
		fail := b.fail
		b.mu.Unlock()
		if fail {
			return nil, nil, fmt.Errorf("boom")
		}
		email := user.Email
		h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprintf(w, "hello %s", email)
		})
		return h, recordingCloser{f: func() {
			b.mu.Lock()
			b.closes[email]++
			b.mu.Unlock()
		}}, nil
	}
}

func (b *countingBuilder) buildCount(email string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.builds[email]
}

func (b *countingBuilder) closeCount(email string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closes[email]
}

// newPoolServer wires verifier → pool into an httptest server.
func newPoolServer(t *testing.T, pool *userPool) *httptest.Server {
	t.Helper()
	handler := auth.RequireBearerToken(poolTestVerifier(), nil)(pool)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts
}

func poolGet(t *testing.T, ts *httptest.Server, subject string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL, nil)
	require.NoError(t, err)
	if subject != "" {
		req.Header.Set("Authorization", "Bearer "+subject)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close() //nolint:errcheck // test cleanup
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(body)
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestUserPoolReusesPerUser(t *testing.T) {
	b := newCountingBuilder()
	pool := newUserPool(context.Background(), b.builder(), discardLogger())
	ts := newPoolServer(t, pool)

	for range 5 {
		status, body := poolGet(t, ts, "alice")
		assert.Equal(t, http.StatusOK, status)
		assert.Equal(t, "hello alice@example.com", body)
	}
	status, body := poolGet(t, ts, "bob")
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, "hello bob@example.com", body)

	assert.Equal(t, 1, b.buildCount("alice@example.com"), "same user must reuse the assembly")
	assert.Equal(t, 1, b.buildCount("bob@example.com"), "distinct users must get distinct assemblies")
	assert.Equal(t, 2, pool.size())
}

func TestUserPoolSingleflight(t *testing.T) {
	b := newCountingBuilder()
	b.block = make(chan struct{})
	pool := newUserPool(context.Background(), b.builder(), discardLogger())
	ts := newPoolServer(t, pool)

	const n = 10
	var wg sync.WaitGroup
	var okCount atomic.Int64
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, _ := poolGet(t, ts, "alice")
			if status == http.StatusOK {
				okCount.Add(1)
			}
		}()
	}
	time.Sleep(50 * time.Millisecond) // let requests pile up on the build
	close(b.block)
	wg.Wait()

	assert.Equal(t, int64(n), okCount.Load())
	assert.Equal(t, 1, b.buildCount("alice@example.com"), "concurrent first requests must build once")
}

func TestUserPoolFailedBuildRetries(t *testing.T) {
	b := newCountingBuilder()
	b.fail = true
	pool := newUserPool(context.Background(), b.builder(), discardLogger())
	ts := newPoolServer(t, pool)

	status, _ := poolGet(t, ts, "alice")
	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Equal(t, 0, pool.size(), "failed build must not be cached")

	b.mu.Lock()
	b.fail = false
	b.mu.Unlock()
	status, _ = poolGet(t, ts, "alice")
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, 2, b.buildCount("alice@example.com"))
}

func TestUserPoolMissingIdentity(t *testing.T) {
	b := newCountingBuilder()
	pool := newUserPool(context.Background(), b.builder(), discardLogger())

	// Directly hit the pool without RequireBearerToken: no identity in ctx.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	pool.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, 0, pool.size())
}

func TestUserPoolEvictIdle(t *testing.T) {
	b := newCountingBuilder()
	pool := newUserPool(context.Background(), b.builder(), discardLogger())
	ts := newPoolServer(t, pool)

	status, _ := poolGet(t, ts, "alice")
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, 1, pool.size())

	// Age the entry past the TTL and run one janitor sweep.
	pool.mu.Lock()
	for _, e := range pool.entries {
		e.lastUsed.Store(time.Now().Add(-2 * userPoolIdleTTL).UnixNano())
	}
	pool.mu.Unlock()
	pool.evictIdle()

	assert.Equal(t, 0, pool.size())
	assert.Equal(t, 1, b.closeCount("alice@example.com"), "eviction must close the assembly exactly once")

	// Next request transparently rebuilds.
	status, _ = poolGet(t, ts, "alice")
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, 2, b.buildCount("alice@example.com"))
}

func TestUserPoolInflightBlocksEviction(t *testing.T) {
	b := newCountingBuilder()
	pool := newUserPool(context.Background(), b.builder(), discardLogger())
	ts := newPoolServer(t, pool)

	status, _ := poolGet(t, ts, "alice")
	require.Equal(t, http.StatusOK, status)

	pool.mu.Lock()
	var entry *userEntry
	for _, e := range pool.entries {
		entry = e
		e.lastUsed.Store(time.Now().Add(-2 * userPoolIdleTTL).UnixNano())
	}
	pool.mu.Unlock()
	require.NotNil(t, entry)

	// Simulate a hanging streamable GET holding the entry.
	entry.inflight.Add(1)
	pool.evictIdle()
	assert.Equal(t, 1, pool.size(), "in-flight entry must not be evicted")
	assert.Equal(t, 0, b.closeCount("alice@example.com"))

	entry.inflight.Add(-1)
	pool.evictIdle()
	assert.Equal(t, 0, pool.size())
	assert.Equal(t, 1, b.closeCount("alice@example.com"))
}

func TestUserPoolClose(t *testing.T) {
	b := newCountingBuilder()
	pool := newUserPool(context.Background(), b.builder(), discardLogger())
	ts := newPoolServer(t, pool)

	for _, u := range []string{"alice", "bob", "carol"} {
		status, _ := poolGet(t, ts, u)
		require.Equal(t, http.StatusOK, status)
	}
	require.NoError(t, pool.Close())
	assert.Equal(t, 0, pool.size())
	for _, u := range []string{"alice", "bob", "carol"} {
		assert.Equal(t, 1, b.closeCount(u+"@example.com"))
	}
}

func TestSwappableTokenSource(t *testing.T) {
	s := &swappableTokenSource{ts: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "one"})}
	tok, err := s.Token()
	require.NoError(t, err)
	assert.Equal(t, "one", tok.AccessToken)

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i%2 == 0 {
				s.set(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "two"}))
			} else {
				_, _ = s.Token()
			}
		}()
	}
	wg.Wait()
	tok, err = s.Token()
	require.NoError(t, err)
	assert.Equal(t, "two", tok.AccessToken)
}

func TestUserPoolOverflowEvictsLRU(t *testing.T) {
	b := newCountingBuilder()
	pool := newUserPool(context.Background(), b.builder(), discardLogger())
	pool.maxUsers = 2
	ts := newPoolServer(t, pool)

	base := time.Now()
	pool.now = func() time.Time { return base }
	require.Equal(t, http.StatusOK, first(poolGet(t, ts, "alice")))
	pool.now = func() time.Time { return base.Add(time.Second) }
	require.Equal(t, http.StatusOK, first(poolGet(t, ts, "bob")))
	require.Equal(t, 2, pool.size())

	// The third user displaces the least-recently-used entry (alice).
	pool.now = func() time.Time { return base.Add(2 * time.Second) }
	require.Equal(t, http.StatusOK, first(poolGet(t, ts, "carol")))
	assert.Equal(t, 2, pool.size())
	assert.Equal(t, 1, b.closeCount("alice@example.com"), "LRU entry must be closed on displacement")
	assert.Equal(t, 0, b.closeCount("bob@example.com"))

	// Alice comes back: transparently rebuilt.
	require.Equal(t, http.StatusOK, first(poolGet(t, ts, "alice")))
	assert.Equal(t, 2, b.buildCount("alice@example.com"))
}

func first(status int, _ string) int { return status }

func TestUserPoolOverflowAllBusyYields503(t *testing.T) {
	b := newCountingBuilder()
	b.block = make(chan struct{})
	pool := newUserPool(context.Background(), b.builder(), discardLogger())
	pool.maxUsers = 1
	ts := newPoolServer(t, pool)

	// alice's build hangs, holding her entry non-evictable (inflight > 0).
	aliceDone := make(chan struct{})
	go func() {
		defer close(aliceDone)
		status, _ := poolGet(t, ts, "alice")
		assert.Equal(t, http.StatusOK, status)
	}()
	require.Eventually(t, func() bool { return pool.size() == 1 }, time.Second, time.Millisecond)

	status, _ := poolGet(t, ts, "bob")
	assert.Equal(t, http.StatusServiceUnavailable, status)

	close(b.block)
	<-aliceDone
}

func TestUserPoolBuilderPanicDoesNotWedgeUser(t *testing.T) {
	calls := 0
	builder := func(_ context.Context, user *authsrv.UserIdentity, _ oauth2.TokenSource) (http.Handler, io.Closer, error) {
		calls++
		if calls == 1 {
			panic("builder exploded")
		}
		email := user.Email
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprintf(w, "hello %s", email)
		}), recordingCloser{f: func() {}}, nil
	}
	pool := newUserPool(context.Background(), builder, discardLogger())
	ts := newPoolServer(t, pool)

	// The panicking build turns into a 503, not a hang, and is not cached.
	status, _ := poolGet(t, ts, "alice")
	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Equal(t, 0, pool.size())

	// The next request rebuilds successfully.
	status, body := poolGet(t, ts, "alice")
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, "hello alice@example.com", body)
}

func TestUserPoolWaiterHonorsRequestContext(t *testing.T) {
	b := newCountingBuilder()
	b.block = make(chan struct{})
	defer close(b.block)
	pool := newUserPool(context.Background(), b.builder(), discardLogger())

	// First caller starts the (blocked) build.
	firstStarted := make(chan struct{})
	go func() {
		close(firstStarted)
		_, _ = pool.entryFor(context.Background(),
			&authsrv.UserIdentity{Subject: "s", Email: "e"},
			oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "x"}))
	}()
	<-firstStarted
	require.Eventually(t, func() bool { return pool.size() == 1 }, time.Second, time.Millisecond)

	// Second caller waits on the same entry but gives up with its context.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	entry, err := pool.entryFor(ctx,
		&authsrv.UserIdentity{Subject: "s", Email: "e"},
		oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "x"}))
	require.ErrorIs(t, err, context.Canceled)
	require.NotNil(t, entry)
	pool.release(entry)
}

// TestUserPoolSeparatesProjects: the same user authorized against two
// different projects gets two independent assemblies (project-choice mode).
func TestUserPoolSeparatesProjects(t *testing.T) {
	b := newCountingBuilder()
	pool := newUserPool(context.Background(), b.builder(), discardLogger())
	verifier := func(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		sub, proj, _ := strings.Cut(token, "/")
		return authsrv.NewTokenInfoForTesting(
			sub, sub+"@example.com", "example.com", proj, "ya29."+token, time.Now().Add(time.Hour)), nil
	}
	handler := auth.RequireBearerToken(verifier, nil)(pool)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	for _, tok := range []string{"alice/proj-a", "alice/proj-b", "alice/proj-a", "alice/proj-b"} {
		status, _ := poolGet(t, ts, tok)
		require.Equal(t, http.StatusOK, status)
	}
	// One build per (user, project), not per request and not one shared.
	assert.Equal(t, 2, b.buildCount("alice@example.com"))
	assert.Equal(t, 2, pool.size())
}
