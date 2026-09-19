#!/usr/bin/env bash
set -euo pipefail

ACTION=${1:-check-stable}
case "$ACTION" in
  candidate|stable) source "$(dirname "$0")/lib.sh" apply ;;
  check-candidate|check-stable) source "$(dirname "$0")/lib.sh" check ;;
  *) echo "usage: $0 candidate|stable|check-candidate|check-stable" >&2; exit 2 ;;
esac
require_commands gcloud python3

URL_MAP=${RESOURCE_PREFIX}-url-map
BACKEND=${RESOURCE_PREFIX}-backend
CANDIDATE_BACKEND=${RESOURCE_PREFIX}-candidate-backend
oauth_paths=(/.well-known/oauth-protected-resource /.well-known/oauth-authorization-server /.well-known/openid-configuration /jwks.json /register /authorize /authorize/confirm /callback /token /revoke)

# Only replace our exact, dedicated host matcher. Refuse shared hosts or
# additional routing policies rather than discarding operator configuration.
route_state=$(gcloud compute url-maps describe "$URL_MAP" --project "$GCP_PROJECT" --global --format=json | python3 -c 'import json,sys
d=json.load(sys.stdin); host,backend,candidate=sys.argv[1:4]; oauth=sys.argv[4:]
hosts=[h for h in d.get("hostRules",[]) if host in h.get("hosts",[])]
if len(hosts)!=1 or hosts[0]["hosts"]!=[host]:
    sys.exit("expected one dedicated public host rule")
name=hosts[0]["pathMatcher"]
if sum(h.get("pathMatcher")==name for h in d["hostRules"])!=1:
    sys.exit("refusing to replace a shared path matcher")
matchers=[m for m in d.get("pathMatchers",[]) if m.get("name")==name]
if len(matchers)!=1 or name not in ("mcp-routes","mcp-cutover"):
    sys.exit("unexpected public path matcher")
m=matchers[0]
if set(m)-{"name","description","defaultService","pathRules"} or not m.get("defaultService","").endswith("/backendServices/"+backend):
    sys.exit("public path matcher has unowned configuration")
paths=[]
for rule in m.get("pathRules",[]):
    if set(rule)-{"paths","service"} or not rule.get("service","").endswith("/backendServices/"+candidate):
        sys.exit("unexpected path rule")
    paths.extend(rule["paths"])
expected=["/__candidate/readyz"]+(oauth if name=="mcp-cutover" else [])
if sorted(paths)!=sorted(expected):
    sys.exit("unexpected public path rules")
print("candidate" if name=="mcp-cutover" else "stable")' "$PUBLIC_HOSTNAME" "$BACKEND" "$CANDIDATE_BACKEND" "${oauth_paths[@]}")

if [[ "$ACTION" == check-* ]]; then
  [[ "$route_state" == "${ACTION#check-}" ]] || { echo "OAuth routes are $route_state, expected ${ACTION#check-}" >&2; exit 1; }
  exit 0
fi

if [[ "$ACTION" == "candidate" ]]; then
  [[ -n "${CANDIDATE_REVISION:-}" ]] || { echo "CANDIDATE_REVISION from rollout prepare is required" >&2; exit 2; }
  tagged_revision=$(gcloud run services describe "$SERVICE" --project "$GCP_PROJECT" --region "$GCP_REGION" --format=json | python3 -c 'import json,sys
print(next((x["revisionName"] for x in json.load(sys.stdin)["status"]["traffic"] if x.get("tag")=="candidate"), ""))')
  [[ "$tagged_revision" == "$CANDIDATE_REVISION" ]] || { echo "candidate tag changed; prepare again" >&2; exit 1; }
fi

if [[ "$route_state" == "$ACTION" ]]; then
  echo "OAuth routes already target $ACTION"
  exit 0
fi

matcher=mcp-routes
rules="/__candidate/readyz=$CANDIDATE_BACKEND"
if [[ "$ACTION" == "candidate" ]]; then
  matcher=mcp-cutover
  for path in "${oauth_paths[@]}"; do
    rules+=",$path=$CANDIDATE_BACKEND"
  done
fi
gcloud compute url-maps add-path-matcher "$URL_MAP" --project "$GCP_PROJECT" --global \
  --path-matcher-name "$matcher" --existing-host "$PUBLIC_HOSTNAME" --delete-orphaned-path-matcher \
  --default-service "$BACKEND" --path-rules "$rules" --quiet
echo "OAuth routes now target $ACTION; allow load-balancer propagation before authorizing."
