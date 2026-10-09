#!/usr/bin/env bash
# Data access, step d: per-app packages (ADR 0107; docs/design-data-access.md item 5), against a real
# booth-core and Keycloak, after deploy-realcore.sh. Packages come from the in-cluster test index
# (realcore/pypi.yaml, fixtures/pypi_index.py), not public PyPI, so CI doesn't depend on it; the
# install sets apps.pip.indexUrl to it, an egress rule for it, and apps.pip.timeout=60s.
# Runs last: section 6 switches apps.egress.mode to closed (and back).
#
# The install runs one app at a time (apps.maxRunning=1), so each app is stopped before the next.
#
#   1. how long an app takes to become ready, without and with one requirement
#   2. an app with a requirements.txt installs a pure-Python package from the index and imports it
#   3. the pip init container mounts no token, bearer or credentials (pod spec, and its own view)
#   4. the scanner, from the Streamlit container of that app, still finds nothing
#   5. a bad requirement fails the app and shows pip's output to the owner
#   6. a hung install (a package page that never finishes) fails at apps.pip.timeout, not "starting"
#   7. with apps.egress.mode=closed and no index rule, the failure says so
set -euo pipefail
source "$(cd "$(dirname "$0")" && pwd)/lib.sh"
repo=$(cd "$here/../.." && pwd)

step "sign in owner2-user (an owner of acme-analytics)"
s=$(sessions owner2-user)
owner2=$(field "$s" owner2-user 3)
[ ${#owner2} -gt 20 ] || fail "could not open a session"

# Keep the current app active through core's iframe proxy (idle timeout is 60s).
echo "" >/tmp/pip-current
( while true; do id=$(cat /tmp/pip-current); [ -n "$id" ] && curl -s -o /dev/null -H "Cookie: booth_iframe_session=$owner2" "$core/iframe/streamlit/apps/$id/_stcore/health"; sleep 5; done ) &
cleanup_pids+=($!)

src=$(node -e 'process.stdout.write(JSON.stringify("import streamlit as st\nst.write(\"pip app\")\n"))')
create() { # NAME REQUIREMENTS -> app id
  local req
  req=$(node -e 'process.stdout.write(JSON.stringify(process.argv[1]))' "$2")
  api "$owner2" POST /api/apps "{\"name\":\"$1\",\"description\":\"\",\"source\":$src,\"requirements\":$req,\"shared\":true}"
  [ "$code" = 201 ] || fail "create $1: $code $body"
  echo "$body" | json 'd.id'
}
set_requirements() { # ID REQUIREMENTS
  local req
  req=$(node -e 'process.stdout.write(JSON.stringify(process.argv[1]))' "$2")
  api "$owner2" PUT "/api/apps/$1" "{\"name\":\"Pip app\",\"description\":\"\",\"source\":$src,\"requirements\":$req,\"shared\":true}"
  [ "$code" = 200 ] || fail "update $1: $code $body"
}
# wait_for ID STATE SECONDS: polls the app's status every second until STATE (or failed); prints
# "<state> <seconds> <states seen>". It runs in $(...), a subshell, so it can't hand back $body:
# reason_of reads the app again in the caller's shell (run 37952130970 read a stale $body).
wait_for() {
  local start now st seen=""
  start=$(date +%s)
  while true; do
    api "$owner2" GET "/api/apps/$1"
    st=$(echo "$body" | json 'd.status.state')
    case " $seen " in *" $st "*) ;; *) seen="$seen $st" ;; esac
    now=$(date +%s)
    if [ "$st" = "$2" ] || [ "$st" = failed ] || [ $((now - start)) -ge "$3" ]; then
      echo "$st $((now - start))$seen"
      return 0
    fi
    sleep 1
  done
}
reason_of() { api "$owner2" GET "/api/apps/$1"; echo "$body" | json 'd.status.reason||""'; }
start_app() { echo "$1" >/tmp/pip-current; api "$owner2" POST "/api/apps/$1/start"; [ "$code" = 200 ] || fail "start $1: $code $body"; }
stop_app() { api "$owner2" POST "/api/apps/$1/stop"; echo "" >/tmp/pip-current; }

