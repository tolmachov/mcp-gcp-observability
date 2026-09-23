#!/usr/bin/env bash
set -euo pipefail
ACTION=${1:-check}
case "$ACTION" in
  check) source "$(dirname "$0")/lib.sh" check ;;
  prepare|apply|cutover) source "$(dirname "$0")/lib.sh" apply ;;
  *) echo "usage: $0 check|prepare|apply|cutover" >&2; exit 2 ;;
esac
DEPLOY_DIR=$(cd "$(dirname "$0")" && pwd)
require_commands gcloud curl python3

ENV_FILE=${ENV_FILE:-deploy/cloudrun.env}
IMAGE=${IMAGE:-}
ROLLOUT_DWELL_SECONDS=${ROLLOUT_DWELL_SECONDS:-300}

service_json=$(mktemp)
rollback_armed=0
restore_oauth=0
rollback() {
  echo "rollout failed; restoring $stable_revision" >&2
  if ! gcloud run services update-traffic "$SERVICE" --project "$GCP_PROJECT" --region "$GCP_REGION" --to-revisions "$stable_revision=100" --quiet; then
    echo "ROLLBACK FAILED: restore $stable_revision traffic manually" >&2
  fi
  if (( restore_oauth )); then
    if ! "$DEPLOY_DIR/oauth-routes.sh" stable; then
      echo "ROLLBACK FAILED: restore stable OAuth routes manually" >&2
    fi
  fi
}
finish() {
  local status=$?
  trap - EXIT
  # EXIT runs for failures inside functions too; ERR without errtrace does not.
  if (( status != 0 && rollback_armed )); then
    rollback
  fi
  rm -f "$service_json"
  exit "$status"
}
trap finish EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

describe_service() {
  gcloud run services describe "$SERVICE" --project "$GCP_PROJECT" --region "$GCP_REGION" --format=json >"$service_json"
}

annotation_value() {
  python3 -c 'import json,sys
d=json.load(open(sys.argv[1])); annotations=d["metadata"].get("annotations",{}) if sys.argv[2]=="service" else d["spec"]["template"]["metadata"].get("annotations",{})
print(annotations.get(sys.argv[3], ""))' "$service_json" "$1" "$2"
}

check_service_config() {
  describe_service
  local ingress timeout concurrency service_account execution min_scale max_scale cpu memory
  ingress=$(annotation_value service 'run.googleapis.com/ingress')
  timeout=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["spec"]["template"]["spec"].get("timeoutSeconds", ""))' "$service_json")
  concurrency=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["spec"]["template"]["spec"].get("containerConcurrency", ""))' "$service_json")
  service_account=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["spec"]["template"]["spec"].get("serviceAccountName", ""))' "$service_json")
  execution=$(annotation_value template 'run.googleapis.com/execution-environment')
  min_scale=$(annotation_value template 'autoscaling.knative.dev/minScale')
  max_scale=$(annotation_value template 'autoscaling.knative.dev/maxScale')
  cpu=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["spec"]["template"]["spec"]["containers"][0]["resources"]["limits"].get("cpu", ""))' "$service_json")
  memory=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["spec"]["template"]["spec"]["containers"][0]["resources"]["limits"].get("memory", ""))' "$service_json")
  [[ "$ingress" == "internal-and-cloud-load-balancing" ]] || { echo "unexpected ingress: $ingress" >&2; return 1; }
  [[ "$timeout" == "600" || "$timeout" == "600s" ]] || { echo "unexpected timeout: $timeout" >&2; return 1; }
  [[ "$concurrency" == "8" ]] || { echo "unexpected concurrency: $concurrency" >&2; return 1; }
  [[ "$service_account" == "$RUNTIME_SA" ]] || { echo "unexpected service account: $service_account" >&2; return 1; }
  [[ "$execution" == "gen2" && "$min_scale" == "1" && "$max_scale" == "3" ]] || { echo "unexpected execution/autoscaling settings" >&2; return 1; }
	[[ ("$cpu" == "1" || "$cpu" == "1000m") && "$memory" == "1Gi" ]] || { echo "unexpected resource limits: cpu=$cpu memory=$memory" >&2; return 1; }
}

if [[ "$ACTION" == "check" ]]; then
  check_service_config
  echo "rollout check: OK"
  exit 0
fi

