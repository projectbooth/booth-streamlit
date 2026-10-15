#!/usr/bin/env bash
# App writes, phase 1 (ADR 0117 and its "Reconciled design" items 1a, 2 and 7;
# docs/design-app-writes.md), against a real booth-core and Keycloak users, with the booth-api
# stand-in deploy-realcore.sh registers as module "api" (fixtures/api_stub.py), until booth-api's
# app writes and its revoke-only path land. Every write reaches the stand-in through core's real
# gateway, which verifies the token first.
#
# "As user code" means `kubectl exec` into the app's streamlit container (see data-access.sh). An
# exec has no browser session, so the code states the viewer booth_streamlit.user() would return.
#
#   1. an owner declares a write target, which needs the confirmation; a viewer sees the declaration
#      and can't change it
#   2. the declaration closes the app's egress: the pod rolls to egress=closed, the internet is
#      dropped (it connected before) while the backend's 8081 still connects; BOOTH_WRITES_URL is set
#   3. without booth-api's grant, the write is refused not_granted, saying an owner's grant on the
#      API page is needed too
#   4. with the grant, the row is written: through core's gateway, with the app's own workload token
#      (subject streamlit:<ws>:<appId>, booth_module streamlit), X-Workspace, X-Booth-App-User,
#      an Idempotency-Key, and entered_by from the viewer
#   5. an undeclared operation and an undeclared API are refused by the module; booth-api never hears
#   6. take ownership while booth-api can't revoke (the stand-in answers 503): 503, the owner is
#      unchanged, the declaration is already cleared, and the revoke carried streamlit's own
#      revoke-only token (subject streamlit:<ws>:grants) for the app's principal
#   7. once booth-api answers, take ownership succeeds, booth-api's grant is gone, and a write is
#      refused (nothing declared)
set -euo pipefail
source "$(cd "$(dirname "$0")" && pwd)/lib.sh"
ws=acme-analytics
stub_port=${STUB_PORT:-18097}
kubectl -n booth-api-stub port-forward svc/api-stub "$stub_port:8080" >"/tmp/pf-stub-$stub_port.log" 2>&1 &
cleanup_pids+=($!)
for _ in $(seq 1 30); do curl -s -o /dev/null "http://localhost:$stub_port/healthz" && break; sleep 1; done
stub() { # METHOD PATH [JSON]: the stand-in's own test endpoints, never through core
  if [ -n "${3:-}" ]; then curl -s -X "$1" -H 'Content-Type: application/json' -d "$3" "http://localhost:$stub_port$2"; else curl -s "http://localhost:$stub_port$2"; fi
}
stub POST /stub/fail-revoke '{"on":false}' >/dev/null

