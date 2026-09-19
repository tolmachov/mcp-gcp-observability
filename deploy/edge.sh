#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/lib.sh" "${1:-check}"
require_commands gcloud python3

NEG=${RESOURCE_PREFIX}-neg
BACKEND=${RESOURCE_PREFIX}-backend
CANDIDATE_NEG=${RESOURCE_PREFIX}-candidate-neg
CANDIDATE_BACKEND=${RESOURCE_PREFIX}-candidate-backend
POLICY=${RESOURCE_PREFIX}-armor
IP_NAME=${RESOURCE_PREFIX}-ip
URL_MAP=${RESOURCE_PREFIX}-url-map
CERT=${RESOURCE_PREFIX}-cert
HTTPS_PROXY=${RESOURCE_PREFIX}-https-proxy
FORWARDING_RULE=${RESOURCE_PREFIX}-https

ensure_resource() {
  local description=$1
  shift
  if ! "$@" >/dev/null 2>&1; then
    if [[ "$MODE" == "check" ]]; then
      echo "missing $description" >&2
      exit 1
    fi
    return 1
  fi
}

ensure_resource "global IP $IP_NAME" gcloud compute addresses describe "$IP_NAME" --project "$GCP_PROJECT" --global || \
  gcloud compute addresses create "$IP_NAME" --project "$GCP_PROJECT" --global --ip-version IPV4 --quiet

ensure_resource "serverless NEG $NEG" gcloud compute network-endpoint-groups describe "$NEG" --project "$GCP_PROJECT" --region "$GCP_REGION" || \
  gcloud compute network-endpoint-groups create "$NEG" --project "$GCP_PROJECT" --region "$GCP_REGION" --network-endpoint-type serverless --cloud-run-service "$SERVICE" --quiet
neg_service=$(gcloud compute network-endpoint-groups describe "$NEG" --project "$GCP_PROJECT" --region "$GCP_REGION" --format='value(cloudRun.service)')
[[ "$neg_service" == "$SERVICE" ]] || { echo "$NEG targets $neg_service, expected $SERVICE" >&2; exit 1; }

ensure_resource "candidate serverless NEG $CANDIDATE_NEG" gcloud compute network-endpoint-groups describe "$CANDIDATE_NEG" --project "$GCP_PROJECT" --region "$GCP_REGION" || \
  gcloud compute network-endpoint-groups create "$CANDIDATE_NEG" --project "$GCP_PROJECT" --region "$GCP_REGION" --network-endpoint-type serverless --cloud-run-service "$SERVICE" --cloud-run-tag candidate --quiet
candidate_neg_target=$(gcloud compute network-endpoint-groups describe "$CANDIDATE_NEG" --project "$GCP_PROJECT" --region "$GCP_REGION" --format='value(cloudRun.service,cloudRun.tag)')
[[ "$candidate_neg_target" == "$SERVICE"$'\t'"candidate" ]] || { echo "$CANDIDATE_NEG must target $SERVICE tag candidate, got $candidate_neg_target" >&2; exit 1; }

ensure_resource "backend service $BACKEND" gcloud compute backend-services describe "$BACKEND" --project "$GCP_PROJECT" --global || \
  gcloud compute backend-services create "$BACKEND" --project "$GCP_PROJECT" --global --load-balancing-scheme EXTERNAL_MANAGED --protocol HTTP --enable-logging --quiet

ensure_resource "candidate backend service $CANDIDATE_BACKEND" gcloud compute backend-services describe "$CANDIDATE_BACKEND" --project "$GCP_PROJECT" --global || \
  gcloud compute backend-services create "$CANDIDATE_BACKEND" --project "$GCP_PROJECT" --global --load-balancing-scheme EXTERNAL_MANAGED --protocol HTTP --enable-logging --quiet

backends=$(gcloud compute backend-services describe "$BACKEND" --project "$GCP_PROJECT" --global --format='value(backends.group)')
if ! grep -q "/networkEndpointGroups/$NEG" <<<"$backends"; then
  if [[ "$MODE" == "apply" ]]; then
    gcloud compute backend-services add-backend "$BACKEND" --project "$GCP_PROJECT" --global --network-endpoint-group "$NEG" --network-endpoint-group-region "$GCP_REGION" --quiet
  else
    echo "$BACKEND is not attached to $NEG" >&2
    exit 1
  fi
fi

candidate_backends=$(gcloud compute backend-services describe "$CANDIDATE_BACKEND" --project "$GCP_PROJECT" --global --format='value(backends.group)')
if ! grep -q "/networkEndpointGroups/$CANDIDATE_NEG" <<<"$candidate_backends"; then
  if [[ "$MODE" == "apply" ]]; then
    gcloud compute backend-services add-backend "$CANDIDATE_BACKEND" --project "$GCP_PROJECT" --global --network-endpoint-group "$CANDIDATE_NEG" --network-endpoint-group-region "$GCP_REGION" --quiet
  else
    echo "$CANDIDATE_BACKEND is not attached to $CANDIDATE_NEG" >&2
    exit 1
  fi
fi

