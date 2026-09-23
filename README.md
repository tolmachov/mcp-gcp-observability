# MCP GCP Observability

MCP server for Google Cloud Logging, Error Reporting, Trace, Monitoring, and Profiler. It has two deliberately separate deployment modes:

- local `stdio`, authenticated with Application Default Credentials;
- remote stateless HTTP, always authenticated through Google OAuth and backed by Firestore security state.

This release is breaking. Existing OAuth tokens, refresh-token families, DCR client IDs, profiler diff identifiers, pagination cursors, and old tool schemas are invalid. Every remote client must register and authorize again after rollout.

## Requirements

- Go version from `go.mod`;
- enabled APIs for the products being queried;
- user IAM permissions for the requested GCP data;
- for HTTP: Google OAuth web client, Firestore Native, External HTTPS Load Balancer, Cloud Armor, and managed DNS.

The runtime service account reads and writes only Firestore OAuth state and Secret Manager values. Every observability RPC uses the delegated Google user token; the runtime identity is never used as a data-access fallback.

## Local stdio

Authenticate ADC and install the binary:

```bash
gcloud auth application-default login
go install github.com/tolmachov/mcp-gcp-observability@latest
```

Pinned configuration omits `project_id` from every project-scoped public schema:

```json
{
  "mcpServers": {
    "gcp-observability": {
      "command": "/path/to/mcp-gcp-observability",
      "args": ["run"],
      "env": {"GCP_DEFAULT_PROJECT": "my-production-project"}
    }
  }
}
```

Without `GCP_DEFAULT_PROJECT`, every project-scoped tool and prompt requires `project_id`, and resource URIs use a required project segment. Unknown input fields are rejected, so a pinned deployment cannot be overridden by injecting `project_id`.

## Remote HTTP

HTTP cannot start without complete Google OAuth and Firestore configuration. There is no unauthenticated mode and no process-local session or OAuth security state. Streamable HTTP requests are stateless and can land on different instances.

Required settings:

| Setting | Purpose |
|---|---|
| `MCP_TRANSPORT=http` | Enable Streamable HTTP |
| `AUTH_ISSUER_URL` | Public HTTPS load-balancer origin, without query or fragment |
| `AUTH_GOOGLE_CLIENT_ID` | Google OAuth web client ID |
| `AUTH_GOOGLE_CLIENT_SECRET` | Google OAuth client secret; use Secret Manager |
| `AUTH_TOKEN_KEY` | Base64 32-byte encryption key; use Secret Manager |
| `AUTH_STATE_PROJECT` | Project containing Firestore security state |
| `AUTH_STATE_DATABASE` | Firestore database, default `(default)` |
| `AUTH_ALLOWED_DOMAINS` | Workspace admission allowlist; mandatory when unpinned |
| `GCP_DEFAULT_PROJECT` | Optional API pin; if set, access is checked at login and refresh |

Optional settings are `AUTH_ALLOWED_REDIRECTS`, `AUTH_GOOGLE_SCOPES`, `AUTH_SKIP_CONSENT` (development only), `MCP_HTTP_ADDR`, `MCP_VARIANT`, `DNS_SERVER`, and `METRICS_REGISTRY_FILE`.

Redirect URIs must be absolute and contain no userinfo or fragment. HTTPS and custom schemes require an explicit allowlist entry. HTTP is accepted only for loopback IPs or `localhost`. Authorization compares the complete registered URI, including query, with only the loopback port allowed to differ.

OAuth authorization state and codes are opaque, single-use values. Refresh tokens consist of a family ID and random secret. Code redemption and refresh rotation are Firestore transactions; replay revokes the whole grant family. Every MCP request verifies that the family remains active. Firestore failure returns `503`, while invalid credentials return `401`.

## Project contract

`GCP_DEFAULT_PROJECT` changes the public MCP API at registration time:

| Deployment | Tool and prompt input | Resources | Authorization |
|---|---|---|---|
| Pinned | no `project_id`; unknown fields rejected | exact URI containing the configured project | login and refresh check access to the pin; each GCP RPC uses user IAM |
| Unpinned | required syntactically valid `project_id` | URI template with required project segment | domain admission gate plus user IAM on every GCP RPC |

OAuth never asks the user to choose a project and tokens contain no project claim.

## Tools

| Area | Tools |
|---|---|
| Logging | `logs_query`, `logs_k8s`, `logs_by_trace`, `logs_by_request_id`, `logs_find_requests`, `logs_services`, `logs_summary` |
| Error Reporting | `errors_list`, `errors_get`, `errors_trends` |
| Trace | `trace_list`, `trace_get`, `trace_find_from_logs` |
| Monitoring | `metrics_list`, `metrics_snapshot`, `metrics_top_contributors`, `metrics_related`, `metrics_compare` |
| Profiler | `profiler_list`, `profiler_top`, `profiler_peek`, `profiler_flamegraph`, `profiler_compare`, `profiler_trends` |

`errors_list` and `errors_trends` use `window`, one of `1h`, `6h`, `24h`, `7d`, or `30d` (default `24h`). Responses return the same `window` and exact `time_range_begin`.

Profiler analysis always uses immutable source IDs. `profiler_top`, `profiler_peek`, and `profiler_flamegraph` accept `profile_id` plus optional `base_profile_id`; a diff is recomputed for that request. `profiler_compare` returns comparison data, not a cached identifier. `profiler_list` cursors are opaque and bound to filter fingerprints; raw Google page tokens and cursors from older releases are rejected.

Example unpinned calls:

```json
{"name":"errors_list","arguments":{"project_id":"my-project","window":"24h","limit":20}}
```