step "sign in owner-user, owner2-user (owners of $ws) and viewer-user"
s=$(sessions owner-user owner2-user viewer-user)
owner=$(field "$s" owner-user 3); owner_sub=$(field "$s" owner-user 2)
owner2=$(field "$s" owner2-user 3); owner2_sub=$(field "$s" owner2-user 2)
viewer=$(field "$s" viewer-user 3)
[ ${#owner} -gt 20 ] && [ ${#owner2} -gt 20 ] && [ ${#viewer} -gt 20 ] || fail "could not open sessions"

api "$owner" GET /api/me
[ "$(echo "$body" | json 'd.writes')" = true ] || fail "app writes are off in this install: $body"

src=$(node -e 'process.stdout.write(JSON.stringify("import streamlit as st\nst.write(\"writes app\")\n"))')
api "$owner" POST /api/apps "{\"name\":\"Writes A\",\"description\":\"\",\"source\":$src,\"shared\":true}"; [ "$code" = 201 ] || fail "create: $code $body"
A=$(echo "$body" | json 'd.id')
principal="streamlit:$ws:$A"
api "$owner" POST "/api/apps/$A/start"; [ "$code" = 200 ] || fail "start: $code $body"
# Keep A active through core's iframe proxy, as owner2 (owner-user loses access in section 6).
( while true; do curl -s -o /dev/null -H "Cookie: booth_iframe_session=$owner2" "$core/iframe/streamlit/apps/$A/_stcore/health"; sleep 10; done ) &
cleanup_pids+=($!)
wait_ready() { # waits for a ready pod whose egress label is $1
  for _ in $(seq 1 120); do
    p=$(app_pod "$A")
    if [ -n "$p" ] && [ "$(kubectl -n "$ns" get pod "$p" -o jsonpath='{.metadata.labels.booth\.projectbooth\.io/egress}')" = "$1" ] &&
      [ "$(kubectl -n "$ns" get deploy "app-$A" -o jsonpath='{.status.readyReplicas}')" = 1 ]; then return 0; fi
    sleep 2
  done
  fail "app $A never had a ready pod labelled egress=$1"
}
wait_ready open
echo "app: $A ($(app_pod "$A")), principal $principal"

backend_pod=$(kubectl -n "$ns" get endpoints booth-streamlit -o jsonpath='{.subsets[0].addresses[0].ip}')
targets=$(node -e 'process.stdout.write(JSON.stringify({targets: {"the internet (1.1.1.1:443)": ["1.1.1.1", 443], "backend internal :8081": [process.argv[1], 8081]}, dns: "example.com", timeout: 5}))' "$backend_pod")
netprobe() { local pod; pod=$(wait_pod "$A") || return 1; kubectl -n "$ns" exec -i "$pod" -c streamlit -- python - "$targets" <"$here/fixtures/netprobe.py"; }
r=$(netprobe); echo "before declaring: $r"
[ "$(echo "$r" | json 'd["the internet (1.1.1.1:443)"]')" = open ] || fail "control: the app couldn't reach the internet before it declared a write target: $r"

step "1. the owner declares a write target, with the confirmation; a viewer sees it and can't change it"
decl='{"targets":[{"api":"household","operations":["insert"]}]'
api "$owner" PUT "/api/apps/$A/write-targets" "$decl}"
[ "$code" = 400 ] || fail "an unconfirmed declaration was accepted: $code $body"
api "$viewer" PUT "/api/apps/$A/write-targets" "$decl,\"confirmed\":true}"
[ "$code" = 403 ] || fail "a viewer declared a write target: $code $body"
api "$owner" PUT "/api/apps/$A/write-targets" "$decl,\"confirmed\":true}"
[ "$code" = 200 ] || fail "declare: $code $body"
api "$viewer" GET "/api/apps"
[ "$(echo "$body" | A="$A" json 'JSON.stringify(d.apps.find(a=>a.id===process.env.A).writeTargets)')" = '[{"api":"household","operations":["insert"]}]' ] \
  || fail "the viewer doesn't see the declaration: $body"

step "2. the declaration closes the app's egress (the pod rolls); the backend's 8081 still connects"
wait_ready closed
pod=$(app_pod "$A")
[ "$(kubectl -n "$ns" get pod "$pod" -o jsonpath='{.spec.containers[?(@.name=="streamlit")].env[?(@.name=="BOOTH_WRITES_URL")].value}')" = "http://127.0.0.1:8090/writes" ] \
  || fail "the writing app has no BOOTH_WRITES_URL"
for _ in $(seq 1 6); do # Calico converges on a new pod within seconds; accept only a drop at the end
  r=$(netprobe)
  [ "$(echo "$r" | json 'd["the internet (1.1.1.1:443)"]')" = "closed:TimeoutError" ] && break
  sleep 2
done
echo "after declaring: $r"
[ "$(echo "$r" | json 'd["the internet (1.1.1.1:443)"]')" = "closed:TimeoutError" ] || fail "a writing app still reaches the internet: $r"
[ "$(echo "$r" | json 'd["backend internal :8081"]')" = open ] || fail "a writing app can't reach the backend's 8081: $r"

# WRITE OP API: one write through booth_streamlit.api, printing "ok <row>" or "refused <status> <code> <message>".
WRITE='import json, sys
import booth_streamlit as bs
from booth_streamlit import api
bs.user = lambda: bs.User(subject="it-viewer-subject", workspace="acme-analytics", role="viewer")
op, target = sys.argv[1], sys.argv[2]
try:
    w = api.insert(target, {"amount": "12.50"}) if op == "insert" else api.delete(target, 1)
    print("ok", json.dumps(w.row, sort_keys=True))
except api.WriteRefused as e:
    print("refused", e.status, e.code, e.message)
except Exception as e:
    print("error", type(e).__name__, e)'
before=$(stub GET /stub/log | json 'd.length')

step "3. without booth-api's grant: refused not_granted, saying the API page's grant is needed too"
out=$(as_user_code "$A" "$WRITE" insert household); echo "$out"
case "$out" in "refused 403 not_granted "*"also needs an owner's grant on the API page"*"$principal"*) ;; *) fail "ungranted write: $out" ;; esac

step "4. with the grant: written through core's gateway, as the app, with entered_by"
stub POST /stub/grant "{\"principal\":\"$principal\",\"api\":\"household\",\"operations\":[\"insert\"]}" >/dev/null
out=$(as_user_code "$A" "$WRITE" insert household); echo "$out"
[ "$out" = 'ok {"amount": "12.50", "entered_by": "it-viewer-subject", "id": "1"}' ] || fail "granted write: $out"
last=$(stub GET /stub/log | json 'JSON.stringify(d[d.length-1])'); echo "booth-api saw: $last"
check() { [ "$(echo "$last" | json "$1")" = "$2" ] || fail "booth-api saw $1 = $(echo "$last" | json "$1"), want $2"; }
check 'd.method+" "+d.path' "POST /app/v1/household/rows"
check 'd.sub' "$principal"
check 'd.booth_module' streamlit
check 'd.workspace' "$ws"
check 'd.appUser' it-viewer-subject
check 'd.body.data.entered_by' it-viewer-subject
check 'String((d.idempotencyKey||"").length)' 36

