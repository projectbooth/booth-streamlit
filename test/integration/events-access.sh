#!/usr/bin/env bash
# Data access, step e: lineage and dashboard events (ADR 0018/0046/0050; docs/design-data-access.md
# item 6), against a real booth-core (its NATS and JetStream stream), the real booth-catalog and its
# real dashboard subscriber, after deploy-realcore.sh. Runs last; uses owner2-user (data-access.sh
# leaves owner-user without a role in acme-analytics) and the "files data" dataset files-access.sh
# registered.
#
#   1. the editor's dataset list, read as the owner through booth-catalog
#   2. a shared app with a declared dataset appears in booth-catalog as a dashboard, with its
#      lineage edge to that dataset (both directions)
#   3. an edit updates it
#   4. unsharing removes it
#   5. with NATS stopped, a change waits in the outbox (/healthz says so); NATS back, it arrives
set -euo pipefail
source "$(cd "$(dirname "$0")" && pwd)/lib.sh"

step "sign in owner2-user (an owner of acme-analytics)"
s=$(sessions owner2-user)
owner2=$(field "$s" owner2-user 3)
[ ${#owner2} -gt 20 ] || fail "could not open a session"

# One helper pod in the cluster for reading booth-catalog as owner2-user through core's gateway
# (tokens must carry Keycloak's in-cluster issuer).
kubectl -n "$probe_ns" delete pod catalog-reader --ignore-not-found --wait >/dev/null 2>&1
kubectl -n "$probe_ns" run catalog-reader --restart=Never --image=curlimages/curl --command -- sleep 3600 >/dev/null
kubectl -n "$probe_ns" wait --for=condition=Ready pod/catalog-reader --timeout=120s >/dev/null
cleanup_reader() { kubectl -n "$probe_ns" delete pod catalog-reader --wait=false >/dev/null 2>&1 || true; }
catalog() { # PATH: prints "<status> <body>"
  kubectl -n "$probe_ns" exec catalog-reader -- sh -c '
t=$(curl -s -X POST http://keycloak.keycloak.svc:8080/realms/booth/protocol/openid-connect/token -d grant_type=password -d client_id=booth-design -d scope=openid -d username=owner2-user -d "password=$1" | sed -n "s/.*\"access_token\":\"\([^\"]*\)\".*/\1/p")
curl -s -o /tmp/b -w "%{http_code} " -H "Authorization: Bearer $t" -H "X-Workspace: acme-analytics" "http://booth-core.booth-system.svc:8080/modules/catalog$2"; cat /tmp/b' _ "$password" "$1"
}
# dashboard_of APP-ID: the catalog's dashboard for the app ("" when there is none)
dashboard_of() {
  catalog "/api/dashboards?source=streamlit&limit=200" | cut -d' ' -f2- |
    node -e 'const d=JSON.parse(require("fs").readFileSync(0,"utf8")); const x=(d.items||[]).find(i=>i.externalId===process.argv[1]); process.stdout.write(x?JSON.stringify(x):"")' "$1"
}
wait_dashboard() { # APP-ID present|absent SECONDS [NAME]
  local d=""
  for _ in $(seq 1 "$3"); do
    d=$(dashboard_of "$1")
    if [ "$2" = present ] && [ -n "$d" ] && { [ -z "${4:-}" ] || [ "$(echo "$d" | json 'd.name')" = "$4" ]; }; then echo "$d"; return 0; fi
    if [ "$2" = absent ] && [ -z "$d" ]; then return 0; fi
    sleep 1
  done
  fail "dashboard for $1 not $2${4:+ (name $4)} within $3s; last: $d"
}

step "1. the editor's dataset list is booth-catalog's, read as the owner"
api "$owner2" GET /api/catalog/datasets
[ "$code" = 200 ] || fail "listing datasets: $code $body"
echo "$body" | head -c 400; echo
ds=$(echo "$body" | json '(d.datasets.find(x=>x.name==="files data")||{}).id')
[ -n "$ds" ] || fail "the 'files data' dataset (registered by files-access.sh) is not in the list"
echo "dataset: $ds"

step "2. a shared app with a declared dataset appears in booth-catalog, with its lineage edge"
src=$(node -e 'process.stdout.write(JSON.stringify("import streamlit as st\nst.write(\"lineage app\")\n"))')
input() { echo "{\"name\":\"$1\",\"description\":\"Revenue by region\",\"source\":$src,\"requirements\":\"\",\"sources\":[\"$ds\"],\"shared\":$2}"; }
api "$owner2" POST /api/apps "$(input "Lineage app" true)"; [ "$code" = 201 ] || fail "create: $code $body"
A=$(echo "$body" | json 'd.id')
echo "app: $A"
d=$(wait_dashboard "$A" present 60)
echo "dashboard: $d"
cid=$(echo "$d" | json 'd.id')
[ "$(echo "$d" | json 'd.sourceModule')" = streamlit ] && [ "$(echo "$d" | json 'd.path')" = /streamlit ] && [ "$(echo "$d" | json 'd.lineageComplete')" = false ] \
  && [ "$(echo "$d" | json 'd.name')" = "Lineage app" ] || fail "dashboard fields"
detail=$(catalog "/api/dashboards/$cid" | cut -d' ' -f2-)
echo "lineage: $(echo "$detail" | json 'JSON.stringify(d.lineage)')"
resolved=$(echo "$detail" | DS=$ds json 'd.lineage.sources.length===1 && d.lineage.sources[0].type==="dataset" && d.lineage.sources[0].datasetId===process.env.DS && (d.lineage.sources[0].datasets||[]).some(x=>x.id===process.env.DS)')
[ "$resolved" = true ] || fail "the dashboard's lineage does not resolve to the dataset"
down=$(catalog "/api/datasets/$ds/lineage" | cut -d' ' -f2-)
[ "$(echo "$down" | CID=$cid json '(d.dashboards||[]).some(x=>x.id===process.env.CID)')" = true ] || fail "the dataset's lineage does not list the dashboard: $down"
echo "the dataset's downstream lineage lists the dashboard"

step "3. an edit updates it"
api "$owner2" PUT "/api/apps/$A" "$(input "Lineage app v2" true)"; [ "$code" = 200 ] || fail "edit: $code $body"
wait_dashboard "$A" present 60 "Lineage app v2" >/dev/null
echo "renamed in the catalog"

step "4. unsharing removes it"
api "$owner2" PUT "/api/apps/$A" "$(input "Lineage app v2" false)"; [ "$code" = 200 ] || fail "unshare: $code $body"
wait_dashboard "$A" absent 60
[ "$(catalog "/api/dashboards/$cid" | cut -d' ' -f1)" = 404 ] || fail "the unshared app's dashboard is still readable"
echo "gone from the catalog"

step "5. NATS stopped: the change waits in the outbox; NATS back: it arrives"
kubectl -n "$ns" port-forward svc/booth-streamlit 18095:8080 >/tmp/pf-backend.log 2>&1 &
cleanup_pids+=($!)
for _ in $(seq 1 30); do curl -s -o /dev/null http://localhost:18095/livez && break; sleep 1; done
kubectl -n booth-system scale statefulset booth-core-nats --replicas=0 >/dev/null
# Conditions, not sleeps or swallowed failures: NATS's pod is gone, and the backend sees the bus down.
gone=""
for _ in $(seq 1 90); do kubectl -n booth-system get pod booth-core-nats-0 >/dev/null 2>&1 || { gone=1; break; }; sleep 2; done
[ -n "$gone" ] || fail "booth-core-nats-0 still exists after scaling NATS to zero"
bus=""
for _ in $(seq 1 60); do bus=$(curl -s http://localhost:18095/healthz | json 'd.eventBus'); [ "$bus" = connecting ] && break; sleep 1; done
[ "$bus" = connecting ] || fail "the backend never saw the bus go down (eventBus=$bus)"
echo "with NATS down: $(curl -s http://localhost:18095/healthz)"
api "$owner2" PUT "/api/apps/$A" "$(input "Lineage app v3" true)"; [ "$code" = 200 ] || fail "re-share: $code $body"
# The event is written in the change's own transaction, so it is pending as soon as the PUT returns.
h=""
for _ in $(seq 1 10); do h=$(curl -s http://localhost:18095/healthz); [ "$(echo "$h" | json 'd.eventsPending')" -ge 1 ] && break; sleep 1; done
echo "after the change: $h"
[ "$(echo "$h" | json 'd.eventsPending')" -ge 1 ] && [ "$(echo "$h" | json 'd.eventsFailed')" = 0 ] || fail "the event is not waiting in the outbox"
kubectl -n booth-system scale statefulset booth-core-nats --replicas=1 >/dev/null
kubectl -n booth-system rollout status statefulset booth-core-nats --timeout=300s >/dev/null
d=$(wait_dashboard "$A" present 180 "Lineage app v3")
echo "after NATS came back: $d"
h=$(curl -s http://localhost:18095/healthz); echo "healthz: $h"
[ "$(echo "$h" | json 'd.eventsPending')" = 0 ] || fail "events still pending after NATS came back"

api "$owner2" DELETE "/api/apps/$A"
wait_dashboard "$A" absent 60
cleanup_reader
echo "events access: all checks passed"
