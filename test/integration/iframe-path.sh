#!/usr/bin/env bash
# Build step 1's end-to-end check, after deploy-realcore.sh: a real Chromium reaches the demo app
# through booth-core's iframe proxy, with real Keycloak tokens and core's real X-Booth-Identity.
# See browser/iframe-path.mjs for what is checked.
#
# Tokens and iframe URLs are minted inside the cluster (a token's `iss` must be Keycloak's
# in-cluster URL, which is what core trusts), then the browser reaches core through a
# port-forward. Core's navigation token lives one minute, so the browser runs right after.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
port=${CORE_PORT:-18080}
probe_ns=booth-streamlit-it-probe
fail() { echo "FAIL: $*" >&2; exit 1; }

kubectl create namespace "$probe_ns" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

# probe NAME SCRIPT: run SCRIPT in a one-off curl pod and print its output. Waits for the pod to
# finish and then reads its logs, instead of `kubectl run --rm -i`, whose attach race loses output.
probe() {
  local name=$1 script=$2 phase=""
  kubectl -n "$probe_ns" delete pod "$name" --ignore-not-found --wait >/dev/null 2>&1
  kubectl -n "$probe_ns" run "$name" --restart=Never --image=curlimages/curl --command -- sh -c "$script" >/dev/null
  for _ in $(seq 1 120); do
    phase=$(kubectl -n "$probe_ns" get pod "$name" -o jsonpath='{.status.phase}' 2>/dev/null)
    case "$phase" in Succeeded|Failed) break ;; esac
    sleep 1
  done
  kubectl -n "$probe_ns" logs "pod/$name" 2>&1 || echo "(no logs; pod phase: ${phase:-unknown})"
  kubectl -n "$probe_ns" delete pod "$name" --wait=false >/dev/null 2>&1 || true
}

password=$(kubectl -n keycloak get secret realcore-test-password -o jsonpath='{.data.password}' | base64 -d)

# One line per user: "<user> <sub> <iframe url>". Retries while core is still picking up the
# streamlit registration.
read -r -d '' script <<'SH' || true
KC=http://keycloak.keycloak.svc:8080/realms/booth/protocol/openid-connect
CORE=http://booth-core.booth-system.svc:8080
mint() {
  tok=$(curl -s -X POST "$KC/token" -d grant_type=password -d client_id=booth-design -d scope=openid \
    -d "username=$1" -d "password=$PW" | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p')
  [ -n "$tok" ] || { echo "$1 notoken"; return; }
  sub=$(curl -s -H "Authorization: Bearer $tok" "$KC/userinfo" | sed -n 's/.*"sub":"\([^"]*\)".*/\1/p')
  for i in $(seq 1 30); do
    url=$(curl -s -H "Authorization: Bearer $tok" -H "X-Workspace: $2" "$CORE/api/modules/streamlit/iframe-url" \
      | sed -n 's/.*"url":"\([^"]*\)".*/\1/p')
    [ -n "$url" ] && break
    sleep 3
  done
  echo "$1 ${sub:-nosub} ${url:-nourl}"
}
mint viewer-user acme-analytics
mint outsider-user other-team
SH

echo "--- port-forward booth-core to localhost:$port"
kubectl -n booth-system port-forward svc/booth-core "$port:8080" >/tmp/booth-core-port-forward.log 2>&1 &
pf=$!
trap 'kill $pf 2>/dev/null || true' EXIT
for _ in $(seq 1 30); do curl -s -o /dev/null "http://localhost:$port/healthz" && break; sleep 1; done

echo "--- real tokens and iframe URLs from Keycloak and booth-core"
out=$(probe iframe-urls "PW='$password'; $script")
redacted=$(echo "$out" | sed 's/\(booth_iframe_token=\)[^ ]*/\1<redacted>/')
echo "$redacted"
viewer=$(echo "$out" | grep '^viewer-user ')
outsider=$(echo "$out" | grep '^outsider-user ')
read -r _ viewer_sub viewer_url <<<"$viewer"
read -r _ _ outsider_url <<<"$outsider"
case "$viewer_sub$viewer_url$outsider_url" in *no*|"") fail "could not mint tokens/URLs: $redacted" ;; esac

echo "--- Chromium through core's iframe proxy"
(cd "$here/browser" && node iframe-path.mjs "http://localhost:$port" "$viewer_url" "$viewer_sub" "$outsider_url")
