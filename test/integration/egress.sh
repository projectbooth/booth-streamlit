#!/usr/bin/env bash
# App egress (ADR 0104 item 4; charts/booth-streamlit/templates/networkpolicy.yaml), probed from
# inside an app's own Streamlit container, as its user code would (fixtures/netprobe.py).
#
# Needs a CNI that enforces egress NetworkPolicy: the real-core job runs Calico (cni/), because
# kind's default kindnet enforces ingress only, which would make every denial below vacuous.
#
#   apps.egress.mode=open (the install's mode):
#     denied, and each must be a DROP (a timeout), never a refusal: booth-core's pod and its Service
#     ClusterIP, another namespace's pod (Keycloak), the node's kubelet, and this module's own
#     backend on its module port (only its internal port 8081 is allowed);
#     positive control for each denial: the same probe from an unrestricted pod connects, so the
#     target is up and the timeout is the app pod's egress policy;
#     allowed, and each must connect: the internet, DNS, the backend's 8081, and the rules this
#     install adds (booth-database's Postgres, MinIO, the package index).
#   apps.egress.mode=closed:
#     the internet is denied (a drop); its positive control is the same probe from the same pod in
#     open mode. DNS and the explicit rules still work.
set -euo pipefail
source "$(cd "$(dirname "$0")" && pwd)/lib.sh"
repo=$(cd "$here/../.." && pwd)