scheme=$(gcloud compute backend-services describe "$BACKEND" --project "$GCP_PROJECT" --global --format='value(loadBalancingScheme)')
[[ "$scheme" == "EXTERNAL_MANAGED" ]] || { echo "$BACKEND uses unexpected load balancing scheme: $scheme" >&2; exit 1; }

ensure_resource "Cloud Armor policy $POLICY" gcloud compute security-policies describe "$POLICY" --project "$GCP_PROJECT" || \
  gcloud compute security-policies create "$POLICY" --project "$GCP_PROJECT" --type CLOUD_ARMOR --quiet

upsert_rule() {
  local priority=$1
  shift
  if gcloud compute security-policies rules describe "$priority" --security-policy "$POLICY" --project "$GCP_PROJECT" >/dev/null 2>&1; then
    if [[ "$MODE" == "apply" ]]; then
      gcloud compute security-policies rules update "$priority" --security-policy "$POLICY" --project "$GCP_PROJECT" "$@" --quiet
    fi
  elif [[ "$MODE" == "apply" ]]; then
    gcloud compute security-policies rules create "$priority" --security-policy "$POLICY" --project "$GCP_PROJECT" "$@" --quiet
  else
    echo "missing Cloud Armor rule priority $priority" >&2
    exit 1
  fi
}

oauth_expression="request.path.matches('^/(authorize|authorize/confirm|callback|token|register|revoke)(/.*)?$')"
upsert_rule 100 --expression "$oauth_expression" --action rate-based-ban --rate-limit-threshold-count 60 --rate-limit-threshold-interval-sec 60 --ban-threshold-count 300 --ban-threshold-interval-sec 300 --ban-duration-sec 600 --conform-action allow --exceed-action deny-429 --enforce-on-key IP
upsert_rule 200 --src-ip-ranges='*' --action throttle --rate-limit-threshold-count 600 --rate-limit-threshold-interval-sec 60 --conform-action allow --exceed-action deny-429 --enforce-on-key IP

if [[ "$MODE" == "apply" ]]; then
  gcloud compute backend-services update "$BACKEND" --project "$GCP_PROJECT" --global --security-policy "$POLICY" --quiet
  gcloud compute backend-services update "$CANDIDATE_BACKEND" --project "$GCP_PROJECT" --global --security-policy "$POLICY" --quiet
fi
attached_policy=$(gcloud compute backend-services describe "$BACKEND" --project "$GCP_PROJECT" --global --format='value(securityPolicy)')
grep -q "/securityPolicies/$POLICY$" <<<"$attached_policy" || { echo "$BACKEND is not protected by $POLICY" >&2; exit 1; }
candidate_policy=$(gcloud compute backend-services describe "$CANDIDATE_BACKEND" --project "$GCP_PROJECT" --global --format='value(securityPolicy)')
grep -q "/securityPolicies/$POLICY$" <<<"$candidate_policy" || { echo "$CANDIDATE_BACKEND is not protected by $POLICY" >&2; exit 1; }

oauth_action=$(gcloud compute security-policies rules describe 100 --security-policy "$POLICY" --project "$GCP_PROJECT" --format='value(action)')
oauth_rate=$(gcloud compute security-policies rules describe 100 --security-policy "$POLICY" --project "$GCP_PROJECT" --format='value(rateLimitOptions.rateLimitThreshold.count,rateLimitOptions.rateLimitThreshold.intervalSec,rateLimitOptions.banThreshold.count,rateLimitOptions.banThreshold.intervalSec,rateLimitOptions.banDurationSec,rateLimitOptions.enforceOnKey)')
[[ ("$oauth_action" == "rate-based-ban" || "$oauth_action" == "rate_based_ban") && "$oauth_rate" == $'60\t60\t300\t300\t600\tIP' ]] || { echo "Cloud Armor OAuth rate rule has drifted: $oauth_action $oauth_rate" >&2; exit 1; }
general_action=$(gcloud compute security-policies rules describe 200 --security-policy "$POLICY" --project "$GCP_PROJECT" --format='value(action)')
general_rate=$(gcloud compute security-policies rules describe 200 --security-policy "$POLICY" --project "$GCP_PROJECT" --format='value(rateLimitOptions.rateLimitThreshold.count,rateLimitOptions.rateLimitThreshold.intervalSec,rateLimitOptions.enforceOnKey)')
[[ "$general_action" == "throttle" && "$general_rate" == $'600\t60\tIP' ]] || { echo "Cloud Armor general rate rule has drifted: $general_action $general_rate" >&2; exit 1; }

ensure_resource "URL map $URL_MAP" gcloud compute url-maps describe "$URL_MAP" --project "$GCP_PROJECT" --global || \
  gcloud compute url-maps create "$URL_MAP" --project "$GCP_PROJECT" --default-service "$BACKEND" --global --quiet
url_backend=$(gcloud compute url-maps describe "$URL_MAP" --project "$GCP_PROJECT" --global --format='value(defaultService)')
grep -q "/backendServices/$BACKEND$" <<<"$url_backend" || { echo "$URL_MAP does not route to $BACKEND" >&2; exit 1; }