step "1. time to ready: without requirements, then with one"
P0=$(create "No packages" "")
start_app "$P0"
r=$(wait_for "$P0" running 180); echo "no requirements: $r"
set -- $r; [ "$1" = running ] || fail "the app without requirements did not start: $r"
t_plain=$2
stop_app "$P0"

P1=$(create "Pip app" "booth-it-hello==1.0.0")
start_app "$P1"
r=$(wait_for "$P1" running 240); echo "one requirement: $r"
set -- $r; [ "$1" = running ] || fail "the app with a requirement did not start: $r  $(reason_of "$P1")"
t_pip=$2
echo "$r" | grep -q " installing" || echo "note: 'installing' was not observed (the install took less than the 1s poll)"
pod=$(wait_pod "$P1")
pip_log=$(kubectl -n "$ns" logs "$pod" -c pip)
echo "$pip_log" | grep "^booth pip install:"
t_install=$(echo "$pip_log" | sed -n 's/^booth pip install: done in \([0-9.]*\)s$/\1/p')

step "2. the app imports the package installed from the index"
out=$(as_user_code "$P1" '
import booth_it_hello, sys
print("greeting=" + booth_it_hello.GREETING)
print("from=" + booth_it_hello.__file__)
print("path=" + ":".join(p for p in sys.path if p.startswith("/opt/booth")))')
echo "$out"
echo "$out" | grep -qx "greeting=hello from the booth test index" || fail "the package did not import"
echo "$out" | grep -q "^from=/opt/booth/site/booth_it_hello/" || fail "the package is not from the site volume"
out=$(as_user_code "$P1" '
import os
try:
    open("/opt/booth/site/written-by-app", "w"); print("site=writable")
except OSError as e:
    print("site=read-only " + type(e).__name__)')
echo "$out"
echo "$out" | grep -q "^site=read-only" || fail "the Streamlit container can write the site volume"

step "3. the pip init container mounts no token, bearer or credentials"
mounts=$(kubectl -n "$ns" get pod "$pod" -o jsonpath='{range .spec.initContainers[?(@.name=="pip")].volumeMounts[*]}{.name}={.mountPath}:{.readOnly} {end}')
echo "pod spec: $mounts"
[ "$mounts" = "source=/app:true site=/opt/booth/site: " ] || fail "the pip init container mounts more than the source and the site volume: $mounts"
envfrom=$(kubectl -n "$ns" get pod "$pod" -o jsonpath='{.spec.initContainers[?(@.name=="pip")].envFrom}{.spec.initContainers[?(@.name=="pip")].env[*].valueFrom}')
[ -z "$envfrom" ] || fail "the pip init container takes env from a Secret or field: $envfrom"
[ "$(kubectl -n "$ns" get pod "$pod" -o jsonpath='{.spec.automountServiceAccountToken}')" = false ] || fail "the pod mounts a service-account token"
seen_mounts=$(echo "$pip_log" | sed -n 's/^booth pip install: mounts: //p')
echo "its own view (/proc/self/mounts): $seen_mounts"
for bad in /var/run/booth /run/booth /etc/booth serviceaccount; do
  case " $seen_mounts " in *"$bad"*) fail "the pip init container could see $bad" ;; esac
done
echo "$seen_mounts" | grep -q "/opt/booth/site" || fail "the mount listing is missing the site volume (is it reading /proc/self/mounts?)"

step "4. the scanner, from the Streamlit container of the app with packages: no token, no bearer"
bearer=$(kubectl -n "$ns" get secret "app-$P1-gate" -o jsonpath='{.data.bearer}' | base64 -d)
bearer_sha=$(printf '%s' "$bearer" | sha256sum | cut -d' ' -f1)
scan=$(kubectl -n "$ns" exec -i "$(wait_pod "$P1")" -c streamlit -- python - "$issuer" "$bearer_sha" <"$here/fixtures/scanner.py")
echo "$scan"
[ "$(echo "$scan" | json 'd.found.decoy')" = true ] || fail "the scanner did not find its own decoy: the scan is broken, not clean"
[ "$(echo "$scan" | json 'd.found.workload_jwt.length')" = 0 ] || fail "user code can read a workload token"
[ "$(echo "$scan" | json 'd.found.bearer.length')" = 0 ] || fail "user code can read the gate bearer"
[ "$(echo "$scan" | json 'd.found.sa_token')" = false ] || fail "user code has a service-account token"

step "5. a bad requirement fails the app and shows pip's output to the owner"
set_requirements "$P1" "booth-it-does-not-exist==9.9"
r=$(wait_for "$P1" failed 180); echo "bad requirement: $r"
reason=$(reason_of "$P1")
echo "reason: $reason"
set -- $r; [ "$1" = failed ] || fail "a bad requirement did not fail the app: $r"
t_bad=$2
echo "$reason" | grep -q "^pip install failed (exit 1):" || fail "the reason does not say pip failed"
echo "$reason" | grep -q "No matching distribution found for booth-it-does-not-exist==9.9" || fail "the reason lacks pip's output"

step "6. a hung install fails at apps.pip.timeout (60s here), not 'starting' forever"
set_requirements "$P1" "booth-it-slow"
r=$(wait_for "$P1" failed 240); echo "hung install: $r"
reason=$(reason_of "$P1")
echo "reason: $reason"
set -- $r; [ "$1" = failed ] || fail "a hung install did not fail the app within 240s: $r"
t_hang=$2
echo "$reason" | grep -q "^pip install did not finish within 60s" || fail "the reason does not say the install was stopped at the deadline"

step "7. apps.egress.mode=closed with no index rule: the failure says so"
set_requirements "$P1" "booth-it-hello==1.0.0"
stop_app "$P1"
helm upgrade booth-streamlit "$repo/charts/booth-streamlit" -n "$ns" --reuse-values \
  --set apps.egress.mode=closed --set apps.pip.egress.podSelector=null >/dev/null
kubectl -n "$ns" rollout status deploy/booth-streamlit --timeout=300s >/dev/null
closed=$(kubectl -n "$ns" get deploy booth-streamlit -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="BOOTH_APP_PIP_EGRESS_CLOSED")].value}')
rules=$(kubectl -n "$ns" get networkpolicy -o yaml | grep -c "booth-pypi" || true)
echo "backend BOOTH_APP_PIP_EGRESS_CLOSED=$closed; app egress rules naming booth-pypi: $rules"
[ "$closed" = true ] && [ "$rules" = 0 ] || fail "the closed-egress setup did not take"
for _ in $(seq 1 30); do api "$owner2" GET "/api/apps/$P1"; [ "$code" = 200 ] && break; sleep 2; done
start_app "$P1"
r=$(wait_for "$P1" failed 240); echo "closed egress: $r"
reason=$(reason_of "$P1")
echo "reason: $reason"
set -- $r; [ "$1" = failed ] || fail "with closed egress the install did not fail: $r"
t_closed=$2
echo "$reason" | grep -q "^pip install needs internet access, and apps.egress.mode is closed" || fail "the closed-egress wording is missing"
stop_app "$P1"
helm upgrade booth-streamlit "$repo/charts/booth-streamlit" -n "$ns" --reuse-values \
  --set apps.egress.mode=open --set apps.pip.egress.podSelector.app=pypi >/dev/null
kubectl -n "$ns" rollout status deploy/booth-streamlit --timeout=300s >/dev/null

echo "timing: ready without requirements ${t_plain}s; with one requirement ${t_pip}s (pip itself ${t_install:-?}s);" \
  "a bad requirement shown as failed after ${t_bad}s; a hung install failed after ${t_hang}s (apps.pip.timeout=60s);" \
  "closed egress failed after ${t_closed}s (seconds, polled every 1s, from Start or the edit)"
for id in "$P0" "$P1"; do api "$owner2" DELETE "/api/apps/$id"; done
echo "pip access: all checks passed"
