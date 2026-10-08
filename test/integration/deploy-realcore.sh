#!/usr/bin/env bash
# Deploys a real Keycloak and a real booth-core (built from a pinned checkout), then booth-streamlit
# with its chart defaults, so core itself provisions the module's database (ADR 0053) and mints its
# event-bus credential from the manifest's `events` (ADR 0050), the path a real install takes.
# Also deploys the hand-started demo app (fixtures/demo-app.yaml) and points the module's
# static-app seam at it, for iframe-path.sh.
#
#   test/integration/deploy-realcore.sh <booth-core checkout> <core image> <streamlit image> <app-runtime image>
#
# All images must already be loaded into the cluster (pullPolicy Never). The test password is
# generated per run and kept in the Secret keycloak/realcore-test-password for iframe-path.sh.
set -euo pipefail

core_dir=$1
core_image=$2
image=$3
runtime_image=$4
ns=booth-streamlit
repo=$(cd "$(dirname "$0")/../.." && pwd)
here="$repo/test/integration/realcore"

echo "--- Keycloak (realm: owner, editor and viewer of acme-analytics; an owner of other-team)"
kubectl create namespace keycloak --dry-run=client -o yaml | kubectl apply -f - >/dev/null
password=$(openssl rand -hex 12)
kubectl -n keycloak create secret generic realcore-test-password --from-literal=password="$password" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n keycloak create secret generic keycloak-admin --from-literal=password="$(openssl rand -hex 12)" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null
realm=$(mktemp)
sed "s/__TEST_PASSWORD__/$password/g" "$here/realm-booth.json.tpl" >"$realm"
kubectl -n keycloak create configmap keycloak-realm --from-file=realm-booth.json="$realm" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null
rm -f "$realm"
kubectl apply -f "$here/keycloak.yaml" >/dev/null
# booth-core starts without Keycloak (it retries OIDC discovery), so don't wait here; wait at the end.

echo "--- booth-core from $core_dir ($(git -C "$core_dir" rev-parse --short HEAD 2>/dev/null || echo '?'))"
kubectl create namespace booth-system --dry-run=client -o yaml | kubectl apply -f -
# Core's own CRD, applied explicitly: helm never updates a CRD that already exists.
kubectl apply -f "$core_dir/charts/booth-core/crds/"
helm upgrade --install booth-core "$core_dir/charts/booth-core" --namespace booth-system \
  --set oidc.issuerUrl=http://keycloak.keycloak.svc:8080/realms/booth --set oidc.clientId=booth-design \
  --set-string iframeSigningKey="$(openssl rand -hex 32)" \
  --set image.repository="${core_image%:*}" --set image.tag="${core_image##*:}" --set image.pullPolicy=Never \
  --wait --timeout 10m
kubectl -n booth-system rollout status deploy/booth-core --timeout=300s

echo "--- the hand-started demo app (step 1; the lifecycle replaces it)"
kubectl create namespace "$ns" --dry-run=client -o yaml | kubectl apply -f -
sed "s|image: booth-streamlit-app-runtime:ci|image: $runtime_image|" "$repo/test/integration/fixtures/demo-app.yaml" \
  | kubectl -n "$ns" apply -f -

echo "--- booth-streamlit with chart defaults (database and bus credential from core)"
# No --wait on purpose: the pod cannot start until core has written both Secrets, which it does
# only after it sees the BoothModule this install creates. verify.sh waits for that explicitly.
helm upgrade --install booth-streamlit "$repo/charts/booth-streamlit" --namespace "$ns" \
  --set image.repository="${image%:*}" --set image.tag="${image##*:}" --set image.pullPolicy=Never \
  --set-json 'staticApps="{\"demo\":{\"workspace\":\"acme-analytics\",\"url\":\"http://demo-app:8501\"}}"'

kubectl -n "$ns" rollout status deploy/demo-app --timeout=300s
kubectl -n keycloak rollout status deploy/keycloak --timeout=600s
