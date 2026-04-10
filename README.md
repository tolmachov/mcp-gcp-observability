# MCP GCP Observability

MCP server for querying Google Cloud Logging, Error Reporting, Cloud Trace, and Cloud Monitoring without the web UI.

## Features

- **Cloud Logging** — query with full filter syntax, Kubernetes-aware queries, log summaries and service discovery
- **Error Reporting** — grouped errors sorted by count, individual events with stack traces
- **Cloud Trace** — span trees, latency analysis, trace-based log correlation
- **Cloud Monitoring** — metric discovery, semantic snapshots with baseline comparison, anomaly classification, contributor drill-down, and arbitrary window comparison

## Prerequisites

- Go 1.26+
- GCP project with the following APIs enabled:
  - Cloud Logging
  - Error Reporting
  - Cloud Trace
  - Cloud Monitoring
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
| `logs.query` | Execute arbitrary Cloud Logging queries with filter syntax |
| `logs.k8s` | Query Kubernetes container logs with convenient filters |
| `logs.by_trace` | Find all logs associated with a trace ID |
| `logs.by_request_id` | Find all logs associated with a request ID |
| `logs.find_requests` | Discover HTTP requests by URL pattern, returns trace/request IDs |
| `logs.services` | Discover available services and resources in the project |
| `logs.summary` | Aggregated log statistics: severity distribution, top services, top errors |

### Error Reporting

| Tool | Description |
|------|-------------|
| `errors.list` | List error groups sorted by count |
| `errors.get` | Get error group details with individual events and stack traces |

### Tracing

| Tool | Description |
|------|-------------|
| `trace.get` | Get trace details with complete span tree by trace ID |

### Metrics

| Tool | Description |
|------|-------------|
| `metrics.list` | Discover available metrics from Cloud Monitoring |
| `metrics.snapshot` | Semantic snapshot with baseline comparison, trend detection, and anomaly classification |
| `metrics.top_contributors` | Break down metric by label dimension to find top contributors to an anomaly |
| `metrics.related` | Check all related metrics for correlated anomalies |
| `metrics.compare` | Compare two arbitrary time windows (before/after deploy, incident diff) |

## Recommended Workflow

### Logs & Errors

1. `logs.services` — discover available services
2. `logs.summary` — get severity distribution, top errors, top services
3. `errors.list` — list error groups sorted by count
4. `logs.query` or `logs.k8s` — investigate specific logs with filters
5. `logs.by_trace` — follow a single request across services
6. `trace.get` — get detailed span tree for latency analysis

### Metrics

1. `metrics.list` — discover available metrics, filter by kind
2. `metrics.snapshot` — get semantic snapshot with baseline comparison
3. `metrics.top_contributors` — drill down by dimension to find root cause
4. `metrics.related` — check correlated metrics for broader context
5. `metrics.compare` — compare before/after windows for deploy or incident analysis

## Built-in Prompts

The server provides pre-built investigation workflows:

| Prompt | Description |
|--------|-------------|
| `investigate-errors` | Investigate top errors, get details, find related logs |
| `trace-request` | Trace HTTP request end-to-end: find by URL, follow trace, analyze spans |
| `investigate-metrics` | Metric anomaly investigation: discover, snapshot, drill down, check related |
| `service-health` | Check health of services: discover, summarize logs, identify issues |

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

### HTTP Transport

For remote deployments or shared access, use the streamable HTTP transport:

```bash
mcp-gcp-observability run --transport http --http-addr :8080
```

**Security:** The HTTP transport does not include built-in authentication. When exposing over a network, place it behind a reverse proxy with authentication or use network-level access controls.

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
