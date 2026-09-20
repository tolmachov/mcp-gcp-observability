# Production runbook

## Release boundary

This release intentionally has no compatibility bridge. Before rollout, notify users that DCR registration and Google authorization must be repeated. Never restore old token keys, old cursor decoders, or cached profiler diff objects as a rollback mechanism.

## Prerequisites

Export `PUBLIC_HOSTNAME`, `DNS_ZONE`, `FIRESTORE_LOCATION`, `GCP_PROJECT`, and `GCP_REGION`. If OAuth state is stored in a different project, also export `AUTH_STATE_PROJECT`. Set `AUTH_STATE_DATABASE` for a non-default database.

Create Google OAuth and token-key Secret Manager values out of band. The Google web client must authorize exactly `https://$PUBLIC_HOSTNAME/callback`.

Run:

```bash
./deploy/bootstrap.sh apply
./deploy/bootstrap.sh check
```

For a new environment, provision the initial v2 Cloud Run service with the runtime settings from `rollout.sh` before creating the serverless NEG. There is no old revision to preserve: this initial service receives 100% traffic, with restricted ingress. The release scripts below require an existing stable revision at 100%. Then run:

```bash
./deploy/edge.sh apply
./deploy/edge.sh check
./deploy/observability.sh apply
./deploy/observability.sh check
```

`bootstrap.sh` refuses to change an existing Firestore location. Confirm TTL is enabled for `mcp_oauth_states.expires_at`, `mcp_oauth_codes.expires_at`, and `mcp_oauth_grants.expires_at`. Application checks remain authoritative because TTL deletion is asynchronous.

## Pre-production gates

1. Run build, vet, lint, unit, race, `make test-deploy`, Firestore-emulator, and vulnerability checks. Deployment tests use fake CLIs and never contact GCP.
2. Run `make test-production` with delegated test-user credentials. A missing product or any skipped tool is a failed release gate.
3. Run `infra-check`, `observability-check`, and `deploy-check` against the target project.
4. Confirm Cloud Armor rules are attached: OAuth endpoints allow 60 requests/minute/IP, ban after 300 requests/5 minutes for 600 seconds, and the remaining endpoint allows 600 requests/minute/IP.
5. Confirm direct `run.app` requests cannot reach the service and the public hostname resolves to the global load-balancer address.

## First v2 cutover (and other incompatible releases)

Reserve a maintenance window and serialize all deployments and URL-map edits for this service. Old clients must register and authorize again. Do not use the 10%/50% path when either revision cannot accept the other revision's OAuth artifacts.

1. Run `make deploy-prepare`. This deploys a tagged candidate at zero traffic, checks its configuration, and prints `CANDIDATE_REVISION` and `STABLE_REVISION`. It needs no MCP access token. Record both exact revision names for recovery; leave the issuer, Google callback URL, and token keys configured for the production hostname.
2. Export the printed revision names and route **all OAuth endpoints** (discovery, registration, authorization, callback, token and revocation) through the protected candidate backend:

   ```bash
   export CANDIDATE_REVISION=service-prepared-revision
   export STABLE_REVISION=service-prior-revision
   ./deploy/oauth-routes.sh candidate
   ./deploy/oauth-routes.sh check-candidate
   ```

   MCP traffic still targets the old revision. OAuth v1 operations and new MCP v2 calls may fail during this maintenance phase. Wait for load-balancer propagation before starting a fresh login. The routing helper only replaces the exact dedicated matcher created by `edge.sh`; it refuses additional policies or shared host rules. It uses the existing Cloud Armor-protected backends and preserves the production hostname and issuer.
3. Register an operator client and complete Google authorization at `https://$PUBLIC_HOSTNAME`, including the code exchange at `/token`. Save its new `mcp_at_v2_...` access token as `OPERATOR_BEARER_TOKEN`. OAuth now consistently targets the candidate even though it has zero ordinary traffic. Complete this flow with an OAuth-capable MCP client; MCP calls become available after the next step. If login or any preparation fails, abort with `./deploy/oauth-routes.sh stable`; no traffic has been promoted yet.
4. Set `ALERT_CHECK_COMMAND` to the organization alert gate, then run `make deploy-cutover`. This reuses the exact tagged revision (it does not redeploy), validates candidate readiness with the new token, and sends **100%** traffic to it in one update. It never assigns 10% or 50% for this transition. After the dwell and signal gates succeed, OAuth routes return to the ordinary backend, which now serves the same v2 revision.

