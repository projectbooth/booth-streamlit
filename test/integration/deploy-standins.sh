#!/usr/bin/env bash
# Deploys booth-streamlit beside stand-in PostgreSQL and NATS, with booth-core's BoothModule CRD
# (vendored from booth-core's chart) but no booth-core. Covers: the chart installs, the
# BoothModule is accepted by core's real schema with no fields pruned, and the backend reaches
# its database and bus. It cannot cover anything core does (provisioning, credentials).
#
#   test/integration/deploy-standins.sh <image>     # image already loaded into the cluster
set -euo pipefail

image=$1
ns=booth-streamlit
repo=$(cd "$(dirname "$0")/../.." && pwd)

kubectl apply -f "$repo/test/integration/fixtures/boothmodule-crd.yaml"
kubectl create namespace "$ns" --dry-run=client -o yaml | kubectl apply -f -
kubectl -n "$ns" apply -f "$repo/test/integration/fixtures/standins.yaml"
kubectl -n "$ns" rollout status deployment/postgres --timeout=180s
kubectl -n "$ns" rollout status deployment/nats --timeout=180s

# The stand-in bus has no authentication and there is no core here to mint a credential, so the
# credentials Secret is switched off and the bus address given by hand; the database is ours, not
# core's. The real-core job covers both of core's halves.
helm upgrade --install booth-streamlit "$repo/charts/booth-streamlit" \
  --namespace "$ns" \
  --set image.repository="${image%:*}" --set image.tag="${image##*:}" --set image.pullPolicy=Never \
  --set postgres.provisionedByCore=false --set postgres.dsnSecret.name=booth-streamlit-db \
  --set eventBus.credentialsSecret.enabled=false --set nats.url=nats://nats:4222 \
  --wait --timeout 5m
