# MCP GCP Observability

MCP server for querying Google Cloud Logging and Error Reporting without the web UI.

## Features

- Query Cloud Logging with full filter syntax
- Kubernetes-aware log queries
- Trace-based log correlation
- HTTP request discovery with trace/request ID extraction
- Error Reporting integration (grouped errors, stack traces)
- Service discovery
- Log aggregation and summaries

## Prerequisites

- Go 1.22+
- GCP project with Cloud Logging and Error Reporting APIs enabled
- Application Default Credentials configured:
  ```bash
  gcloud auth application-default login
  ```
- Required IAM permissions:
  - `logging.logEntries.list`
  - `errorreporting.groupMetadata.list`
  - `errorreporting.events.list`

## Installation

```bash
go install github.com/tolmachov/mcp-gcp-observability@latest
```

Or build from source:

```bash
git clone https://github.com/tolmachov/mcp-gcp-observability.git
cd mcp-gcp-observability
go build -o mcp-gcp-observability .
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

| Tool | Description |
|------|-------------|
| `logs.query` | Execute arbitrary Cloud Logging queries with filter syntax |
| `logs.k8s` | Query Kubernetes container logs with convenient filters |
| `logs.by_trace` | Find all logs associated with a trace ID |
| `logs.find_requests` | Find HTTP requests by URL pattern, returns trace/request IDs |
| `logs.summary` | Aggregated log statistics (severity, top services, top errors) |
| `logs.services` | Discover available services and resources in the project |
| `errors.list` | List error groups from Error Reporting, sorted by count |
| `errors.get` | Get error group details with individual events |

## Prompt Examples

**Initial triage:**
- "What services are running in the project?"
- "Show me a summary of logs from the last hour"
- "What are the top errors in the last 24 hours?"

**Debugging:**
- "Show me ERROR logs from the crypto-steam namespace"
- "Find recent requests to /api/profile that returned 500"
- "Get all logs for trace ID abc123def456"
- "Show stack traces for error group XYZ"

**Investigation:**
- "Why is payment-service failing? Check errors and recent logs"
- "Find slow requests to /v1/connect/balance"
- "Show me traced requests to /api/profile so I can investigate latency"

## Configuration

| Environment Variable | Default | Description |
|---------------------|---------|-------------|
| `GCP_DEFAULT_PROJECT` | (required) | Default GCP project ID |
| `LOGS_MAX_LIMIT` | `1000` | Maximum log entries per request |
| `ERRORS_MAX_LIMIT` | `100` | Maximum error groups per request |

## License

MIT
