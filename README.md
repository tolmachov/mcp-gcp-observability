<div align="center">

# MCP GCP Observability

**MCP server for querying Google Cloud Logging, Error Reporting, Cloud Trace, Cloud Monitoring, and Cloud Profiler without the web UI.**

[![MCP Server](https://badge.mcpx.dev?type=server 'MCP Server')](https://github.com/punkpeye/awesome-mcp-servers)
[![Go Version](https://img.shields.io/github/go-mod/go-version/tolmachov/mcp-gcp-observability)](https://go.dev/)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)
[![Go Report Card](https://goreportcard.com/badge/github.com/tolmachov/mcp-gcp-observability)](https://goreportcard.com/report/github.com/tolmachov/mcp-gcp-observability)
[![mcp-gcp-observability MCP server](https://glama.ai/mcp/servers/tolmachov/mcp-gcp-observability/badges/score.svg)](https://glama.ai/mcp/servers/tolmachov/mcp-gcp-observability)

[![mcp-gcp-observability MCP server](https://glama.ai/mcp/servers/tolmachov/mcp-gcp-observability/badges/card.svg)](https://glama.ai/mcp/servers/tolmachov/mcp-gcp-observability)

</div>

## Features

- **Cloud Logging** — query with full filter syntax, Kubernetes-aware queries, log summaries and service discovery
- **Error Reporting** — grouped errors sorted by count, individual events with stack traces
- **Cloud Trace** — span trees, latency analysis, trace-based log correlation
- **Cloud Monitoring** — metric discovery, semantic snapshots with baseline comparison, anomaly classification, contributor drill-down, and arbitrary window comparison
- **Cloud Profiler** — profile listing, hotspot analysis (top/peek), call graph visualization (flamegraph), profile comparison, and performance trend tracking

## Prerequisites

- Go 1.26+
- GCP project with the following APIs enabled:
  - Cloud Logging
  - Error Reporting
  - Cloud Trace
  - Cloud Monitoring
  - Cloud Profiler
- Application Default Credentials configured:
  ```bash
  gcloud auth application-default login
  ```
- Required IAM permissions:
  - `logging.logEntries.list`
  - `errorreporting.groupMetadata.list`
  - `errorreporting.events.list`
  - `cloudtrace.traces.get`
  - `monitoring.timeSeries.list`
  - `monitoring.metricDescriptors.list`
  - `cloudprofiler.profiles.list`

## Installation

```bash
go install github.com/tolmachov/mcp-gcp-observability@latest
```

Or build from source:

```bash
git clone https://github.com/tolmachov/mcp-gcp-observability.git
cd mcp-gcp-observability
make build
```

## Setup

1. Copy `.env.example` to `.env` and set your project ID:
   ```bash
   cp .env.example .env
   ```

2. Configure your MCP client:

**Claude Desktop** (`claude_desktop_config.json`):
```json
{
  "mcpServers": {
    "gcp-observability": {
      "command": "/path/to/mcp-gcp-observability",
      "args": ["run"],
      "env": {
        "GCP_DEFAULT_PROJECT": "your-project-id"
      }
    }
  }
}
```

**Claude Code** (`.claude/settings.json`):
```json
{
  "mcpServers": {
    "gcp-observability": {
      "command": "/path/to/mcp-gcp-observability",
      "args": ["run"],
      "env": {
        "GCP_DEFAULT_PROJECT": "your-project-id"
      }
    }
  }
}
```

## Available Tools

### Logs

| Tool | Description |
|------|-------------|
| `logs_query` | Execute arbitrary Cloud Logging queries with filter syntax |
| `logs_k8s` | Query Kubernetes container logs with convenient filters |
| `logs_by_trace` | Find all logs associated with a trace ID |
| `logs_by_request_id` | Find all logs associated with a request ID |
| `logs_find_requests` | Discover HTTP requests by URL pattern, returns trace/request IDs |
| `logs_services` | Discover available services and resources in the project |
| `logs_summary` | Aggregated log statistics: severity distribution, top services, top errors |

### Error Reporting

| Tool | Description |
|------|-------------|
| `errors_list` | List error groups sorted by count |
| `errors_get` | Get error group details with individual events and stack traces |
| `errors_trends` | Classify error groups as new/growing/shrinking/disappeared over a window |

### Tracing

| Tool | Description |
|------|-------------|
| `trace_list` | Search traces by span name, latency, or time range |
| `trace_get` | Get trace details with complete span tree by trace ID |
| `trace_find_from_logs` | Discover traces by scanning logs matching a filter |

### Metrics

| Tool | Description |
|------|-------------|
| `metrics_list` | Discover available metrics from Cloud Monitoring |
| `metrics_snapshot` | Semantic snapshot with baseline comparison, trend detection, and anomaly classification |
| `metrics_top_contributors` | Break down metric by label dimension to find top contributors to an anomaly |
| `metrics_related` | Check all related metrics for correlated anomalies |
| `metrics_compare` | Compare two arbitrary time windows (before/after deploy, incident diff) |

### Profiling

| Tool | Description |
|------|-------------|
| `profiler_list` | List available Cloud Profiler profiles with metadata |
| `profiler_top` | Show top functions ranked by resource consumption (pprof top) |
| `profiler_peek` | Show callers and callees of a specific function (pprof peek) |
| `profiler_flamegraph` | Get bounded subtree of the call graph (flamegraph view) |
| `profiler_compare` | Compare two profiles to find regressions; returns diff_id |
| `profiler_trends` | Track how function costs change over time across multiple profiles |

## Recommended Workflow

### Logs & Errors

1. `logs_services` — discover available services
2. `logs_summary` — get severity distribution, top errors, top services
3. `errors_list` — list error groups sorted by count
4. `logs_query` or `logs_k8s` — investigate specific logs with filters
5. `logs_by_trace` — follow a single request across services
6. `trace_list` — search traces by span name, latency, or time range
7. `trace_get` — get detailed span tree for latency analysis

### Metrics

1. `metrics_list` — discover available metrics, filter by kind
2. `metrics_snapshot` — get semantic snapshot with baseline comparison
3. `metrics_top_contributors` — drill down by dimension to find root cause
4. `metrics_related` — check correlated metrics for broader context
5. `metrics_compare` — compare before/after windows for deploy or incident analysis

### Profiling Analysis

1. `profiler_list` — discover available profiles
2. `profiler_top` — find top functions by resource consumption
3. `profiler_peek` — understand a hotspot's callers and callees
4. `profiler_flamegraph` — view bounded subtree of the call graph
5. `profiler_compare` — A/B comparison (use diff_id with top/peek/flamegraph)
6. `profiler_trends` — track how function costs change over time across multiple profiles

## Built-in Prompts

The server provides pre-built investigation workflows:

| Prompt | Description |
|--------|-------------|
| `investigate-errors` | Investigate top errors, get details, find related logs |
| `trace-request` | Trace HTTP request end-to-end: find by URL, follow trace, analyze spans |
| `investigate-metrics` | Metric anomaly investigation: discover, snapshot, drill down, check related |
| `service-health` | Check health of services: discover, summarize logs, identify issues |
| `investigate-profile` | Investigate performance hotspots using Cloud Profiler |
| `generate-metrics-registry` | Scan codebase and auto-generate metrics registry overlay YAML |

## Configuration

| Flag | Env Var | Default | Description |
|------|---------|---------|-------------|
| `--gcp-default-project` | `GCP_DEFAULT_PROJECT` | (required) | Default GCP project ID |
| `--logs-max-limit` | `LOGS_MAX_LIMIT` | `1000` | Maximum log entries per request |
| `--errors-max-limit` | `ERRORS_MAX_LIMIT` | `100` | Maximum error groups per request |
| `--dns-server` | `DNS_SERVER` | (none) | Custom DNS server for GCP API resolution |
| `--metrics-registry` | `METRICS_REGISTRY_FILE` | (none) | Path to metrics semantic registry YAML file |
| `--transport` | `MCP_TRANSPORT` | `stdio` | Transport mode: `stdio` (default) or `http` |
| `--http-addr` | `MCP_HTTP_ADDR` | `:8080` | HTTP listen address (only used with `--transport http`) |
| `--auth` | `MCP_AUTH` | `none` | HTTP authentication: `none` or `google` (embedded OAuth, see below) |
| `--auth-issuer-url` | `AUTH_ISSUER_URL` | (none) | Public base URL of this service — https, or http on localhost for dev (required for `--auth google`) |
| `--auth-google-client-id` | `AUTH_GOOGLE_CLIENT_ID` | (none) | Google OAuth web client ID (required for `--auth google`) |
| `--auth-google-client-secret` | `AUTH_GOOGLE_CLIENT_SECRET` | (none) | Google OAuth web client secret (required for `--auth google`) |
| `--auth-allowed-domains` | `AUTH_ALLOWED_DOMAINS` | (none) | Workspace domains allowed to log in, comma-separated (this or `--auth-require-project-access` is required for `--auth google`) |
| `--auth-require-project-access` | `AUTH_REQUIRE_PROJECT_ACCESS` | (none) | GCP project ID: only accounts with IAM access to it may log in — for teams without a Workspace domain |
| `--auth-token-key` | `AUTH_TOKEN_KEY` | (none) | Base64 32-byte token encryption keys, comma-separated; first encrypts, all decrypt (required for `--auth google`) |
| `--auth-allowed-redirects` | `AUTH_ALLOWED_REDIRECTS` | (none) | Extra exact-match OAuth redirect URIs (loopback and claude.ai/claude.com are built in) |
| `--auth-google-scopes` | `AUTH_GOOGLE_SCOPES` | openid, email, cloud-platform | Google OAuth scopes requested at login (Error Reporting/Profiler accept no narrower scope) |
| `--auth-skip-consent` | `AUTH_SKIP_CONSENT` | `false` | Skip the consent page (development only) |

### HTTP Transport

For remote deployments or shared access, use the streamable HTTP transport:

```bash
mcp-gcp-observability run --transport http --http-addr :8080
```

**Security:** With `--auth none` (the default) the HTTP transport has no authentication — place it behind an authenticating reverse proxy or network-level access controls. For a shared multi-user deployment use `--auth google` (next section).

## Shared server on Cloud Run (`--auth google`)

Instead of everyone running a local copy, deploy one shared instance. The server embeds a full OAuth 2.1 authorization server (RFC 8414 metadata, RFC 7591 Dynamic Client Registration, PKCE, refresh tokens) with Google Workspace as the identity provider:

- Employees add the URL to their MCP client; the browser opens a Google login.
- Every GCP API call runs under **the user's own Google token** (scope `cloud-platform` — Error Reporting and Profiler accept no narrower one), so personal IAM permissions apply — no shared service-account identity for data access.
- **Project selection**: with `GCP_DEFAULT_PROJECT` set, the server is pinned to that project and logging in requires IAM access to it. With it omitted, each user types a GCP project ID on the consent page; the server verifies their access to it (`testIamPermissions` with their own token) and binds the session — and its tools — to that project.
- All issued tokens are stateless encrypted blobs (AES-256-GCM); no database.

### Connecting (for users)

```bash
# Claude Code
claude mcp add --transport http gcp-obs https://<service-url>/
```

For claude.ai / Claude Desktop: **Settings → Connectors → Add custom connector** with the same URL. Cursor and other MCP clients: add a remote MCP server with the URL; the OAuth flow starts automatically.

Each user needs read-only IAM roles on the observability project: `roles/logging.viewer`, `roles/monitoring.viewer`, `roles/cloudtrace.user`, `roles/errorreporting.viewer`, `roles/cloudprofiler.user`.

### One-time GCP setup (for operators)

1. **Enable APIs**: `run.googleapis.com`, `artifactregistry.googleapis.com`, `secretmanager.googleapis.com` (plus the observability APIs from Prerequisites).
2. **Artifact Registry**: `gcloud artifacts repositories create mcp --repository-format=docker --location=<region>`.
3. **Runtime service account** (minimal — it never reads observability data):
   ```bash
   gcloud iam service-accounts create mcp-gcp-observability
   ```
4. **Secrets**:
   ```bash
   openssl rand -base64 32 | gcloud secrets create mcp-obs-token-key --data-file=-
   printf '%s' "<client-secret>" | gcloud secrets create mcp-obs-google-client-secret --data-file=-
   # grant the runtime SA access:
   for s in mcp-obs-token-key mcp-obs-google-client-secret; do
     gcloud secrets add-iam-policy-binding $s \
       --member serviceAccount:mcp-gcp-observability@<project>.iam.gserviceaccount.com \
       --role roles/secretmanager.secretAccessor
   done
   ```
5. **First deploy** (to learn the service URL): `cp deploy/cloudrun.env.example deploy/cloudrun.env`, fill in everything except `AUTH_ISSUER_URL`, temporarily set `MCP_AUTH: none`, then `make deploy GCP_PROJECT=<project> GCP_REGION=<region>`.
6. **OAuth client**: in the same GCP project, configure the OAuth consent screen as **Internal** (this alone restricts login to your Workspace), then create an **OAuth client ID → Web application** with authorized redirect URI `https://<service-url>/callback`.
7. **Final deploy**: set `AUTH_ISSUER_URL` to the service URL, `MCP_AUTH: google`, and `make deploy` again.

`--allow-unauthenticated` at the Cloud Run level is intentional: the OAuth layer inside the app is the authentication boundary (the `/.well-known` metadata and the OAuth endpoints must be publicly reachable).

### Operations runbook

- **Key rotation**: generate a new key (`openssl rand -base64 32`), set the `mcp-obs-token-key` secret to `newKey,oldKey` (new key encrypts, old tokens stay valid), deploy; later drop the old key.
- **Kill switch**: replacing the token key with a single fresh key invalidates every outstanding token instantly (all users re-login).
- **Revoking one user**: disable the app for the user in the Workspace admin console (or the user revokes it at myaccount.google.com → Security); refresh grants stop working immediately, and outstanding access tokens expire within the hour (they never outlive the embedded Google token).
- **Restricting domains**: `AUTH_ALLOWED_DOMAINS` is checked at login, at token refresh, *and* on every MCP request — after redeploying with a domain removed, that domain's existing tokens are rejected on their next use.
- **No Workspace domain?** Set `AUTH_REQUIRE_PROJECT_ACCESS` to a GCP project ID instead: at login and on every token refresh the server probes `testIamPermissions` with the user's own token, so only Google accounts that hold at least one IAM role on that project (`resourcemanager.projects.get`) can log in. Revoking a user's IAM role cuts off token renewal and, natively, every GCP call. Both gates may be combined.

## GCP API Limits

Each tool call translates to one or more GCP API requests. Be aware of [GCP quotas](https://cloud.google.com/monitoring/quotas):

- **Cloud Logging** — default 60 read requests/minute per project
- **Cloud Monitoring** — default 6,000 time series read requests/minute
- **Cloud Trace** — default 300 read requests/minute
- **Error Reporting** — default 300 read requests/minute

The server applies per-tool timeouts (30-60 seconds). For large result sets, use pagination via `page_token` and keep `limit` values reasonable.

## Metrics Semantic Registry

Optionally provide a YAML file (`--metrics-registry`) to enrich metric analysis with domain knowledge:

```yaml
metrics:
  custom.googleapis.com/http/request_latency:
    kind: latency
    unit: ms
    better_direction: down
    slo_threshold: 500
    related_metrics:
      - custom.googleapis.com/http/request_count
      - custom.googleapis.com/http/error_rate

  custom.googleapis.com/http/error_rate:
    kind: error_rate
    unit: ratio
    better_direction: down
    slo_threshold: 0.01

  compute.googleapis.com/instance/cpu/utilization:
    kind: resource_utilization
    unit: ratio
    better_direction: down
    saturation_cap: 1.0
```

Without a registry, metric kinds are auto-detected from naming conventions (e.g. `latency` in name → latency kind).

Supported metric kinds: `latency`, `throughput`, `error_rate`, `resource_utilization`, `saturation`, `availability`, `freshness`, `business_kpi`.

## License

MIT