Once cutover has validated its candidate and route ownership, any failure, interrupt, or termination restores both stable traffic and stable OAuth routes. An explicit rollback failure is printed if GCP rejects recovery; use the manual commands below. Before that point (for example, a changed candidate tag), the script refuses to alter routing; abort the prepared maintenance phase explicitly. Do not run `edge-apply` or another deployment between prepare and completion/abort. `edge-check` deliberately rejects the temporary cutover matcher.

## Compatible rollout

Set `OPERATOR_BEARER_TOKEN` to a current operator MCP access token. Set `ALERT_CHECK_COMMAND` to a command that exits non-zero when an organization alert or incident is active. Then run `make deploy`.

The script requires ordinary OAuth routing and checks stable readiness before deploying. It deploys with no traffic, checks `/__candidate/readyz` through the protected candidate load-balancer route using the same token, and shifts 10%, 50%, then 100%. Each stage waits `ROLLOUT_DWELL_SECONDS` (default five minutes), rejects recent 5xx/OAuth-store/replay/response-budget events, executes `ALERT_CHECK_COMMAND`, and performs authenticated public readiness. Errors inside any gate, including failure to read logs, invoke rollback. Readiness HTTP requests have a 60-second timeout.

The URL-map switch uses [gcloud add-path-matcher](https://docs.cloud.google.com/sdk/gcloud/reference/compute/url-maps/add-path-matcher) with `--existing-host` and `--delete-orphaned-path-matcher`; traffic promotion uses explicit [revision percentages](https://docs.cloud.google.com/sdk/gcloud/reference/run/services/update-traffic).

At 100%, verify:

- authenticated MCP initialize and a tool call through the public load balancer;
- an initialize request on one instance followed by a tool call on another;
- pinned schemas omit and reject `project_id`, or unpinned calls succeed against two IAM-authorized projects;
- replaying a consumed code or refresh token revokes the grant family;
- maximum log, profiler, and flamegraph responses remain within budgets.

## Rollback

Automatic rollout failure sends 100% traffic to the prior revision; cutover also restores ordinary OAuth routing. For manual rollback (including an interrupted maintenance phase):

```bash
gcloud run services update-traffic "$SERVICE" \
  --project "$GCP_PROJECT" --region "$GCP_REGION" \
  --to-revisions "$STABLE_REVISION=100"
./deploy/oauth-routes.sh stable
./deploy/oauth-routes.sh check-stable
```

Do not delete Firestore state. Because compatibility is intentionally absent, users authorize again after rollback if their client artifacts no longer match the active revision.

## Key rotation and revocation

Add a new Secret Manager version containing `newKey,oldKey`, deploy, then remove the old key in a later release. The first key encrypts and all listed keys decrypt. Dropping an old key invalidates artifacts encrypted by it and requires reauthorization.

The revocation endpoint immediately marks the Firestore family revoked and attempts Google revocation. Access-token verification reads the family on every MCP request. Firestore unavailability is a `503` availability incident, never an authentication failure.

## Alert response

For client `400`/`401` responses, inspect the matching application rejection:

```bash
gcloud logging read \
  'resource.type="cloud_run_revision" AND resource.labels.service_name="mcp-gcp-observability" AND jsonPayload.msg="http_request_rejected"' \
  --project "$GCP_PROJECT" --freshness=30m --limit=50 \
  --format='table(timestamp,jsonPayload.client,jsonPayload.route,jsonPayload.status,jsonPayload.rpc_method,jsonPayload.reason,jsonPayload.protocol_version,jsonPayload.request_id)'
```

Use the client's response `X-Request-ID` or the event's `trace_id` to correlate
requests. `accept_requires_json_and_sse`, `request_missing_id`, and
`unsupported_protocol_version` identify transport failures before tool dispatch.
On `/token`, `oauth_error=invalid_grant` plus `reason` distinguishes malformed or
legacy refresh tokens from expired, revoked, or replayed grants. A `200` MCP
response can still contain a JSON-RPC/tool error; HTTP rejection logs alone do
not prove tool success.

- `MCP: oauth store failures`: halt rollout; check Firestore availability, database ID, IAM, and quota.
- `MCP: grant security events`: distinguish explicit revocation from replay; replay requires security investigation.
- `MCP: response budget violations`: capture the tool name and input; do not raise limits before fixing normalization.
- `MCP: profiler saturation`: inspect concurrent workload and profile sizes.
- memory above 80% or container restarts: stop rollout and inspect compressed-cache accounting and decompression bounds.
- Cloud Armor bans: confirm abusive source and avoid bypassing the load balancer.
- 5xx above 1% for five minutes: roll back unless a known external GCP outage is confirmed.
