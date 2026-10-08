#!/usr/bin/env bash
# ADR 0105 against a real booth-core, after deploy-realcore.sh: only workspace owners may create,
# edit, start, stop or delete an app. Real Keycloak users, real tokens, core's real iframe session
# and X-Booth-Identity, so the role the module enforces is the one core's assertion carries.
#
#   owner-user    owner  of acme-analytics  creates a shared app; every write succeeds
#   editor-user   editor of acme-analytics  refused on every write (403), may read the shared app
#   viewer-user   viewer of acme-analytics  refused on every write (403), may read the shared app
#   outsider-user owner  of other-team      refused on every write to it (404: not in their workspace)
#
# Everything runs in one curl pod (no jq there, hence sed), printing "<user> <op> <status>" lines.
set -euo pipefail

probe_ns=booth-streamlit-it-probe
fail() { echo "FAIL: $*" >&2; exit 1; }
kubectl create namespace "$probe_ns" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

probe() {
  local name=$1 script=$2 phase=""
  kubectl -n "$probe_ns" delete pod "$name" --ignore-not-found --wait >/dev/null 2>&1
  kubectl -n "$probe_ns" run "$name" --restart=Never --image=curlimages/curl --command -- sh -c "$script" >/dev/null
  for _ in $(seq 1 180); do
    phase=$(kubectl -n "$probe_ns" get pod "$name" -o jsonpath='{.status.phase}' 2>/dev/null)
    case "$phase" in Succeeded|Failed) break ;; esac
    sleep 1
  done
  kubectl -n "$probe_ns" logs "pod/$name" 2>&1 || echo "(no logs; pod phase: ${phase:-unknown})"
  kubectl -n "$probe_ns" delete pod "$name" --wait=false >/dev/null 2>&1 || true
}

password=$(kubectl -n keycloak get secret realcore-test-password -o jsonpath='{.data.password}' | base64 -d)

read -r -d '' script <<'SH' || true
KC=http://keycloak.keycloak.svc:8080/realms/booth/protocol/openid-connect
CORE=http://booth-core.booth-system.svc:8080
# session USER WORKSPACE: prints core's iframe session cookie value for USER acting in WORKSPACE.
session() {
  tok=$(curl -s -X POST "$KC/token" -d grant_type=password -d client_id=booth-design -d scope=openid \
    -d "username=$1" -d "password=$PW" | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p')
  for i in $(seq 1 30); do
    url=$(curl -s -H "Authorization: Bearer $tok" -H "X-Workspace: $2" "$CORE/api/modules/streamlit/iframe-url" \
      | sed -n 's/.*"url":"\([^"]*\)".*/\1/p')
    [ -n "$url" ] && break
    sleep 3
  done
  curl -s -o /dev/null -D - "$CORE$url" | tr -d '\r' | sed -n 's/^[Ss]et-[Cc]ookie: booth_iframe_session=\([^;]*\).*/\1/p'
}
# call USER COOKIE METHOD PATH [JSON]: prints "<user> <method> <path> <status>"
call() {
  if [ -n "${5:-}" ]; then
    code=$(curl -s -o /tmp/body -w '%{http_code}' -X "$3" -H "Cookie: booth_iframe_session=$2" -H 'Content-Type: application/json' -d "$5" "$CORE/iframe/streamlit$4")
  else
    code=$(curl -s -o /tmp/body -w '%{http_code}' -X "$3" -H "Cookie: booth_iframe_session=$2" "$CORE/iframe/streamlit$4")
  fi
  echo "$1 $3 $4 $code"
}
BODY='{"name":"Authz probe","description":"","source":"import streamlit as st","shared":true}'
EDIT='{"name":"Hijacked","description":"","source":"print(1)","shared":true}'

owner=$(session owner-user acme-analytics)
editor=$(session editor-user acme-analytics)
viewer=$(session viewer-user acme-analytics)
outsider=$(session outsider-user other-team)
[ ${#owner} -gt 20 ] || echo "nocookie owner"

call owner "$owner" GET /api/me
code=$(curl -s -o /tmp/created -w '%{http_code}' -X POST -H "Cookie: booth_iframe_session=$owner" -H 'Content-Type: application/json' -d "$BODY" "$CORE/iframe/streamlit/api/apps")
echo "owner POST /api/apps $code"
ID=$(sed -n 's/.*"id":"\([^"]*\)".*/\1/p' /tmp/created)
echo "appid ${ID:-none}"

for who in editor viewer outsider; do
  # A plain case, not eval: busybox sh in the curl image expanded eval "c=\$$who" to the name.
  case $who in editor) c=$editor ;; viewer) c=$viewer ;; outsider) c=$outsider ;; esac
  [ ${#c} -gt 20 ] || echo "nocookie $who"
  call "$who" "$c" POST /api/apps "$BODY"
  call "$who" "$c" PUT "/api/apps/$ID" "$EDIT"
  call "$who" "$c" POST "/api/apps/$ID/start"
  call "$who" "$c" POST "/api/apps/$ID/stop"
  call "$who" "$c" DELETE "/api/apps/$ID"
  call "$who" "$c" GET "/api/apps/$ID"
done

# The app survived every refused write unchanged.
curl -s -H "Cookie: booth_iframe_session=$owner" "$CORE/iframe/streamlit/api/apps/$ID" >/tmp/after
grep -q '"name":"Authz probe"' /tmp/after && grep -q '"desiredState":"stopped"' /tmp/after && echo "unchanged yes" || echo "unchanged no"

call owner "$owner" PUT "/api/apps/$ID" '{"name":"Authz probe 2","description":"","source":"import streamlit as st","shared":true}'
call owner "$owner" POST "/api/apps/$ID/start"
call owner "$owner" POST "/api/apps/$ID/stop"
call owner "$owner" DELETE "/api/apps/$ID"
SH

echo "--- ADR 0105 through core's iframe proxy, with real roles"
out=$(probe api-authz "PW='$password'; $script")
echo "$out"

expect() { echo "$out" | grep -qx "$1" || fail "expected line: $1"; }
echo "$out" | grep -q '^nocookie' && fail "could not open an iframe session for every user"
id=$(echo "$out" | sed -n 's/^appid //p')
[ -n "$id" ] && [ "$id" != none ] || fail "owner could not create an app"

expect "owner GET /api/me 200"
expect "owner POST /api/apps 201"
for who in editor viewer; do
  expect "$who POST /api/apps 403"
  for op in "PUT /api/apps/$id" "POST /api/apps/$id/start" "POST /api/apps/$id/stop" "DELETE /api/apps/$id"; do
    expect "$who $op 403"
  done
  expect "$who GET /api/apps/$id 200" # shared with the workspace: readable, not writable
done
for op in "PUT /api/apps/$id" "POST /api/apps/$id/start" "POST /api/apps/$id/stop" "DELETE /api/apps/$id" "GET /api/apps/$id"; do
  expect "outsider $op 404"
done
# Creating in their own workspace is the outsider's right; it never touches acme-analytics.
expect "outsider POST /api/apps 201"
expect "unchanged yes"
expect "owner PUT /api/apps/$id 200"
expect "owner POST /api/apps/$id/start 200"
expect "owner POST /api/apps/$id/stop 200"
expect "owner DELETE /api/apps/$id 204"
echo "api authz: all checks passed"