```json
{"name":"profiler_top","arguments":{"project_id":"my-project","profile_id":"current-source-id","base_profile_id":"base-source-id","limit":20}}
```

Built-in prompts are `investigate-errors`, `trace-request`, `investigate-metrics`, `service-health`, `investigate-profile`, and local-only `generate-metrics-registry`.

## Resource and concurrency limits

- HTTP headers: 64 KiB; MCP request body: 1 MiB; encoded tool result: 2 MiB (a larger result becomes a tool error asking for a shorter window, a lower limit or a narrower filter).
- Logs: at most 200 entries and 8 KiB normalized JSON per entry. Truncation reports `entry_truncated`, `omitted_bytes`, and `truncated_fields`.
- Flamegraphs: at most 1,000 nodes; `pruned_nodes` counts the subtrees cut by `max_depth`, `min_pct` or the node limit (one per omitted child, not its descendants).
- Compressed source profile: 16 MiB; decompressed profile: 64 MiB.
- Profiler cache: compressed sources only, 16 MiB per user and 64 MiB per process. Parsed graphs are request-local.
- Four concurrent tool calls per user and two concurrent `profiler_*` calls per process. A diff (`base_profile_id` or `profiler_compare`) is one call and fetches its two source profiles one after the other. A call that times out waiting for a slot fails with an error saying so.
- `profiler_*` calls have an eight-minute scan budget that starts once the call holds a profiler slot; a call that uses it up fails with advice to narrow the scan. Cloud Run has a ten-minute request timeout, which also counts the wait for a slot.

Monitoring ingestion and aggregation discard non-finite values, count them in `non_finite_points`, and emit data-quality warnings. Public JSON never contains `NaN` or infinity. Registry validation rejects non-finite thresholds, SLOs, and saturation values.

## Production deployment

Copy runtime settings and export the required deployment variables:

```bash
cp deploy/cloudrun.env.example deploy/cloudrun.env
export PUBLIC_HOSTNAME=mcp-observability.example.com
export DNS_ZONE=example-com
export FIRESTORE_LOCATION=nam5
export GCP_PROJECT=my-runtime-project
export GCP_REGION=us-central1
```

Create the two Secret Manager resources and their values out of band:

- `mcp-obs-google-client-secret`;
- `mcp-obs-token-key` (for example, `openssl rand -base64 32`).

Scripts never create or print secret values. Infrastructure scripts support a read-only `check` mode and an explicit `apply` mode:

```bash
make bootstrap-apply
make bootstrap-check
# After the Cloud Run service exists:
make edge-apply
make observability-apply
make edge-check observability-check
```

`deploy/bootstrap.sh` enables APIs, creates Artifact Registry and Firestore Native, validates the immutable Firestore location, enables TTL on `expires_at`, and configures the runtime identity. `deploy/edge.sh` creates the serverless NEG, External Managed HTTPS load balancer, managed certificate, DNS record, and Cloud Armor limits. Direct `run.app` ingress is blocked by the Cloud Run ingress setting.

For a compatible release, build and stage with an authenticated operator token:

```bash
export OPERATOR_BEARER_TOKEN='mcp_at_v2_...'
make deploy
```

The rollout creates a no-traffic revision with 1 CPU, 1 GiB memory, concurrency 8, one to three instances, gen2, and a 600-second timeout. It checks Firestore-backed readiness, then promotes 10% → 50% → 100%, waiting and checking production signals at every gate. `ALERT_CHECK_COMMAND` is required and must exit non-zero while any organization rollout-blocking alert is active. On failure the script restores the prior revision and keeps Firestore data.

For the first **v1 → v2 transition**, use `make deploy-prepare`, route OAuth to the prepared candidate with `deploy/oauth-routes.sh candidate`, register and authorize the operator again, then run `make deploy-cutover`. This maintenance procedure switches directly to 100%; it never mixes incompatible OAuth revisions. Follow the exact commands and recovery instructions in the runbook. Cutover failures restore both traffic and OAuth routing.

For a new environment with no prior revision, provision the initial v2 Cloud Run service first, then run `edge-apply` and `observability-apply` and complete an authenticated smoke test. Release scripts require one existing stable revision at 100%.

See [deploy/RUNBOOK.md](deploy/RUNBOOK.md) for setup, smoke, rollback, and key-rotation procedures.

HTTP rejections emit `http_request_rejected` with the status, classified reason,
MCP method, protocol version, client family, and presence of session/MCP headers.
OAuth token errors also include `oauth_error` and the server-owned explanation.
`X-Request-ID` in the response matches `request_id` in the event; `trace_id` links
the event to the Cloud Run request trace. No bearer/refresh tokens, session IDs,
RPC arguments, raw error bodies, or URL query parameters are logged by this
diagnostic. Unrecognized errors are marked `unclassified_http_error` rather than
dumping potentially sensitive response content. Successful SSE responses stream
unchanged without diagnostic buffering.

## Development and verification

```bash
make build
make test
make test-race
make test-deploy
make lint
```

CI also runs Firestore-emulator transaction tests and pinned `govulncheck`. Real GCP integration tests are manual because they require delegated credentials and live product data:

```bash
make test-integration
# Release gate: fails if any real-GCP test is skipped.
make test-production
```

## Metrics registry

Use `METRICS_REGISTRY_FILE` to overlay semantic metadata and validate it before deployment:

```bash
mcp-gcp-observability validate-registry ./metrics.yaml
```

Supported kinds are `latency`, `throughput`, `error_rate`, `resource_utilization`, `saturation`, `availability`, `freshness`, and `business_kpi`.
