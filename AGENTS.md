# AGENTS.md

Instructions for coding agents working in this repository.

## Project

Go 1.26 MCP server for Google Cloud observability: Cloud Logging, Error Reporting,
Cloud Trace, Cloud Monitoring and Cloud Profiler. `main.go` loads `.env` (godotenv)
and runs `internal.New(...).Run` — a urfave/cli v3 app with subcommands `run`
(start the server) and `validate-registry` (check a metrics registry overlay).

`README.md` is the source of truth for the tool list, configuration and deployment.
`requirements.md` is the original spec and is outdated; `TODO.md` and
`experimental-ext-variants.md` are planning notes.

## Layout

- `internal/` — CLI app (`app.go`), flags (`flags.go`), `internal.Version` (set via ldflags)
- `internal/server` — MCP server, transports, prompts, resources, completion, per-user client pool, variants (`full` / `compact` / `monitoring`)
- `internal/tools` — one file per MCP tool (`logs_*`, `errors_*`, `trace_*`, `metrics_*`, `profiler_*`), input schemas (`inputs.go`, `schema.go`), HTML charts
- `internal/gcpdata` — backends that call the GCP APIs (logs, errors, traces, metrics, profiler + profile cache)
- `internal/metrics` — semantic metrics registry (`default_registry.yaml`), baselines, anomaly classification, aggregation
- `internal/authsrv` — OAuth authorization server for the shared deployment (Google login, DCR, sealed tokens, rate limiting, project access check)
- `internal/gcpclient` — GCP client configuration and construction
- `test/` — integration tests behind the `integration` build tag (need real GCP credentials)
- `deploy/` — Cloud Build config and `cloudrun.env.example`

## Configuration

Every flag in `internal/flags.go` also reads an env var (see `.env.example`).
`GCP_DEFAULT_PROJECT` is required. Default mode is local `stdio` with no auth;
the shared deployment uses `MCP_TRANSPORT=http` + `MCP_AUTH=google` (`AUTH_*` vars).
`MCP_VARIANT` selects the tool set, `METRICS_REGISTRY_FILE` adds a registry overlay.

## Commands

```bash
make build             # build the binary
make test              # go test ./...
make test-race         # go test -race ./...
make lint              # golangci-lint v2 (.golangci.yml)
make fmt               # gofmt + goimports via golangci-lint
make test-integration  # real GCP project + credentials required
```

CI (`.github/workflows/ci.yml`) runs `go build`, `go vet`, `go test ./...` and
golangci-lint v2.12.2. A change is done only when all of them pass with zero lint issues.

## Conventions

- Wrap errors with context: `fmt.Errorf("...: %w", err)`. Linters include errorlint, wrapcheck, gosec, unparam.
- Adding a tool: new file in `internal/tools` with a `Register*` function, register it in
  `registerAllTools` (`internal/server/variants.go`) and, if it belongs to the monitoring
  variant, in `tools.RegisterCore`. Update `allToolsCount` / `CoreToolsCount` (pinned by
  tests) and the tool tables in `README.md`.
- Never commit `.env` or `deploy/cloudrun.env`.

## Deployment

The shared server runs on Cloud Run. `make deploy` builds and pushes the image and rolls
out a new revision; non-secret settings come from `deploy/cloudrun.env`, secrets from
Secret Manager. One-time setup is in the README. Deploy only when explicitly asked.
