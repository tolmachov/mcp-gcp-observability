# Observability architecture deepening implementation plan

> **For agentic workers:** Use superpowers:subagent-driven-development. Steps use checkbox syntax for tracking.

**Goal:** Implement all three candidates approved from the architecture report, preserving public behavior.

**Architecture:** One production HTTP entry module owns request policy. The Profiler navigation module owns fetch/diff/analysis/result assembly. An internal grant lifecycle module owns grant interpretation and transitions while the existing persistence adapters retain atomicity.

**Tech Stack:** Existing Go 1.27.1, MCP SDK, Google SDK clients, Firestore and in-process test adapters; no new dependencies.

**Spec:** Architecture report `architecture-review-20261020-171057.html` in the OS temp directory, approved in chat with “давай все исправим”. The constraints and tasks below capture its binding requirements.

## Global constraints

- Preserve public MCP inputs, outputs, tool membership, defaults, bounded results and error guidance.
- Preserve OAuth wire/token/storage formats, domain and pinned-project enforcement, expiry/error precedence and safe diagnostic reason classes.
- Keep Firestore redemption/rotation/replay decisions atomic; preserve absolute grant expiry and locally committed revocation before upstream Google revocation.
- A replayed refresh revokes the family. A forged or rotated refresh submitted to revoke is a no-op. Revoked grants retain possession evidence for upstream retry.
- Keep source-only bounded profile caches, request-local parsed graphs, sequential diff fetches, progress heartbeats and scan/concurrency budgets.
- Use existing worktree/branch; do not alter `.serena/`, secrets or deployment settings. No commit, push, PR or deployment is part of this request.
- Prefix shell commands with `rtk`; `rtk proxy` runs commands without filtering.

## Review focus

- HTTP policy ordering: diagnostic observation encloses origin/body rejections, OAuth bypasses apply only to intended routes, SSE still streams.
- Profiler source/diff navigation: value-type compatibility, invalid indexes, ambiguous functions, zero totals and node limits produce existing results/errors.
- Concurrent refresh/redeem: one winner plus replay revocation, with absolute expiry unchanged in both adapters.
- Revoke possession and upstream retry: a forged token never kills a family, a valid revoked token can retry Google revocation.
- Store corruption/outage and bearer expiry: preserve invalid-credential versus transient-store-error distinction and diagnostic precedence.

### Task 1: Production HTTP entry policy

**Files:** `internal/server/transport.go`, `internal/server/transport_auth_test.go`, focused HTTP policy tests.

**Interface:** An unexported assembled HTTP handler function, `httpEntryHandler(logger *slog.Logger, handler http.Handler) http.Handler`, owns diagnostics, route policy, cross-origin protection, body limit/observation and the existing OAuth bypass list. Both `serveHTTP` and wiring tests call it. Socket shutdown remains separate.

- [x] Add behavior coverage through the new entry interface; prove removing body observation or origin protection breaks relevant assertions. Existing wiring tests must not rebuild middleware order.
- [x] Implement the single composition, migrate both callers, remove redundant policy parameters if all callers use the same HTTP mode.
- [x] Run `rtk go test ./internal/server ./internal/httpdiag`; self-review scope and invariants. Write a short report with commands/results and changed paths to the requested scratch report file.

### Task 2: Deep profile navigation

**Files:** `internal/gcpdata/backends.go`, new navigation implementation/tests in `internal/gcpdata/`, `internal/tools/profiler_{top,peek,flamegraph}.go`, `internal/tools/tools.go`, affected test adapters.

**Interface:** Replace parsed-graph operations on `ProfilerQuerier` with `TopProfile`, `PeekProfile`, `FlamegraphProfile`, each taking context plus a named operation params struct and returning its existing domain result plus error. Params include project, current/base IDs and the existing operation inputs. Keep low-level parsed-graph operations private to `gcpdata` where possible; existing pure algorithm tests remain. Do not retain unused compatibility methods on the consumer interface.

- [x] Write navigation behavior tests first against the real querier using existing in-process Export adapter and compressed profiles: source and current-minus-base, bad index, absent/ambiguous function, truncation and zero-total bounded flamegraph.
- [x] Observe the missing new interface fail, implement fetch/diff/analyze/assemble inside the navigation module, preserving exact result fields and analysis/fetch error distinctions.
- [x] Migrate tool callers to transport/presentation only; preserve progress across long fetches and existing guidance. Update all test adapters, remove graph leaks from tools and server test dependencies.
- [x] Run `rtk go test ./internal/gcpdata ./internal/tools ./internal/server`; write task report with red/green evidence.

### Task 3: Grant lifecycle ownership

**Files:** new `internal/authsrv/grants.go` and behavior tests, `token.go`, `verifier.go`, `revoke.go`, `store.go`, `firestore_store.go` as needed.

**Interface:** A concrete internal grant lifecycle module groups code-to-family issuance, active-family interpretation, refresh preparation/rotation and possession-checked local revocation. It uses the existing `oauthStateStore` seam and sealer. HTTP callers receive domain outcomes or classified failures, not raw grant records/status/hash/generation/expiry instructions. Keep upstream refresh/project admission/revocation and HTTP formatting in their current callers. Prefer concrete methods over a new interface or generic workflow engine. Time and store substitutions in existing tests must still work.

- [x] Add or reuse behavior tests through HTTP/middleware and real persistence adapters; add focused lifecycle tests where needed for the new interface. Show a meaningful invariant mutation fails, rather than treating compilation alone as sufficient.
- [x] Move lifecycle decisions and record construction out of token/verifier/revoke handlers. Preserve distinct refresh/revoke possession policy, corrupt/missing/outage responses, store-first bearer expiry precedence, absolute TTL, upstream refresh-token replacement, generation increments and sealed claim continuity.
- [x] Consolidate duplicated grant transition rules across memory and Firestore only where both can invoke the same decision inside their existing atomic operation. Do not move checks outside transactions or dilute adapter tests.
- [x] For refresh-token revocation, recheck possession atomically with the revoke write, so a rotation between lookup and revoke makes the old token a no-op. Add a deterministic lookup/rotate/revoke regression and cover both adapters; preserve current-secret retry on revoked/expired families.
- [x] Run `rtk go test ./internal/authsrv ./internal/server`, race tests and Firestore-emulator tests if local tooling supports them; report exact skipped external verification.

### Final verification

- [x] Add a concise domain glossary for grant family and source profile; no generic architecture terms or implementation details.
- [x] Review each task independently, resolve material findings, then run build, vet, full tests, full race tests and golangci-lint with zero issues.
- [x] Conduct fresh whole-change review of the working-tree diff; preserve unrelated work and leave the verified changes uncommitted for the user.
