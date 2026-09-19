#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/lib.sh" "${1:-check}"
require_commands gcloud

apis=(
  artifactregistry.googleapis.com cloudbuild.googleapis.com compute.googleapis.com
  dns.googleapis.com firestore.googleapis.com logging.googleapis.com
  monitoring.googleapis.com run.googleapis.com secretmanager.googleapis.com
)

enabled=$(gcloud services list --project "$GCP_PROJECT" --enabled --format='value(config.name)')
for api in "${apis[@]}"; do
  if ! grep -qx "$api" <<<"$enabled"; then
    if [[ "$MODE" == "apply" ]]; then
      gcloud services enable "$api" --project "$GCP_PROJECT" --quiet
    else
      echo "missing enabled API: $api" >&2
      exit 1
    fi
  fi
done
if [[ "$FIRESTORE_PROJECT" != "$GCP_PROJECT" ]] && ! gcloud services list --project "$FIRESTORE_PROJECT" --enabled --format='value(config.name)' | grep -qx firestore.googleapis.com; then
  if [[ "$MODE" == "apply" ]]; then
    gcloud services enable firestore.googleapis.com --project "$FIRESTORE_PROJECT" --quiet
  else
    echo "Firestore API is not enabled in $FIRESTORE_PROJECT" >&2
    exit 1
  fi
fi

if ! gcloud artifacts repositories describe "$AR_REPO" --project "$GCP_PROJECT" --location "$GCP_REGION" >/dev/null 2>&1; then
  if [[ "$MODE" == "apply" ]]; then
    gcloud artifacts repositories create "$AR_REPO" --project "$GCP_PROJECT" --location "$GCP_REGION" --repository-format docker --quiet
  else
    echo "missing Artifact Registry repository: $AR_REPO" >&2
    exit 1
  fi
fi

if gcloud firestore databases describe --project "$FIRESTORE_PROJECT" --database "$FIRESTORE_DATABASE" >/dev/null 2>&1; then
  actual_location=$(gcloud firestore databases describe --project "$FIRESTORE_PROJECT" --database "$FIRESTORE_DATABASE" --format='value(locationId)')
  actual_type=$(gcloud firestore databases describe --project "$FIRESTORE_PROJECT" --database "$FIRESTORE_DATABASE" --format='value(type)')
  if [[ "$actual_location" != "$FIRESTORE_LOCATION" ]]; then
    echo "existing Firestore location is $actual_location, expected $FIRESTORE_LOCATION; refusing to change it" >&2
    exit 1
  fi
  if [[ "$actual_type" != "FIRESTORE_NATIVE" ]]; then
    echo "existing database is not Firestore Native: $actual_type" >&2
    exit 1
  fi
elif [[ "$MODE" == "apply" ]]; then
  gcloud firestore databases create --project "$FIRESTORE_PROJECT" --database "$FIRESTORE_DATABASE" --location "$FIRESTORE_LOCATION" --type firestore-native --delete-protection --quiet
else
  echo "missing Firestore database: $FIRESTORE_DATABASE" >&2
  exit 1
fi

for collection in mcp_oauth_states mcp_oauth_codes mcp_oauth_grants; do
  ttl_name=$(gcloud firestore fields ttls list --project "$FIRESTORE_PROJECT" --database "$FIRESTORE_DATABASE" --collection-group "$collection" --format='value(name)')
  if ! grep -q '/fields/expires_at$' <<<"$ttl_name"; then
    if [[ "$MODE" == "apply" ]]; then
      gcloud firestore fields ttls update expires_at --project "$FIRESTORE_PROJECT" --database "$FIRESTORE_DATABASE" --collection-group "$collection" --enable-ttl --quiet
    else
      echo "TTL on $collection.expires_at is not enabled" >&2
      exit 1
    fi
  fi
done

sa_name=${RUNTIME_SA%@*}
if ! gcloud iam service-accounts describe "$RUNTIME_SA" --project "$GCP_PROJECT" >/dev/null 2>&1; then
  if [[ "$MODE" == "apply" ]]; then
    gcloud iam service-accounts create "$sa_name" --project "$GCP_PROJECT" --display-name "MCP GCP Observability runtime" --quiet
  else
    echo "missing service account: $RUNTIME_SA" >&2
    exit 1
  fi
fi

if [[ "$MODE" == "apply" ]]; then
  gcloud projects add-iam-policy-binding "$FIRESTORE_PROJECT" --member "serviceAccount:$RUNTIME_SA" --role roles/datastore.user --condition=None --quiet >/dev/null
else
  roles=$(gcloud projects get-iam-policy "$FIRESTORE_PROJECT" --flatten='bindings[].members' --filter="bindings.members:serviceAccount:$RUNTIME_SA" --format='value(bindings.role)')
  grep -qx roles/datastore.user <<<"$roles" || { echo "$RUNTIME_SA lacks roles/datastore.user" >&2; exit 1; }
fi

for secret in "$GOOGLE_SECRET" "$TOKEN_SECRET"; do
  gcloud secrets describe "$secret" --project "$GCP_PROJECT" >/dev/null 2>&1 || {
    echo "required Secret Manager resource is missing: $secret (create its value out of band)" >&2
    exit 1
  }
  if [[ "$MODE" == "apply" ]]; then
    gcloud secrets add-iam-policy-binding "$secret" --project "$GCP_PROJECT" --member "serviceAccount:$RUNTIME_SA" --role roles/secretmanager.secretAccessor --quiet >/dev/null
  else
    secret_roles=$(gcloud secrets get-iam-policy "$secret" --project "$GCP_PROJECT" --flatten='bindings[].members' --filter="bindings.members:serviceAccount:$RUNTIME_SA" --format='value(bindings.role)')
    grep -qx roles/secretmanager.secretAccessor <<<"$secret_roles" || {
      echo "$RUNTIME_SA lacks roles/secretmanager.secretAccessor on $secret" >&2
      exit 1
    }
  fi
done

echo "bootstrap $MODE: OK"
