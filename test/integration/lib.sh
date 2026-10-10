#!/usr/bin/env bash
# Shared helpers for the real-core integration scripts (data-access.sh, files-access.sh), sourced
# after `set -euo pipefail`. Sets up port-forwards to booth-core and Keycloak (CORE_PORT,
# KEYCLOAK_PORT), the test passwords, and functions to run probes, open real sessions, call the
# module's API through core, administer Keycloak groups, and run code as an app's user code.
here=$(cd "$(dirname "$0")" && pwd)
ns=booth-streamlit
probe_ns=booth-streamlit-it-probe
core_port=${CORE_PORT:-18083}
kc_port=${KEYCLOAK_PORT:-18091}
# booth-core's workload issuer in this test install. Not core's chart default (that is the
# ".svc.cluster.local" form): deploy-realcore.sh sets core's workloadIdentity.issuerUrl to this
# explicitly, and has storage, catalog and lakehouse trust the same string.
issuer=http://booth-core.booth-system.svc:8080
fail() { echo "FAIL: $*" >&2; exit 1; }
step() { echo "--- $*"; }
json() { node -e "const d=JSON.parse(require('fs').readFileSync(0,'utf8')); const v=($1); process.stdout.write(v===undefined?'':String(v))"; }

kubectl create namespace "$probe_ns" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

probe() { # NAMESPACE NAME SCRIPT [kubectl run args...]; image from $PROBE_IMAGE (default curl)
  local pns=$1 name=$2 script=$3 phase=""
  shift 3
  kubectl -n "$pns" delete pod "$name" --ignore-not-found --wait >/dev/null 2>&1
  kubectl -n "$pns" run "$name" --restart=Never --image="${PROBE_IMAGE:-curlimages/curl}" "$@" --command -- sh -c "$script" >/dev/null
  for _ in $(seq 1 180); do
    phase=$(kubectl -n "$pns" get pod "$name" -o jsonpath='{.status.phase}' 2>/dev/null)
    case "$phase" in Succeeded|Failed) break ;; esac
    sleep 1
  done
  kubectl -n "$pns" logs "pod/$name" 2>&1 || echo "(no logs; pod phase: ${phase:-unknown})"
  kubectl -n "$pns" delete pod "$name" --wait=false >/dev/null 2>&1 || true
}
limits='{"spec":{"containers":[{"name":"NAME","resources":{"limits":{"cpu":"200m","memory":"128Mi","ephemeral-storage":"64Mi"}}}]}}'

password=$(kubectl -n keycloak get secret realcore-test-password -o jsonpath='{.data.password}' | base64 -d)
kc_admin=$(kubectl -n keycloak get secret keycloak-admin -o jsonpath='{.data.password}' | base64 -d)

# Background processes to stop on exit (port-forwards, keepalives): append to cleanup_pids.
cleanup_pids=()
trap 'kill "${cleanup_pids[@]}" 2>/dev/null || true' EXIT
kubectl -n booth-system port-forward svc/booth-core "$core_port:8080" >"/tmp/pf-core-$core_port.log" 2>&1 &
cleanup_pids+=($!)
kubectl -n keycloak port-forward svc/keycloak "$kc_port:8080" >"/tmp/pf-kc-$kc_port.log" 2>&1 &
cleanup_pids+=($!)
for _ in $(seq 1 30); do curl -s -o /dev/null "http://localhost:$core_port/healthz" && curl -s -o /dev/null "http://localhost:$kc_port/realms/booth" && break; sleep 1; done
core="http://localhost:$core_port"

