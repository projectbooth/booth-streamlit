#!/usr/bin/env bash
# Build step 3 against a real booth-core, after deploy-realcore.sh (which installs the module with
# apps.maxRunning=1 and a short apps.idleTimeout, see IDLE_SECONDS). Each section is one of the
# brief's required cases:
#
#   start    an owner's Start creates the app's Deployment/Service/ConfigMap/Secret and it becomes
#            ready; a real Chromium then opens it through core (iframe-path.sh)
#   gate     a request that forges X-Booth-User without the app's bearer is refused by the gate,
#            from a pod the NetworkPolicy lets in (labelled as the backend)
#   cap      a second Start at maxRunning=1 is refused (409) and creates no pod
#   idle     with no viewer for the idle timeout the app is stopped (suspended, 0 replicas), and a
#            viewer opening it wakes it
#   stop     an owner's Stop scales it to 0, and a viewer opening it does not wake it
#   reconcile the backend restarting leaves a running app's pod alone; deleting an app removes all
#            of its objects
#
# Owner and viewer sessions come from real Keycloak tokens and core's real iframe session; every
# API call goes through core's iframe proxy (port-forwarded), never to the module directly.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
ns=booth-streamlit
probe_ns=booth-streamlit-it-probe
port=${CORE_PORT:-18081}
idle=${IDLE_SECONDS:-60}
fail() { echo "FAIL: $*" >&2; exit 1; }
step() { echo "--- $*"; }
json() { node -e "const d=JSON.parse(require('fs').readFileSync(0,'utf8')); const v=($1); process.stdout.write(v===undefined?'':String(v))"; }

kubectl create namespace "$probe_ns" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

# probe NAMESPACE NAME SCRIPT [kubectl run args...]: run SCRIPT in a one-off curl pod and print its
# output (wait for completion, then read logs: no attach race).
probe() {
  local pns=$1 name=$2 script=$3 phase=""
  shift 3
  kubectl -n "$pns" delete pod "$name" --ignore-not-found --wait >/dev/null 2>&1
  kubectl -n "$pns" run "$name" --restart=Never --image=curlimages/curl "$@" \
    --command -- sh -c "$script" >/dev/null
  for _ in $(seq 1 120); do
    phase=$(kubectl -n "$pns" get pod "$name" -o jsonpath='{.status.phase}' 2>/dev/null)
    case "$phase" in Succeeded|Failed) break ;; esac
    sleep 1
  done
  kubectl -n "$pns" logs "pod/$name" 2>&1 || echo "(no logs; pod phase: ${phase:-unknown})"
  kubectl -n "$pns" delete pod "$name" --wait=false >/dev/null 2>&1 || true
}

password=$(kubectl -n keycloak get secret realcore-test-password -o jsonpath='{.data.password}' | base64 -d)

step "port-forward booth-core to localhost:$port"
kubectl -n booth-system port-forward svc/booth-core "$port:8080" >/tmp/booth-core-pf-lifecycle.log 2>&1 &
pf=$!
trap 'kill $pf 2>/dev/null || true' EXIT
for _ in $(seq 1 30); do curl -s -o /dev/null "http://localhost:$port/healthz" && break; sleep 1; done
core="http://localhost:$port"