describe_service
stable_revision=$(python3 -c 'import json,sys
d=json.load(open(sys.argv[1])); live=[x for x in d.get("status",{}).get("traffic",[]) if x.get("percent",0)>0]
if len(live)!=1 or live[0].get("percent")!=100 or not live[0].get("revisionName"):
    sys.exit("expected one stable revision at 100%; settle the current rollout first")
print(live[0]["revisionName"])' "$service_json")

if [[ "$ACTION" == "cutover" ]]; then
  [[ -n "${CANDIDATE_REVISION:-}" ]] || { echo "CANDIDATE_REVISION is required for cutover" >&2; exit 2; }
  candidate_revision=$CANDIDATE_REVISION
  tagged_revision=$(python3 -c 'import json,sys
print(next((x["revisionName"] for x in json.load(open(sys.argv[1]))["status"]["traffic"] if x.get("tag")=="candidate"), ""))' "$service_json")
  [[ "$candidate_revision" == "$tagged_revision" && "$candidate_revision" != "$stable_revision" ]] || {
    echo "candidate tag must name the prepared revision, different from stable" >&2; exit 1;
  }
  "$DEPLOY_DIR/oauth-routes.sh" check-candidate
  restore_oauth=1
  rollback_armed=1
else
  "$DEPLOY_DIR/oauth-routes.sh" check-stable
fi

if [[ "$ACTION" != "prepare" ]]; then
  [[ -n "${OPERATOR_BEARER_TOKEN:-}" ]] || { echo "OPERATOR_BEARER_TOKEN is required for authenticated readiness checks" >&2; exit 2; }
  [[ -n "${ALERT_CHECK_COMMAND:-}" ]] || { echo "ALERT_CHECK_COMMAND is required and must fail when a rollout-blocking alert is active" >&2; exit 2; }
fi

if [[ "$ACTION" == "apply" ]]; then
  # A legacy token cannot pass v2 readiness. Fail before deploying or mixing
  # incompatible revisions; use prepare + OAuth routing + cutover instead.
  curl --max-time 60 -fsS -H "Authorization: Bearer $OPERATOR_BEARER_TOKEN" "https://$PUBLIC_HOSTNAME/readyz" >/dev/null
fi

if [[ "$ACTION" == "prepare" || "$ACTION" == "apply" ]]; then
  [[ -f "$ENV_FILE" ]] || { echo "missing runtime configuration: $ENV_FILE" >&2; exit 2; }
  [[ -n "$IMAGE" ]] || { echo "IMAGE is required when deploying a candidate" >&2; exit 2; }
  env_values=$(grep -v '^[[:space:]]*#' "$ENV_FILE" | grep -v '^[[:space:]]*$' | sed 's/: /=/' | paste -sd'@' -)
  gcloud run deploy "$SERVICE" \
    --project "$GCP_PROJECT" --region "$GCP_REGION" --image "$IMAGE" \
    --service-account "$RUNTIME_SA" --allow-unauthenticated \
    --ingress internal-and-cloud-load-balancing --execution-environment gen2 \
    --min-instances 1 --max-instances 3 --cpu 1 --memory 1Gi --concurrency 8 --timeout 600s \
    --args run --set-env-vars "^@^$env_values" \
    --set-secrets "AUTH_GOOGLE_CLIENT_SECRET=$GOOGLE_SECRET:latest,AUTH_TOKEN_KEY=$TOKEN_SECRET:latest" \
    --no-traffic --tag candidate --quiet
  describe_service
  candidate_revision=$(python3 -c 'import json,sys
print(next((x["revisionName"] for x in json.load(open(sys.argv[1]))["status"]["traffic"] if x.get("tag")=="candidate"), ""))' "$service_json")
  [[ -n "$candidate_revision" && "$candidate_revision" != "$stable_revision" ]] || { echo "could not identify a new candidate revision" >&2; exit 1; }
fi

check_service_config
if [[ "$ACTION" == "prepare" ]]; then
  echo "Prepared CANDIDATE_REVISION=$candidate_revision; STABLE_REVISION=$stable_revision. No traffic was promoted."
  echo "For a breaking release, follow deploy/RUNBOOK.md: route OAuth to the candidate, authorize, then run cutover."
  exit 0
fi

curl --max-time 60 -fsS -H "Authorization: Bearer $OPERATOR_BEARER_TOKEN" "https://$PUBLIC_HOSTNAME/__candidate/readyz" >/dev/null

check_signals() {
  sleep "$ROLLOUT_DWELL_SECONDS"
  local bad_filter bad
  bad_filter="resource.type=\"cloud_run_revision\" AND resource.labels.service_name=\"$SERVICE\" AND (httpRequest.status>=500 OR jsonPayload.msg=(\"oauth_store_failure\" OR \"oauth_grant_corrupt\" OR \"response_budget_violation\" OR \"grant_replay\"))"
  bad=$(gcloud logging read "$bad_filter" --project "$GCP_PROJECT" --freshness "${ROLLOUT_DWELL_SECONDS}s" --limit 1 --format='value(timestamp)')
  [[ -z "$bad" ]] || { echo "rollout gate found a production error at $bad" >&2; return 1; }
  bash -o pipefail -c "$ALERT_CHECK_COMMAND"
  curl --max-time 60 -fsS -H "Authorization: Bearer $OPERATOR_BEARER_TOKEN" "https://$PUBLIC_HOSTNAME/readyz" >/dev/null
}

steps=(10 50 100)
if [[ "$ACTION" == "cutover" ]]; then
  steps=(100)
fi
rollback_armed=1
for candidate_percent in "${steps[@]}"; do
  stable_percent=$((100 - candidate_percent))
  if (( stable_percent > 0 )); then
    traffic="$candidate_revision=$candidate_percent,$stable_revision=$stable_percent"
  else
    traffic="$candidate_revision=100"
  fi
  gcloud run services update-traffic "$SERVICE" --project "$GCP_PROJECT" --region "$GCP_REGION" --to-revisions "$traffic" --quiet
  check_signals
done

if (( restore_oauth )); then
  "$DEPLOY_DIR/oauth-routes.sh" stable
fi
rollback_armed=0
echo "rollout $ACTION: OK ($candidate_revision at 100%)."