step "sign in owner2-user (an owner of acme-analytics)"
s=$(sessions owner2-user)
owner2=$(field "$s" owner2-user 3)
[ ${#owner2} -gt 20 ] || fail "could not open a session"

src=$(node -e 'process.stdout.write(JSON.stringify("import streamlit as st\nst.write(\"egress probe\")\n"))')
api "$owner2" POST /api/apps "{\"name\":\"Egress probe\",\"description\":\"\",\"source\":$src,\"shared\":false}"; [ "$code" = 201 ] || fail "create: $code $body"
A=$(echo "$body" | json 'd.id')
api "$owner2" POST "/api/apps/$A/start"; [ "$code" = 200 ] || fail "start: $code $body"
( while true; do curl -s -o /dev/null -H "Cookie: booth_iframe_session=$owner2" "$core/iframe/streamlit/apps/$A/_stcore/health"; sleep 10; done ) &
cleanup_pids+=($!)
ready=""
for _ in $(seq 1 120); do [ -n "$(app_pod "$A")" ] && [ "$(kubectl -n "$ns" get deploy "app-$A" -o jsonpath='{.status.readyReplicas}')" = 1 ] && { ready=1; break; }; sleep 2; done
[ -n "$ready" ] || fail "the probe app never became ready"
echo "app: $A ($(app_pod "$A"))"

# Targets, by address (pod IPs from Service endpoints, so no DNS is involved in the verdicts).
ep() { kubectl -n "$1" get endpoints "$2" -o jsonpath='{.subsets[0].addresses[0].ip}'; }
core_pod=$(ep booth-system booth-core)
core_svc=$(kubectl -n booth-system get svc booth-core -o jsonpath='{.spec.clusterIP}')
kc_pod=$(ep keycloak keycloak)
node_ip=$(kubectl get nodes -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')
backend_pod=$(ep "$ns" booth-streamlit)
pg_pod=$(kubectl -n booth-database get pod -l app.kubernetes.io/name=booth-database,app.kubernetes.io/component=postgres -o jsonpath='{.items[?(@.status.phase=="Running")].status.podIP}' | awk '{print $1}')
minio_pod=$(ep booth-minio minio)
pypi_pod=$(ep booth-pypi pypi)
for v in core_pod core_svc kc_pod node_ip backend_pod pg_pod minio_pod pypi_pod; do [ -n "${!v}" ] || fail "no address for $v"; done
echo "targets: core pod $core_pod, core Service $core_svc, keycloak pod $kc_pod, node $node_ip, backend pod $backend_pod, postgres $pg_pod, minio $minio_pod, index $pypi_pod"

denied=$(node -e '
const [core, svc, kc, node, backend] = process.argv.slice(1);
process.stdout.write(JSON.stringify({
  "booth-core pod :8080": [core, 8080], "booth-core Service ClusterIP :8080": [svc, 8080],
  "another namespace pod (keycloak) :8080": [kc, 8080], "the node kubelet :10250": [node, 10250],
  "this module backend :8080 (only 8081 is allowed)": [backend, 8080]}))' "$core_pod" "$core_svc" "$kc_pod" "$node_ip" "$backend_pod")
allowed=$(node -e '
const [backend, pg, minio, pypi] = process.argv.slice(1);
process.stdout.write(JSON.stringify({
  "the internet (1.1.1.1:443)": ["1.1.1.1", 443], "backend internal :8081": [backend, 8081],
  "booth-database postgres :5432": [pg, 5432], "minio :9000": [minio, 9000], "package index :8080": [pypi, 8080]}))' "$backend_pod" "$pg_pod" "$minio_pod" "$pypi_pod")
spec() { node -e 'process.stdout.write(JSON.stringify({targets: JSON.parse(process.argv[1]), dns: "example.com", timeout: 5}))' "$1"; }

from_app() { # TARGETS-JSON: the probe's JSON, from the app's Streamlit container
  local pod
  pod=$(wait_pod "$A") || return 1
  kubectl -n "$ns" exec -i "$pod" -c streamlit -- python - "$(spec "$1")" <"$here/fixtures/netprobe.py"
}
from_control() { # TARGETS-JSON: the same probe from an unrestricted pod (no NetworkPolicy in its namespace)
  local img b64
  img=$(kubectl -n "$ns" get deploy "app-$A" -o jsonpath='{.spec.template.spec.containers[?(@.name=="streamlit")].image}')
  b64=$(base64 -w0 <"$here/fixtures/netprobe.py")
  PROBE_IMAGE=$img probe "$probe_ns" netprobe-control "echo $b64 | base64 -d > /tmp/p.py && python /tmp/p.py '$(spec "$1")'" --image-pull-policy=Never | tail -1
}
ok=1
expect() { # LABEL GOT REGEX
  if echo "$2" | grep -Eq "$3"; then echo "ok: $1 ($2)"; else echo "FAIL: $1: got '$2', want /$3/"; ok=0; fi
}
each() { # RESULT-JSON TARGETS-JSON REGEX PREFIX: every target checked, and counted
  local name n=0 want
  want=$(echo "$2" | json 'Object.keys(d).length')
  # One name per line, each newline-terminated: `read` skips a last line without one (run
  # 38075897753 lost the last target of each group that way).
  while IFS= read -r name; do
    expect "$4$name" "$(echo "$1" | N="$name" json 'd[process.env.N]')" "$3"
    n=$((n + 1))
  done < <(echo "$2" | json 'Object.keys(d).map(k => k + "\n").join("")')
  [ "$n" = "$want" ] || { echo "FAIL: checked $n of $want targets"; ok=0; }
}

step "apps.egress.mode=open: denied destinations are dropped, each with its positive control"
r=$(from_app "$denied"); echo "app: $r"
each "$r" "$denied" '^closed:TimeoutError$' "denied (dropped): "
c=$(from_control "$denied"); echo "control: $c"
each "$c" "$denied" '^open$' "control (unrestricted pod connects): "

step "apps.egress.mode=open: allowed destinations connect"
r=$(from_app "$allowed"); echo "app: $r"
each "$r" "$allowed" '^open$' "allowed: "
expect "DNS resolves" "$(echo "$r" | json 'd.dns')" '^[0-9.]+$'

step "apps.egress.mode=closed: the internet is dropped too; DNS and the explicit rules still work"
helm get values booth-streamlit -n "$ns" -o json >/tmp/egress-values.json
node -e '
const fs = require("fs"); const v = JSON.parse(fs.readFileSync("/tmp/egress-values.json", "utf8"));
v.apps.egress = Object.assign({}, v.apps.egress, {mode: "closed"});
fs.writeFileSync("/tmp/egress-values-closed.json", JSON.stringify(v));'
helm upgrade booth-streamlit "$repo/charts/booth-streamlit" -n "$ns" -f /tmp/egress-values-closed.json >/dev/null
kubectl -n "$ns" rollout status deploy/booth-streamlit --timeout=300s >/dev/null
# The policy, not the pod, changes: wait until the app-pods policy has no internet rule.
for _ in $(seq 1 30); do kubectl -n "$ns" get networkpolicy booth-streamlit-apps -o yaml | grep -q "0.0.0.0/0" || break; sleep 1; done
kubectl -n "$ns" get networkpolicy booth-streamlit-apps -o yaml | grep -q "0.0.0.0/0" && fail "the closed-mode policy still has the internet rule"
# Calico programs a changed policy within seconds: allow a few probes for it to converge, and accept
# only a drop at the end.
for _ in $(seq 1 6); do
  r=$(from_app "$allowed")
  [ "$(echo "$r" | json 'd["the internet (1.1.1.1:443)"]')" = "closed:TimeoutError" ] && break
  sleep 2
done
echo "app: $r"
expect "closed: the internet (dropped)" "$(echo "$r" | json 'd["the internet (1.1.1.1:443)"]')" '^closed:TimeoutError$'
for name in "backend internal :8081" "booth-database postgres :5432" "minio :9000" "package index :8080"; do
  expect "closed, still allowed: $name" "$(echo "$r" | N="$name" json 'd[process.env.N]')" '^open$'
done
expect "closed: DNS still resolves" "$(echo "$r" | json 'd.dns')" '^[0-9.]+$'
helm upgrade booth-streamlit "$repo/charts/booth-streamlit" -n "$ns" -f /tmp/egress-values.json >/dev/null
kubectl -n "$ns" rollout status deploy/booth-streamlit --timeout=300s >/dev/null
for _ in $(seq 1 30); do kubectl -n "$ns" get networkpolicy booth-streamlit-apps -o yaml | grep -q "0.0.0.0/0" && break; sleep 1; done
kubectl -n "$ns" get networkpolicy booth-streamlit-apps -o yaml | grep -q "0.0.0.0/0" || fail "open mode was not restored"

for _ in $(seq 1 30); do api "$owner2" DELETE "/api/apps/$A"; [ "$code" = 204 ] && break; sleep 2; done
[ "$ok" = 1 ] || fail "egress: see the FAIL lines above"
echo "egress: all checks passed"
