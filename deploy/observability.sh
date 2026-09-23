#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/lib.sh" "${1:-check}"
require_commands gcloud

POLICY=${RESOURCE_PREFIX}-armor
metric_prefix=${RESOURCE_PREFIX//-/_}
notification_args=()
if [[ -n "${NOTIFICATION_CHANNELS:-}" ]]; then
  notification_args=(--notification-channels "$NOTIFICATION_CHANNELS")
fi

declare -A filters=(
  [oauth_store_failures]="resource.type=\"cloud_run_revision\" AND resource.labels.service_name=\"$SERVICE\" AND (jsonPayload.msg=\"oauth_store_failure\" OR jsonPayload.msg=\"oauth_grant_corrupt\")"
  [grant_security_events]="resource.type=\"cloud_run_revision\" AND resource.labels.service_name=\"$SERVICE\" AND (jsonPayload.msg=\"grant_replay\" OR jsonPayload.msg=\"grant_revoked\")"
  [response_budget_violations]="resource.type=\"cloud_run_revision\" AND resource.labels.service_name=\"$SERVICE\" AND jsonPayload.msg=\"response_budget_violation\""
  [profiler_saturation]="resource.type=\"cloud_run_revision\" AND resource.labels.service_name=\"$SERVICE\" AND jsonPayload.msg=\"profiler_saturation\""
  [container_restarts]="resource.type=\"cloud_run_revision\" AND resource.labels.service_name=\"$SERVICE\" AND log_id(\"run.googleapis.com/varlog/system\") AND textPayload:\"Container called exit\""
  [cloud_armor_bans]="resource.type=\"http_load_balancer\" AND jsonPayload.enforcedSecurityPolicy.name=\"$POLICY\" AND jsonPayload.statusDetails=\"denied_by_security_policy\""
)

metric_exists() {
  gcloud logging metrics describe "$1" --project "$GCP_PROJECT" >/dev/null 2>&1
}

policy_exists() {
  local display_name=$1
  gcloud monitoring policies list --project "$GCP_PROJECT" --format='value(displayName)' | grep -Fxq "$display_name"
}

ensure_metric() {
  local short_name=$1
  local metric_name=${metric_prefix}_${short_name}
  local filter=${filters[$short_name]}
  if metric_exists "$metric_name"; then
    if [[ "$MODE" == "apply" ]]; then
      gcloud logging metrics update "$metric_name" --project "$GCP_PROJECT" --log-filter "$filter" --description "MCP GCP Observability: $short_name" --quiet
    fi
  elif [[ "$MODE" == "apply" ]]; then
    gcloud logging metrics create "$metric_name" --project "$GCP_PROJECT" --log-filter "$filter" --description "MCP GCP Observability: $short_name" --quiet
  else
    echo "missing logs-based metric: $metric_name" >&2
    exit 1
  fi
}

ensure_count_policy() {
  local short_name=$1
  local display_name="MCP: ${short_name//_/ }"
  local metric_name=${metric_prefix}_${short_name}
  if policy_exists "$display_name"; then
    return
  fi
  if [[ "$MODE" == "check" ]]; then
    echo "missing alert policy: $display_name" >&2
    exit 1
  fi
  gcloud monitoring policies create --project "$GCP_PROJECT" \
    --display-name "$display_name" \
    --condition-display-name "$display_name" \
    --condition-filter "metric.type=\"logging.googleapis.com/user/$metric_name\"" \
    --aggregation='{"alignmentPeriod":"60s","perSeriesAligner":"ALIGN_RATE","crossSeriesReducer":"REDUCE_SUM"}' \
    --duration 0s --if '> 0' --trigger-count 1 \
    --documentation "Investigate the structured event in Cloud Logging and stop rollout while this alert is firing." \
    "${notification_args[@]}" --quiet
}

for short_name in "${!filters[@]}"; do
  ensure_metric "$short_name"
  ensure_count_policy "$short_name"
done

ensure_json_policy() {
  local display_name=$1
  local policy_json=$2
  if policy_exists "$display_name"; then
    return
  fi
  if [[ "$MODE" == "check" ]]; then
    echo "missing alert policy: $display_name" >&2
    exit 1
  fi
  gcloud monitoring policies create --project "$GCP_PROJECT" --policy "$policy_json" "${notification_args[@]}" --quiet
}

five_xx_name="MCP: 5xx ratio above 1 percent"
five_xx_policy=$(printf '%s' "{\"displayName\":\"$five_xx_name\",\"combiner\":\"OR\",\"enabled\":true,\"conditions\":[{\"displayName\":\"5xx ratio > 1% for 5m\",\"conditionThreshold\":{\"filter\":\"resource.type=\\\"cloud_run_revision\\\" AND resource.label.\\\"service_name\\\"=\\\"$SERVICE\\\" AND metric.type=\\\"run.googleapis.com/request_count\\\" AND metric.label.\\\"response_code_class\\\"=\\\"5xx\\\"\",\"denominatorFilter\":\"resource.type=\\\"cloud_run_revision\\\" AND resource.label.\\\"service_name\\\"=\\\"$SERVICE\\\" AND metric.type=\\\"run.googleapis.com/request_count\\\"\",\"comparison\":\"COMPARISON_GT\",\"thresholdValue\":0.01,\"duration\":\"300s\",\"aggregations\":[{\"alignmentPeriod\":\"60s\",\"perSeriesAligner\":\"ALIGN_RATE\",\"crossSeriesReducer\":\"REDUCE_SUM\"}],\"denominatorAggregations\":[{\"alignmentPeriod\":\"60s\",\"perSeriesAligner\":\"ALIGN_RATE\",\"crossSeriesReducer\":\"REDUCE_SUM\"}],\"trigger\":{\"count\":1}}}]}" )
ensure_json_policy "$five_xx_name" "$five_xx_policy"

memory_name="MCP: container memory above 80 percent"
memory_policy=$(printf '%s' "{\"displayName\":\"$memory_name\",\"combiner\":\"OR\",\"enabled\":true,\"conditions\":[{\"displayName\":\"memory utilization > 80% for 5m\",\"conditionThreshold\":{\"filter\":\"resource.type=\\\"cloud_run_revision\\\" AND resource.label.\\\"service_name\\\"=\\\"$SERVICE\\\" AND metric.type=\\\"run.googleapis.com/container/memory/utilizations\\\"\",\"comparison\":\"COMPARISON_GT\",\"thresholdValue\":0.8,\"duration\":\"300s\",\"aggregations\":[{\"alignmentPeriod\":\"60s\",\"perSeriesAligner\":\"ALIGN_PERCENTILE_99\",\"crossSeriesReducer\":\"REDUCE_MAX\"}],\"trigger\":{\"count\":1}}}]}" )
ensure_json_policy "$memory_name" "$memory_policy"

echo "observability $MODE: OK"
