#!/usr/bin/env bash
# Checks a deployed booth-streamlit. Run after deploy-standins.sh, or after deploy-realcore.sh with
# REAL_CORE=1 for the checks only a real booth-core can satisfy.
#
# In-cluster HTTP goes through the API server's service proxy (`kubectl get --raw`), not
# `kubectl run curl`: the latter has an attach race that loses short-lived output (the cause of
# booth-catalog's flaky Integration runs, ADR 0099's applied notes).
set -euo pipefail

ns=booth-streamlit
svc_proxy="/api/v1/namespaces/$ns/services/http:booth-streamlit:8080/proxy"
fail() { echo "FAIL: $*" >&2; exit 1; }

if [ "${REAL_CORE:-}" = "1" ]; then
  echo "--- booth-core provisions the module's database and bus credential (ADR 0053, ADR 0050)"
  for s in booth-database-credentials booth-event-bus-credentials; do
    for _ in $(seq 1 90); do
      kubectl -n "$ns" get secret "$s" >/dev/null 2>&1 && break
      sleep 2
    done
    kubectl -n "$ns" get secret "$s" >/dev/null 2>&1 || fail "booth-core never wrote Secret $s into $ns"
    echo "ok: $s"
  done
  for key in dsn host port database username password; do
    test -n "$(kubectl -n "$ns" get secret booth-database-credentials -o jsonpath="{.data.$key}")" \
      || fail "booth-database-credentials has no $key"
  done
  for key in nats.creds url; do
    test -n "$(kubectl -n "$ns" get secret booth-event-bus-credentials -o jsonpath="{.data.${key//./\\.}}")" \
      || fail "booth-event-bus-credentials has no $key"
  done
fi

echo "--- the backend rolls out"
kubectl -n "$ns" rollout status deployment/booth-streamlit --timeout=300s

echo "--- registration (ADR 0019): the live resource, after core's CRD schema has pruned anything unknown"
bm() { kubectl -n "$ns" get boothmodules.booth.projectbooth.io streamlit -o jsonpath="$1"; }
test "$(bm '{.spec.uiIntegrationMode}')" = "iframe-proxy" || fail "uiIntegrationMode"
test "$(bm '{.spec.navGroup}')" = "build" || fail "navGroup"
test "$(bm '{.spec.navPath}')" = "/streamlit" || fail "navPath"
test "$(bm '{.spec.healthCheckPath}')" = "/healthz" || fail "healthCheckPath"
# ADR 0050: without this core mints no credential. Checked live, because a CRD that doesn't know a
# field drops it silently.
test "$(bm '{.spec.events.publish[0]}')" = "dashboard.*" || fail "events.publish was pruned or wrong"
if [ "${REAL_CORE:-}" = "1" ]; then
  test "$(bm '{.spec.database.enabled}')" = "true" || fail "database.enabled was pruned or wrong"
fi

echo "--- /healthz: database ok and the event bus connected"
out=""
for _ in $(seq 1 60); do
  out="$(kubectl get --raw "$svc_proxy/healthz" 2>/dev/null || true)"
  if echo "$out" | grep -q '"database":"ok"' && echo "$out" | grep -q '"eventBus":"connected"' && echo "$out" | grep -q '"status":"ok"'; then
    break
  fi
  sleep 2
done
echo "$out"
echo "$out" | grep -q '"database":"ok"' || fail "database not ok"
echo "$out" | grep -q '"eventBus":"connected"' || fail "event bus never connected"
echo "$out" | grep -q '"status":"ok"' || fail "status not ok"

echo "--- the UI is served (relative asset URLs, so it works under /iframe/streamlit/)"
page="$(kubectl get --raw "$svc_proxy/")"
echo "$page" | grep -q '<title>Streamlit</title>' || fail "UI index not served: $page"
echo "$page" | grep -q 'src="./assets/' || fail "asset URLs are not relative: $page"

echo "--- no Kubernetes API access for the module's service account"
sa="system:serviceaccount:$ns:booth-streamlit"
for args in "get secrets -n $ns" "list pods -n $ns" "create pods -n $ns" "get secrets -n kube-system"; do
  test "$(kubectl auth can-i $args --as="$sa")" = "no" || fail "unexpectedly ALLOWED: $args"
done

if [ "${REAL_CORE:-}" = "1" ]; then
  echo "--- booth-core's health reconciler sees the module as Healthy"
  phase=""
  for _ in $(seq 1 60); do
    phase="$(bm '{.status.phase}')"
    [ "$phase" = "Healthy" ] && break
    sleep 2
  done
  test "$phase" = "Healthy" || fail "BoothModule status.phase = '$phase', want Healthy"
fi

echo "all checks passed"
