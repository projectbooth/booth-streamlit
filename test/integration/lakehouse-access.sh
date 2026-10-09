#!/usr/bin/env bash
# Data access, step c: the lakehouse (ADR 0107; docs/design-data-access.md item 3), against a real
# booth-core, booth-storage (its s3 credential provider), booth-lakehouse with Lakekeeper, a MinIO and
# real Keycloak users, after deploy-realcore.sh. Runs after files-access.sh and before data-access.sh:
# it pauses owner-user's access in section 5 and restores it at the end.
#
# "As user code" means `kubectl exec` into the app's streamlit container (see data-access.sh).
#
#   0. acme-analytics gets a warehouse (an s3 backend on MinIO, then booth-lakehouse's PUT, as its
#      owner); an app started after that gets the s3 sidecar, read only, scoped to the warehouse
#   1. the owner's app reads a Parquet file from the warehouse through booth_streamlit.pyarrow_fs()
#   2. it cannot write there: the lease is read (the owner is a workspace owner; the app is capped
#      at viewer and asks for read)
#   3. the S3 keys file IS readable by user code, as ADR 0107 accepts (item 8.3 of the plan)
#   4. the scanner, with the s3 sidecar in the pod: no workload token and no bearer anywhere user code
#      can look (it does find the S3 keys, which is item 3, not a finding)
#   5. a refused renewal rolls the pod: the new pod has no keys, and the helper says why
set -euo pipefail
source "$(cd "$(dirname "$0")" && pwd)/lib.sh"

