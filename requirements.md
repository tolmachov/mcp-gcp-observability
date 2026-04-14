# GCP Logs & Error Reporting MCP Server  
## Technical Specification

> **Note:** This is the original design specification. The implementation has diverged significantly — additional tools and Cloud Trace support were added, and the directory structure differs. Refer to the README and source code for the current state.

---

## 1. Objective

Develop an MCP server that enables LLM clients to query, filter, and retrieve logs and errors from Google Cloud without using the web UI.

The server must provide:

- Access to **Google Cloud Logging**
- Access to **Error Reporting**
- Structured, LLM-friendly responses
- Safe and controlled access to production data

Target project (initial scope):

- `spring-monolith-431606-v7`

---

## 2. Scope

### Included

- MCP server implementation
- Integration with:
  - Google Cloud Logging API
  - Google Cloud Error Reporting API
- Tooling for logs + errors querying
- Response normalization
- Error handling
- Configuration via env/config
- Documentation
- Basic tests

### Excluded

- UI parsing (Cloud Console URLs)
- Web UI
- Log ingestion or sinks
- SIEM-level analytics
- Persistent storage outside GCP

---

## 3. Architecture Overview

- MCP server (Go preferred)
- stdio transport
- Uses:
  - Logging API (`entries.list`)
  - Error Reporting API (`projects.events.list`)

---

## 4. MCP Tools

---

## 4.1 `logs_query`

Execute arbitrary Cloud Logging queries.

### Input

```json
{
  "project_id": "string",
  "filter": "string",
  "start_time": "RFC3339",
  "end_time": "RFC3339",
  "limit": 100,
  "order": "desc",
  "page_token": "string"
}

Output

{
  "count": 57,
  "entries": [...]
}


⸻

4.2 logs.tail

Fetch recent logs without explicit time range.

Input

{
  "filter": "string",
  "lookback_seconds": 900,
  "limit": 100
}


⸻

4.3 logs_k8s

Kubernetes-aware logs query.

Input

{
  "namespace": "string",
  "pod_name": "string",
  "container_name": "string",
  "severity": "ERROR",
  "text_search": "timeout",
  "start_time": "...",
  "end_time": "..."
}


⸻

4.4 logs_by_trace

Find logs by trace ID.

⸻

4.5 logs_summary

Return aggregated insights:
	•	severity distribution
	•	top services
	•	top errors
	•	sample entries

⸻

🚨 5. Error Reporting Integration (NEW REQUIREMENT)

Integration with Google Cloud Error Reporting

⸻

5.1 Goal

Allow the agent to retrieve and analyze application errors from:

https://console.cloud.google.com/errors

Without relying on UI.

⸻

5.2 API

Use:
	•	projects.events.list
	•	projects.groupStats.list

⸻

5.3 MCP Tool: errors_list

Purpose

List error groups (aggregated errors).

⸻

Input

{
  "project_id": "string",
  "time_range_hours": 24,
  "limit": 50,
  "service_filter": "string",
  "version_filter": "string"
}


⸻

Behavior
	•	Query Error Reporting API
	•	Return grouped errors (NOT raw logs)
	•	Sorted by occurrence count (desc)

⸻

Output

{
  "groups": [
    {
      "group_id": "string",
      "service": "payment-service",
      "message": "context deadline exceeded",
      "count": 123,
      "first_seen": "timestamp",
      "last_seen": "timestamp",
      "affected_versions": ["v1", "v2"]
    }
  ]
}


⸻

5.4 MCP Tool: errors_get

Purpose

Get details for a specific error group.

⸻

Input

{
  "group_id": "string",
  "limit": 20
}


⸻

Output

{
  "group": {
    "group_id": "string",
    "message": "...",
    "service": "...",
    "instances": [
      {
        "timestamp": "...",
        "message": "...",
        "stack_trace": "...",
        "context": {},
        "http_request": {}
      }
    ]
  }
}


⸻

5.5 MCP Tool: errors_summary

Purpose

Provide LLM-friendly summary of errors.

⸻

Output Example

{
  "total_errors": 842,
  "top_errors": [
    {
      "message": "context deadline exceeded",
      "count": 120
    }
  ],
  "top_services": [
    {
      "service": "payment-service",
      "count": 400
    }
  ]
}


⸻

5.6 Use Cases

Case 1

“Show top errors in last 24h”

→ errors_list

⸻

Case 2

“Why is payment-service failing?”

→ errors_summary + errors_get

⸻

Case 3

“Show stack traces for this error”

→ errors_get

⸻

5.7 Important Notes
	•	Error Reporting groups errors automatically
	•	Not all logs appear there
	•	Works best with:
	•	exceptions
	•	stack traces
	•	structured error logs

⸻

6. Non-Functional Requirements

Performance
	•	Typical request: ≤ 3s
	•	Heavy request: ≤ 10s

⸻

Reliability

Handle:
	•	invalid filters
	•	permission errors
	•	empty results
	•	API timeouts

⸻

Security
	•	No secrets in logs
	•	No credential leaks
	•	IAM minimal scope:
	•	logging.logEntries.list
	•	errorreporting.groupMetadata.list
	•	errorreporting.events.list

⸻

7. Configuration

GCP_DEFAULT_PROJECT=spring-monolith-431606-v7
LOGS_MAX_LIMIT=1000
ERRORS_MAX_LIMIT=100


⸻

8. MCP Config Example

{
  "mcpServers": {
    "gcp-observability": {
      "command": "/usr/local/bin/gcp-mcp",
      "env": {
        "GCP_DEFAULT_PROJECT": "spring-monolith-431606-v7"
      }
    }
  }
}


⸻

9. Acceptance Criteria
	•	MCP server starts successfully
	•	Tools visible in client
	•	Logs retrieval works
	•	Error Reporting retrieval works
	•	Structured responses returned
	•	No crashes on large datasets

⸻

10. Implementation Phases

Phase 1
	•	logs_query
	•	errors_list

Phase 2
	•	logs_k8s
	•	errors_get

Phase 3
	•	logs_summary
	•	errors_summary

⸻

11. Tech Stack

Preferred:
	•	Go
	•	GCP official SDK

⸻

12. Repo Structure

gcp-mcp/
  cmd/server/main.go
  internal/logging/
  internal/errors/
  internal/mcp/
  internal/config/
  README.md


⸻

13. Key Risks
	•	large payloads
	•	API quotas
	•	inconsistent log formats
	•	missing trace correlation

⸻

14. Deliverables
	•	source code
	•	README
	•	config examples
	•	tests

