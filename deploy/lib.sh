#!/usr/bin/env bash
set -euo pipefail

MODE=${1:-check}
if [[ "$MODE" != "check" && "$MODE" != "apply" ]]; then
  echo "usage: $0 check|apply" >&2
  exit 2
fi

required_vars=(PUBLIC_HOSTNAME DNS_ZONE FIRESTORE_LOCATION GCP_PROJECT GCP_REGION)
for name in "${required_vars[@]}"; do
  if [[ -z "${!name:-}" ]]; then
    echo "$name is required" >&2
    exit 2
  fi
done

SERVICE=${SERVICE:-mcp-gcp-observability}
AR_REPO=${AR_REPO:-mcp}
FIRESTORE_DATABASE=${AUTH_STATE_DATABASE:-"(default)"}
FIRESTORE_PROJECT=${AUTH_STATE_PROJECT:-$GCP_PROJECT}
RUNTIME_SA=${RUNTIME_SA:-${SERVICE}@${GCP_PROJECT}.iam.gserviceaccount.com}
GOOGLE_SECRET=${GOOGLE_SECRET:-mcp-obs-google-client-secret}
TOKEN_SECRET=${TOKEN_SECRET:-mcp-obs-token-key}
RESOURCE_PREFIX=${RESOURCE_PREFIX:-mcp-obs}

require_commands() {
  local command
  for command in "$@"; do
    command -v "$command" >/dev/null || { echo "missing command: $command" >&2; exit 2; }
  done
}

apply_only() {
  if [[ "$MODE" == "apply" ]]; then
    "$@"
  else
    return 1
  fi
}

export MODE SERVICE AR_REPO FIRESTORE_DATABASE FIRESTORE_PROJECT RUNTIME_SA GOOGLE_SECRET TOKEN_SECRET RESOURCE_PREFIX