# sessions USER...: prints "<user> <sub> <cookie>" per user, the session opened in $SESSION_WS
# (default acme-analytics). Each user signs in (a real token, so core
# records their current roles) and opens core's iframe session for the module.
sessions() {
  local script='KC=http://keycloak.keycloak.svc:8080/realms/booth/protocol/openid-connect
CORE=http://booth-core.booth-system.svc:8080
for u in USERS; do
  tok=$(curl -s -X POST "$KC/token" -d grant_type=password -d client_id=booth-design -d scope=openid -d "username=$u" -d "password=$PW" | sed -n "s/.*\"access_token\":\"\([^\"]*\)\".*/\1/p")
  sub=$(curl -s -H "Authorization: Bearer $tok" "$KC/userinfo" | sed -n "s/.*\"sub\":\"\([^\"]*\)\".*/\1/p")
  curl -s -o /dev/null -H "Authorization: Bearer $tok" "$CORE/api/me"
  url=$(curl -s -H "Authorization: Bearer $tok" -H "X-Workspace: WS" "$CORE/api/modules/streamlit/iframe-url" | sed -n "s/.*\"url\":\"\([^\"]*\)\".*/\1/p")
  c=""
  [ -n "$url" ] && c=$(curl -s -o /dev/null -D - "$CORE$url" | tr -d "\r" | sed -n "s/^[Ss]et-[Cc]ookie: booth_iframe_session=\([^;]*\).*/\1/p")
  echo "$u ${sub:-nosub} ${c:-nocookie}"
done'
  script=${script/USERS/$*}
  probe "$probe_ns" "sessions-$RANDOM" "PW='$password'; ${script/WS/${SESSION_WS:-acme-analytics}}"
}
field() { echo "$1" | awk -v u="$2" -v i="$3" '$1==u {print $i}'; }

api() { # COOKIE METHOD PATH [JSON]; sets $code and $body
  local args=(-s -o /tmp/da-body -w '%{http_code}' -X "$2" -H "Cookie: booth_iframe_session=$1")
  [ -n "${4:-}" ] && args+=(-H 'Content-Type: application/json' -d "$4")
  code=$(curl "${args[@]}" "$core/iframe/streamlit$3")
  body=$(cat /tmp/da-body)
}

# Keycloak admin (group membership is what roles come from, ADR 0025).
kc_token() {
  curl -s -X POST "http://localhost:$kc_port/realms/master/protocol/openid-connect/token" \
    -d grant_type=password -d client_id=admin-cli -d username=admin -d "password=$kc_admin" | json 'd.access_token'
}
kc_groups() { # USER add|remove GROUP-PATH
  local t uid gid
  t=$(kc_token)
  uid=$(curl -s -H "Authorization: Bearer $t" "http://localhost:$kc_port/admin/realms/booth/users?username=$1&exact=true" | json 'd[0].id')
  gid=$(curl -s -H "Authorization: Bearer $t" "http://localhost:$kc_port/admin/realms/booth/group-by-path$3" | json 'd.id')
  [ -n "$uid" ] && [ -n "$gid" ] || fail "keycloak: no user $1 or group $3"
  if [ "$2" = add ]; then
    curl -s -o /dev/null -w '%{http_code}' -X PUT -H "Authorization: Bearer $t" "http://localhost:$kc_port/admin/realms/booth/users/$uid/groups/$gid" | grep -q 204 || fail "keycloak: adding $1 to $3"
  else
    curl -s -o /dev/null -w '%{http_code}' -X DELETE -H "Authorization: Bearer $t" "http://localhost:$kc_port/admin/realms/booth/users/$uid/groups/$gid" | grep -q 204 || fail "keycloak: removing $1 from $3"
  fi
}

# Empty while the app has no Running pod (e.g. mid-roll: the Recreate strategy stops the old pod
# before starting the new one). Never fails: under set -e a bare `p=$(app_pod)` would otherwise end
# the script silently, which it did in CI run 37838444181.
# A pod being deleted still reports phase Running until it is gone, so those are skipped too.
app_pod() {
  kubectl -n "$ns" get pod -l "booth.projectbooth.io/app-id=$1" --field-selector=status.phase=Running \
    -o go-template='{{range .items}}{{if not .metadata.deletionTimestamp}}{{.metadata.name}} {{end}}{{end}}' 2>/dev/null | awk '{print $1}' || true
}
# The app's Running pod, waiting up to 2 minutes for one, for anything that execs into it. Without
# the wait, an exec during a pod roll ran with no pod name ("error: pod, type/name or --filename
# must be specified", in step b's CI log), and only a retry loop around it hid that.
wait_pod() {
  local p=""
  for _ in $(seq 1 60); do
    p=$(app_pod "$1")
    [ -n "$p" ] && { echo "$p"; return 0; }
    sleep 2
  done
  echo "FAIL: app $1 has no Running pod after 2 minutes" >&2
  return 1
}
as_user_code() { # APP-ID PYTHON-CODE [ARGS...]: runs it in the app's streamlit container
  local pod
  pod=$(wait_pod "$1") || return 1
  kubectl -n "$ns" exec "$pod" -c streamlit -- python -c "$2" "${@:3}"
}