step "5. an undeclared operation and an undeclared API: refused by the module, booth-api never called"
n=$(stub GET /stub/log | json 'd.length')
out=$(as_user_code "$A" "$WRITE" delete household); echo "$out"
case "$out" in "refused 403 not_a_write_target "*) ;; *) fail "undeclared delete: $out" ;; esac
out=$(as_user_code "$A" "$WRITE" insert payroll); echo "$out"
case "$out" in "refused 403 not_a_write_target "*) ;; *) fail "undeclared API: $out" ;; esac
[ "$(stub GET /stub/log | json 'd.length')" = "$n" ] || fail "booth-api was called for an undeclared write"

step "6. take ownership while booth-api can't revoke: refused, owner unchanged, declaration cleared"
kc_groups owner-user remove "/workspaces/$ws/owner"
sessions owner-user >/dev/null
reason=""
for _ in $(seq 1 60); do
  api "$owner2" GET "/api/apps/$A"
  reason=$(echo "$body" | json 'd.dataPausedReason||""')
  [ -n "$reason" ] && break
  sleep 2
done
[ -n "$reason" ] || fail "the app never showed data access as paused"
echo "paused: $reason"
stub POST /stub/fail-revoke '{"on":true}' >/dev/null
api "$owner2" POST "/api/apps/$A/take-ownership"
echo "take ownership: $code $body"
[ "$code" = 503 ] && echo "$body" | grep -q "ownership did not change" || fail "take ownership didn't fail closed: $code $body"
api "$owner2" GET "/api/apps/$A"
[ "$(echo "$body" | json 'd.owner')" = "$owner_sub" ] || fail "the owner changed although the revoke failed: $body"
[ "$(echo "$body" | json 'd.writeTargets.length')" = 0 ] || fail "the declaration wasn't cleared first: $body"
last=$(stub GET /stub/log | json 'JSON.stringify(d[d.length-1])'); echo "booth-api saw: $last"
check 'd.method+" "+d.path' "POST /api/write-grants/revoke-principal"
check 'd.sub' "streamlit:$ws:grants"
check 'd.booth_module' streamlit
check 'd.workspace' "$ws"
check 'd.body.principal' "$principal"
grep -q "take ownership refused: app=$A workspace=$ws by=$owner2_sub" <<<"$(kubectl -n "$ns" logs deploy/booth-streamlit --since=2m)" \
  || fail "the refused take-over was not logged"

step "7. once booth-api answers: ownership changes, the grant is revoked, writes are refused"
stub POST /stub/fail-revoke '{"on":false}' >/dev/null
api "$owner2" POST "/api/apps/$A/take-ownership"
[ "$code" = 200 ] && [ "$(echo "$body" | json 'd.owner')" = "$owner2_sub" ] || fail "take ownership: $code $body"
[ "$(stub GET /stub/grants | P="$principal" json 'JSON.stringify(d[process.env.P]||null)')" = null ] || fail "booth-api still holds the app's grant: $(stub GET /stub/grants)"
grep -q "write grants revoked: workspace=$ws principal=$principal revoked=1 by=$owner2_sub" <<<"$(kubectl -n "$ns" logs deploy/booth-streamlit --since=2m)" \
  || fail "the revoke was not logged with the count"
wait_ready open
# Straight to the gate's loopback /writes: the new pod has no BOOTH_WRITES_URL (nothing declared).
RAW='import urllib.request as u, urllib.error as e
try:
    u.urlopen(u.Request("http://127.0.0.1:8090/writes/household/rows", data=b"{\"data\":{}}", method="POST", headers={"Content-Type": "application/json"}), timeout=30)
    print("written")
except e.HTTPError as x:
    print(x.code, x.read().decode())'
out=$(as_user_code "$A" "$RAW"); echo "$out"
case "$out" in "403 "*not_a_write_target*) ;; *) fail "a write after the take-over: $out" ;; esac

kc_groups owner-user add "/workspaces/$ws/owner"
sessions owner-user >/dev/null
for _ in $(seq 1 30); do api "$owner2" DELETE "/api/apps/$A"; [ "$code" = 204 ] && break; sleep 2; done
echo "writes: all checks passed (booth-api calls before this run: $before)"