step "owner and viewer sessions (real tokens, core's iframe session)"
read -r -d '' sessions <<'SH' || true
KC=http://keycloak.keycloak.svc:8080/realms/booth/protocol/openid-connect
CORE=http://booth-core.booth-system.svc:8080
session() {
  tok=$(curl -s -X POST "$KC/token" -d grant_type=password -d client_id=booth-design -d scope=openid \
    -d "username=$1" -d "password=$PW" | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p')
  for i in $(seq 1 30); do
    url=$(curl -s -H "Authorization: Bearer $tok" -H "X-Workspace: acme-analytics" "$CORE/api/modules/streamlit/iframe-url" \
      | sed -n 's/.*"url":"\([^"]*\)".*/\1/p')
    [ -n "$url" ] && break
    sleep 3
  done
  c=$(curl -s -o /dev/null -D - "$CORE$url" | tr -d '\r' | sed -n 's/^[Ss]et-[Cc]ookie: booth_iframe_session=\([^;]*\).*/\1/p')
  echo "$1 $c"
}
session owner-user
session viewer-user
SH
out=$(probe "$probe_ns" lifecycle-sessions "PW='$password'; $sessions")
owner=$(echo "$out" | sed -n 's/^owner-user //p')
viewer=$(echo "$out" | sed -n 's/^viewer-user //p')
[ ${#owner} -gt 20 ] && [ ${#viewer} -gt 20 ] || fail "could not open sessions (output redacted)"

# api COOKIE METHOD PATH [JSON]: sets $code and $body. Through core's iframe proxy.
api() {
  local args=(-s -o /tmp/lc-body -w '%{http_code}' -X "$2" -H "Cookie: booth_iframe_session=$1")
  [ -n "${4:-}" ] && args+=(-H 'Content-Type: application/json' -d "$4")
  code=$(curl "${args[@]}" "$core/iframe/streamlit$3")
  body=$(cat /tmp/lc-body)
}
state() { api "$owner" GET "/api/apps/$1"; echo "$body" | json 'd.status.state'; }
replicas() { kubectl -n "$ns" get deploy "app-$1" -o jsonpath='{.spec.replicas}' 2>/dev/null; }
wait_state() { # ID STATE SECONDS
  local s=""
  for _ in $(seq 1 "$3"); do s=$(state "$1"); [ "$s" = "$2" ] && return 0; sleep 1; done
  fail "app $1 is '$s' after $3s, want '$2'"
}

source=$(node -e 'process.stdout.write(JSON.stringify(require("fs").readFileSync(process.argv[1],"utf8")))' "$here/fixtures/demo_app.py")
mk() { echo "{\"name\":\"$1\",\"description\":\"\",\"source\":$source,\"shared\":true}"; }
api "$owner" POST /api/apps "$(mk 'Lifecycle A')"; [ "$code" = 201 ] || fail "create A: $code $body"
A=$(echo "$body" | json 'd.id')
api "$owner" POST /api/apps "$(mk 'Lifecycle B')"; [ "$code" = 201 ] || fail "create B: $code $body"
B=$(echo "$body" | json 'd.id')
echo "apps: A=$A B=$B"

step "start: an owner's Start runs the app"
[ "$(replicas "$A")" = 0 ] || fail "a new app should be created stopped (0 replicas), got '$(replicas "$A")'"
api "$owner" POST "/api/apps/$A/start"; [ "$code" = 200 ] || fail "start A: $code $body"
wait_state "$A" running 180
for o in "deploy/app-$A" "svc/app-$A" "configmap/app-$A-src" "secret/app-$A-gate"; do
  kubectl -n "$ns" get "$o" >/dev/null || fail "$o was not created"
done
test "$(kubectl -n "$ns" get deploy "app-$A" -o jsonpath='{.status.readyReplicas}')" = 1 || fail "not ready"
pod=$(kubectl -n "$ns" get pod -l "booth.projectbooth.io/app-id=$A" -o jsonpath='{.items[0].metadata.name}')
test "$(kubectl -n "$ns" get pod "$pod" -o jsonpath='{.metadata.labels.booth\.projectbooth\.io/workspace}')" = acme-analytics || fail "workspace label"
test "$(kubectl -n "$ns" get pod "$pod" -o jsonpath='{.spec.automountServiceAccountToken}')" = false || fail "app pod mounts a token"
echo "ok: $A running ($pod)"

step "start: Chromium opens it through core (viewer)"
APP_ID="$A" CORE_PORT=$((port + 1)) "$here/iframe-path.sh"

step "gate: a forged X-Booth-User without the bearer is refused"
# Labelled as the backend, so the NetworkPolicy lets it in: what stops it is the gate. Limits
# because the namespace has a quota (strategic merge, so only resources are added to the container).
zeros=0000000000000000000000000000000000000000000000000000000000000000
url="http://app-$A:8080/apps/$A/"
gate_script="echo none \$(curl -s -o /dev/null -w '%{http_code}' -m 10 -H 'X-Booth-User: someone-else' -H 'X-Booth-Role: owner' $url)
echo wrong \$(curl -s -o /dev/null -w '%{http_code}' -m 10 -H 'X-Booth-User: someone-else' -H 'X-Booth-Gate-Token: $zeros' $url)"
out=$(probe "$ns" gate-forged "$gate_script" \
  --labels=app.kubernetes.io/name=booth-streamlit,app.kubernetes.io/instance=booth-streamlit \
  --override-type=strategic \
  --overrides='{"spec":{"containers":[{"name":"gate-forged","resources":{"limits":{"cpu":"100m","memory":"64Mi","ephemeral-storage":"16Mi"}}}]}}')
echo "$out"
echo "$out" | grep -qx "none 401" || fail "no bearer: not refused by the gate"
echo "$out" | grep -qx "wrong 401" || fail "wrong bearer: not refused by the gate"
kubectl -n "$ns" logs "$pod" -c gate --tail=20 | grep -q "refused GET /apps/$A/" || fail "the gate did not log the refusal"
# Recorded, not asserted: an ordinary pod elsewhere, which the NetworkPolicy should block entirely
# (only if this cluster's CNI enforces NetworkPolicy, ARCHITECTURE.md item 37b).
np=$(probe "$probe_ns" np-outside "curl -s -o /dev/null -w '%{http_code}' -m 5 http://app-$A.$ns.svc:8080/apps/$A/; true")
echo "info: unlabelled pod in another namespace -> app gate: ${np} (000 = no connection, NetworkPolicy enforced; 401 = not enforced, the gate refused)"

step "cap: a second Start at maxRunning=1 is refused"
api "$owner" POST "/api/apps/$B/start"
[ "$code" = 409 ] || fail "start B at the cap: $code $body"
echo "$body" | grep -q "too many apps" || fail "409 without the reason: $body"
sleep 3
[ "$(replicas "$B")" = 0 ] || fail "B got a replica despite the refusal"
echo "ok: 409 and no pod for B"

step "idle: no viewer for ${idle}s suspends A; a viewer's visit wakes it"
# The browser closed its websocket when iframe-path.sh finished; from here nothing touches A.
wait_state "$A" suspended $((idle + 60))
for _ in $(seq 1 30); do [ "$(replicas "$A")" = 0 ] && break; sleep 1; done
[ "$(replicas "$A")" = 0 ] || fail "suspended but still has a replica"
api "$owner" GET "/api/apps/$A"
[ "$(echo "$body" | json 'd.desiredState')" = running ] || fail "idle shutdown changed the owner's choice"
code=$(curl -s -o /dev/null -w '%{http_code}' -H "Cookie: booth_iframe_session=$viewer" -H 'Sec-Fetch-Dest: iframe' "$core/iframe/streamlit/apps/$A/")
[ "$code" = 503 ] || fail "first visit to a sleeping app: $code, want 503 (starting page)"
wait_state "$A" running 180
code=$(curl -s -o /dev/null -w '%{http_code}' -H "Cookie: booth_iframe_session=$viewer" "$core/iframe/streamlit/apps/$A/_stcore/health")
[ "$code" = 200 ] || fail "after waking: $code"
echo "ok: suspended after idle, woken by a viewer"

step "reconcile: restarting the backend leaves a running app alone"
uid=$(kubectl -n "$ns" get pod -l "booth.projectbooth.io/app-id=$A" -o jsonpath='{.items[0].metadata.uid}')
kubectl -n "$ns" rollout restart deploy/booth-streamlit >/dev/null
kubectl -n "$ns" rollout status deploy/booth-streamlit --timeout=180s >/dev/null
sleep 10
test "$(kubectl -n "$ns" get pod -l "booth.projectbooth.io/app-id=$A" -o jsonpath='{.items[0].metadata.uid}')" = "$uid" || fail "the app's pod was replaced by a backend restart"
wait_state "$A" running 60
echo "ok: same pod after a backend restart"

step "stop: an owner's Stop scales to 0, and a viewer can't wake it"
api "$owner" POST "/api/apps/$A/stop"; [ "$code" = 200 ] || fail "stop: $code $body"
for _ in $(seq 1 30); do [ "$(replicas "$A")" = 0 ] && break; sleep 1; done
[ "$(replicas "$A")" = 0 ] || fail "stopped app still has a replica"
code=$(curl -s -o /tmp/lc-stopped -w '%{http_code}' -H "Cookie: booth_iframe_session=$viewer" -H 'Sec-Fetch-Dest: iframe' "$core/iframe/streamlit/apps/$A/")
[ "$code" = 503 ] && grep -q "stopped" /tmp/lc-stopped || fail "viewer opening a stopped app: $code"
sleep 5
[ "$(replicas "$A")" = 0 ] || fail "a viewer's visit woke an owner-stopped app"
echo "ok"

step "reconcile: deleting an app removes all of its objects"
api "$owner" DELETE "/api/apps/$B"; [ "$code" = 204 ] || fail "delete B: $code"
api "$owner" DELETE "/api/apps/$A"; [ "$code" = 204 ] || fail "delete A: $code"
for id in "$A" "$B"; do
  for o in "deploy/app-$id" "svc/app-$id" "configmap/app-$id-src" "secret/app-$id-gate"; do
    gone=""
    for _ in $(seq 1 60); do
      kubectl -n "$ns" get "$o" >/dev/null 2>&1 || { gone=1; break; }
      sleep 1
    done
    [ -n "$gone" ] || fail "$o still exists after its app was deleted"
  done
done
echo "ok: deployment, service, configmap and secret gone for both"
echo "lifecycle: all checks passed"