route_state=$(gcloud compute url-maps describe "$URL_MAP" --project "$GCP_PROJECT" --global --format=json | python3 -c 'import json,sys
d=json.load(sys.stdin); host=sys.argv[1]; backend=sys.argv[2]; candidate=sys.argv[3]
matchers={m.get("name"):m for m in d.get("pathMatchers",[])}
m=matchers.get("mcp-routes")
host_ok=any(host in h.get("hosts",[]) and h.get("pathMatcher")=="mcp-routes" for h in d.get("hostRules",[]))
default_ok=bool(m and m.get("defaultService","").endswith("/backendServices/"+backend))
path_ok=bool(m and any("/__candidate/readyz" in r.get("paths",[]) and r.get("service","").endswith("/backendServices/"+candidate) for r in m.get("pathRules",[])))
print("ok" if host_ok and default_ok and path_ok else ("missing" if m is None and not host_ok else "drift"))' "$PUBLIC_HOSTNAME" "$BACKEND" "$CANDIDATE_BACKEND")
if [[ "$route_state" == "missing" && "$MODE" == "apply" ]]; then
  gcloud compute url-maps add-path-matcher "$URL_MAP" --project "$GCP_PROJECT" --global \
    --path-matcher-name mcp-routes --new-hosts "$PUBLIC_HOSTNAME" --default-service "$BACKEND" \
    --path-rules "/__candidate/readyz=$CANDIDATE_BACKEND" --quiet
elif [[ "$route_state" != "ok" ]]; then
  echo "$URL_MAP candidate readiness route is $route_state; refusing to overwrite an owned host/path matcher" >&2
  exit 1
fi

if gcloud compute ssl-certificates describe "$CERT" --project "$GCP_PROJECT" --global >/dev/null 2>&1; then
  domains=$(gcloud compute ssl-certificates describe "$CERT" --project "$GCP_PROJECT" --global --format='value(managed.domains)')
  grep -qw "$PUBLIC_HOSTNAME" <<<"$domains" || { echo "certificate $CERT has different domains; refusing replacement" >&2; exit 1; }
elif [[ "$MODE" == "apply" ]]; then
  gcloud compute ssl-certificates create "$CERT" --project "$GCP_PROJECT" --global --domains "$PUBLIC_HOSTNAME" --quiet
else
  echo "missing managed certificate: $CERT" >&2
  exit 1
fi

ensure_resource "HTTPS proxy $HTTPS_PROXY" gcloud compute target-https-proxies describe "$HTTPS_PROXY" --project "$GCP_PROJECT" --global || \
  gcloud compute target-https-proxies create "$HTTPS_PROXY" --project "$GCP_PROJECT" --global --url-map "$URL_MAP" --ssl-certificates "$CERT" --quiet
proxy_url_map=$(gcloud compute target-https-proxies describe "$HTTPS_PROXY" --project "$GCP_PROJECT" --global --format='value(urlMap)')
grep -q "/urlMaps/$URL_MAP$" <<<"$proxy_url_map" || { echo "$HTTPS_PROXY does not use $URL_MAP" >&2; exit 1; }

ensure_resource "HTTPS forwarding rule $FORWARDING_RULE" gcloud compute forwarding-rules describe "$FORWARDING_RULE" --project "$GCP_PROJECT" --global || \
  gcloud compute forwarding-rules create "$FORWARDING_RULE" --project "$GCP_PROJECT" --global --load-balancing-scheme EXTERNAL_MANAGED --network-tier PREMIUM --address "$IP_NAME" --target-https-proxy "$HTTPS_PROXY" --ports 443 --quiet

lb_ip=$(gcloud compute addresses describe "$IP_NAME" --project "$GCP_PROJECT" --global --format='value(address)')
if gcloud dns record-sets describe "$PUBLIC_HOSTNAME." --project "$GCP_PROJECT" --zone "$DNS_ZONE" --type A >/dev/null 2>&1; then
  current_ip=$(gcloud dns record-sets describe "$PUBLIC_HOSTNAME." --project "$GCP_PROJECT" --zone "$DNS_ZONE" --type A --format='value(rrdatas)')
  if [[ "$current_ip" != "$lb_ip" ]]; then
    if [[ "$MODE" == "apply" ]]; then
      gcloud dns record-sets update "$PUBLIC_HOSTNAME." --project "$GCP_PROJECT" --zone "$DNS_ZONE" --type A --ttl 300 --rrdatas "$lb_ip" --quiet
    else
      echo "DNS A record points to $current_ip, expected $lb_ip" >&2
      exit 1
    fi
  fi
elif [[ "$MODE" == "apply" ]]; then
  gcloud dns record-sets create "$PUBLIC_HOSTNAME." --project "$GCP_PROJECT" --zone "$DNS_ZONE" --type A --ttl 300 --rrdatas "$lb_ip" --quiet
else
  echo "missing DNS A record for $PUBLIC_HOSTNAME" >&2
  exit 1
fi

echo "edge $MODE: OK ($PUBLIC_HOSTNAME -> $lb_ip)"