step "sign in owner-user and owner2-user (both owners of acme-analytics)"
s=$(sessions owner-user owner2-user)
owner=$(field "$s" owner-user 3)
owner2=$(field "$s" owner2-user 3)
[ ${#owner} -gt 20 ] && [ ${#owner2} -gt 20 ] || fail "could not open sessions"

step "0. acme-analytics's warehouse: an s3 backend on MinIO, then booth-lakehouse's PUT /api/warehouse (as owner-user, through core's gateway)"
read -r -d '' seed <<'SH' || true
KC=http://keycloak.keycloak.svc:8080/realms/booth/protocol/openid-connect
GW=http://booth-core.booth-system.svc:8080/modules
t=$(curl -s -X POST "$KC/token" -d grant_type=password -d client_id=booth-design -d scope=openid -d username=owner-user -d "password=$PW" | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p')
h() { curl -s -o /tmp/out -w '%{http_code}' -H "Authorization: Bearer $t" -H "X-Workspace: acme-analytics" -H 'Content-Type: application/json' "$@"; }
echo "backend $(h -X POST -d '{"id":"lake","displayName":"lake","kind":"s3","config":{"endpoint":"http://minio.booth-minio.svc:9000","bucket":"lake","pathStyle":true},"credentials":{"accessKeyId":"booth-test","secretAccessKey":"booth-test-secret"}}' "$GW/storage/api/admin/backends")"
for i in $(seq 1 30); do
  c=$(h -X PUT -d '{"backendId":"lake","path":"acme-data"}' "$GW/lakehouse/api/warehouse")
  [ "$c" = 201 ] || [ "$c" = 409 ] && break
  sleep 5
done
echo "warehouse $c $(head -c 300 /tmp/out)"
echo "get $(h "$GW/lakehouse/api/warehouse") $(head -c 300 /tmp/out)"
SH
out=$(probe "$probe_ns" seed-lake "PW='$password'; $seed")
echo "$out"
echo "$out" | grep -q "^backend 20[01]" || fail "registering the s3 backend"
echo "$out" | grep -q "^get 200 .*\"backendId\":\"lake\"" || fail "acme-analytics has no warehouse"

src=$(node -e 'process.stdout.write(JSON.stringify("import streamlit as st\nst.write(\"lake app\")\n"))')
api "$owner" POST /api/apps "{\"name\":\"Lake A\",\"description\":\"\",\"source\":$src,\"shared\":true}"; [ "$code" = 201 ] || fail "create A: $code $body"
A=$(echo "$body" | json 'd.id')
api "$owner" POST "/api/apps/$A/start"; [ "$code" = 200 ] || fail "start A: $code $body"
# Keep A active through core's iframe proxy (idle timeout is 60s; kubectl exec isn't proxy traffic).
( while true; do curl -s -o /dev/null -H "Cookie: booth_iframe_session=$owner2" "$core/iframe/streamlit/apps/$A/_stcore/health"; sleep 10; done ) &
cleanup_pids+=($!)
ready=""
for _ in $(seq 1 120); do [ -n "$(app_pod "$A")" ] && [ "$(kubectl -n "$ns" get deploy "app-$A" -o jsonpath='{.status.readyReplicas}')" = 1 ] && { ready=1; break; }; sleep 2; done
[ -n "$ready" ] || fail "app A never became ready"
echo "app: A=$A (acme-analytics, owner-user)"

args=$(kubectl -n "$ns" get deploy "app-$A" -o jsonpath='{.spec.template.spec.containers[?(@.name=="s3-sidecar")].args}')
echo "s3-sidecar args: $args"
echo "$args" | grep -q -- '--access=read' || fail "no s3 sidecar, or it does not ask for read"
echo "$args" | grep -q -- '--scope={\\"backendId\\":\\"lake\\",\\"path\\":\\"acme-data\\"}' || fail "the s3 sidecar is not scoped to the warehouse"
root=$(as_user_code "$A" 'import booth_streamlit as b; print(b.warehouse_root())')
echo "warehouse root (app code's view): $root"
case "$root" in s3://lake/*) ;; *) fail "BOOTH_WAREHOUSE_ROOT: $root" ;; esac
key=${root#s3://}/streamlit-it/sales.parquet # bucket/key, pyarrow's form

step "1. the owner's app reads a Parquet file from the warehouse through booth_streamlit.pyarrow_fs()"
# The file is put there by the test with MinIO's root credentials (test setup, not the app).
as_user_code "$A" '
import io, base64, pyarrow as pa, pyarrow.parquet as pq
buf = io.BytesIO()
pq.write_table(pa.table({"region": ["north", "south"], "sales": [10, 20]}), buf)
print(base64.b64encode(buf.getvalue()).decode())' | base64 -d >/tmp/sales.parquet
kubectl -n booth-minio exec -i deploy/minio -- sh -c "mc alias set t http://127.0.0.1:9000 booth-test booth-test-secret >/dev/null && mc pipe t/$key >/dev/null && echo put" </tmp/sales.parquet
READ='import sys, pyarrow.parquet as pq, booth_streamlit as b
try:
    t = pq.read_table(sys.argv[1], filesystem=b.pyarrow_fs())
    print("rows=" + ",".join("%s:%s" % (r, s) for r, s in zip(t["region"].to_pylist(), t["sales"].to_pylist())))
except b.DataAccessPaused as e:
    print("paused=" + str(e))
except Exception as e:
    print("error=" + type(e).__name__ + ": " + str(e).splitlines()[0])'
out=$(as_user_code "$A" "$READ" "$key")
echo "$out"
echo "$out" | grep -qx "rows=north:10,south:20" || fail "the app could not read the warehouse's Parquet file"

step "2. the lease is read: writing to the warehouse is refused by the object store"
out=$(as_user_code "$A" '
import sys, booth_streamlit as b
fs = b.pyarrow_fs()
try:
    with fs.open_output_stream(sys.argv[1]) as f:
        f.write(b"written by an app")
    print("write=ALLOWED")
except OSError as e:
    print("write=refused " + str(e).splitlines()[0][:200])' "${key%/*}/written.txt")
echo "$out"
echo "$out" | grep -q "^write=refused .*\(AccessDenied\|Access Denied\|ACCESS_DENIED\)" || fail "the app could write to the warehouse"
kubectl -n booth-minio exec deploy/minio -- sh -c "mc alias set t http://127.0.0.1:9000 booth-test booth-test-secret >/dev/null && mc stat t/${key%/*}/written.txt" >/dev/null 2>&1 \
  && fail "the object the app tried to write exists"

step "3. the S3 keys file IS readable by user code (ADR 0107 accepts this; plan item 8.3)"
out=$(as_user_code "$A" '
import os, stat
p = os.environ["AWS_SHARED_CREDENTIALS_FILE"]
st = os.stat(p)
body = open(p).read()
print("keys-file mode=%o uid=%d app-uid=%d" % (stat.S_IMODE(st.st_mode), st.st_uid, os.getuid()))
print("keys-readable-by-user-code=" + ("yes" if "aws_secret_access_key" in body else "no"))
print("config=" + open(os.environ["AWS_CONFIG_FILE"]).read().replace("\n", " | "))')
echo "$out"
echo "$out" | grep -qx "keys-readable-by-user-code=yes" || fail "expected user code to read the S3 keys (the sidecar's s3 mode)"
echo "$out" | grep -q "^keys-file mode=600 uid=65532 app-uid=65532" || fail "keys file mode or owner"

step "4. the scanner with the s3 sidecar present: no workload token, no bearer (the S3 keys are item 3)"
bearer=$(kubectl -n "$ns" get secret "app-$A-gate" -o jsonpath='{.data.bearer}' | base64 -d)
bearer_sha=$(printf '%s' "$bearer" | sha256sum | cut -d' ' -f1)
scan=$(kubectl -n "$ns" exec -i "$(wait_pod "$A")" -c streamlit -- python - "$issuer" "$bearer_sha" <"$here/fixtures/scanner.py")
echo "$scan"
[ "$(echo "$scan" | json 'd.found.decoy')" = true ] || fail "the scanner did not find its own decoy: the scan is broken, not clean"
[ "$(echo "$scan" | json 'd.found.workload_jwt.length')" = 0 ] || fail "user code can read a workload token: $(echo "$scan" | json 'd.found.workload_jwt')"
[ "$(echo "$scan" | json 'd.found.bearer.length')" = 0 ] || fail "user code can read the gate bearer: $(echo "$scan" | json 'd.found.bearer')"
[ "$(echo "$scan" | json 'd.found.sa_token')" = false ] || fail "user code has a service-account token"
[ "$(echo "$scan" | json 'd.counts.ports.includes(8091)')" = true ] || fail "the scanner did not probe the s3 sidecar's health port"
# The scanner walks without following symlinks, and /var/run is a symlink to /run in the image, so
# the keys file is reported as /run/booth/s3/credentials (Integration run 37928618395).
[ "$(echo "$scan" | json 'd.found.s3_keys.some(p => p === "/var/run/booth/s3/credentials" || p === "/run/booth/s3/credentials")')" = true ] \
  || fail "the scanner did not see the S3 keys file it should be able to read (is it scanning?)"
echo "expected, not a finding: the S3 keys are readable at $(echo "$scan" | json 'd.found.s3_keys.join(", ")')"

step "5. a refused renewal rolls the pod: the new pod has no keys, and the helper says why"
old_pod=$(app_pod "$A")
old_keys=$(as_user_code "$A" 'import os; print(open(os.environ["AWS_SHARED_CREDENTIALS_FILE"]).read())')
kc_groups owner-user remove /workspaces/acme-analytics/owner
sessions owner-user >/dev/null # core records the lost role
reason=""
for _ in $(seq 1 60); do
  api "$owner2" GET "/api/apps/$A"
  reason=$(echo "$body" | json 'd.dataPausedReason||""')
  [ -n "$reason" ] && break
  sleep 2
done
[ -n "$reason" ] || fail "the app never showed data access as paused"
echo "paused: $reason"
new_pod=""
for _ in $(seq 1 90); do
  new_pod=$(app_pod "$A")
  [ -n "$new_pod" ] && [ "$new_pod" != "$old_pod" ] && [ "$(kubectl -n "$ns" get deploy "app-$A" -o jsonpath='{.status.readyReplicas}')" = 1 ] && break
  sleep 2
done
[ -n "$new_pod" ] && [ "$new_pod" != "$old_pod" ] || fail "the pod was not rolled"
echo "rolled: $old_pod -> $new_pod"
out=$(as_user_code "$A" '
import os
print("keys-file=" + ("present" if os.path.exists(os.environ["AWS_SHARED_CREDENTIALS_FILE"]) else "absent"))')
echo "$out"
echo "$out" | grep -qx "keys-file=absent" || fail "the new pod has S3 keys although its owner has no access"
out=$(as_user_code "$A" "$READ" "$key")
echo "$out"
echo "$out" | grep -q "^paused=.*no longer has access" || fail "the helper did not raise DataAccessPaused with the reason"
# Informational, the residual ADR 0107 accepts (plan item 8.3/8.5): keys copied out before the pause
# keep working until their lease expires, wherever they were copied to. Here, from the new pod.
info=$(as_user_code "$A" '
import sys, configparser
from pyarrow import fs
c = configparser.ConfigParser(); c.read_string(sys.argv[1])
s3 = fs.S3FileSystem(access_key=c["default"]["aws_access_key_id"], secret_key=c["default"]["aws_secret_access_key"],
                     endpoint_override="minio.booth-minio.svc:9000", scheme="http")
try:
    s3.get_file_info(sys.argv[2]); print("still read")
except OSError as e:
    print("refused: " + str(e).splitlines()[0][:120])' "$old_keys" "$key" 2>&1 || true)
echo "info: S3 keys copied before the pause, used after the roll: $info (expected: still read, until the lease expires)"

step "restore owner-user for the next script"
kc_groups owner-user add /workspaces/acme-analytics/owner
sessions owner-user >/dev/null
api "$owner2" DELETE "/api/apps/$A"; [ "$code" = 204 ] || fail "delete A: $code"
echo "lakehouse access: all checks passed"
