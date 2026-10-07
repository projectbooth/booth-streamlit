#!/usr/bin/env bash
# Deploys a real booth-core (built from a pinned checkout) and then booth-streamlit with its chart
# defaults, so core itself provisions the module's database (ADR 0053) and mints its event-bus
# credential from the manifest's `events` (ADR 0050). This is the path a real install takes.
#
#   test/integration/deploy-realcore.sh <booth-core checkout> <core image> <streamlit image>
#
# Both images must already be loaded into the cluster (pullPolicy Never). No identity provider is
# deployed: core retries OIDC discovery in the background and serves the module registry,
# provisioning and the bus without it, and nothing in the scaffold needs a signed-in user. The
# iframe-identity path will need Keycloak here once the module verifies X-Booth-Identity (see
# booth-logging's test/integration/realcore for the pattern).
set -euo pipefail

core_dir=$1
core_image=$2
image=$3
ns=booth-streamlit
repo=$(cd "$(dirname "$0")/../.." && pwd)

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

echo "--- booth-streamlit with chart defaults (database and bus credential from core)"
kubectl create namespace "$ns" --dry-run=client -o yaml | kubectl apply -f -
# No --wait on purpose: the pod cannot start until core has written both Secrets, which it does
# only after it sees the BoothModule this install creates. verify.sh waits for that explicitly.
helm upgrade --install booth-streamlit "$repo/charts/booth-streamlit" --namespace "$ns" \
  --set image.repository="${image%:*}" --set image.tag="${image##*:}" --set image.pullPolicy=Never
