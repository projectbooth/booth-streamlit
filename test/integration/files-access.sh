#!/usr/bin/env bash
# Data access, step b: the file read proxy (ADR 0107; docs/design-data-access.md item 4), against a
# real booth-core, booth-storage, booth-catalog and real Keycloak users, after deploy-realcore.sh.
# Runs before data-access.sh: it pauses owner-user's access in section 7 and restores it at the end.
#
# "As user code" means `kubectl exec` into the app's streamlit container (see data-access.sh).
#
#   1. an owner's app reads a catalog-registered file and a storage object through
#      booth_streamlit.files (and lists, and reads the dataset's record)
#   2. PUT, POST and DELETE get 405
#   3. "..", an encoded "..", and encoded separators get 400
#   4. a content path outside the dataset's location gets 403
#   5. a backend and a dataset in another workspace get 404
#   6. app B's bearer (another workspace) reads nothing of app A's
#   7. a paused owner gets 403 with the reason
set -euo pipefail
source "$(cd "$(dirname "$0")" && pwd)/lib.sh"

step "sign in owner-user (acme-analytics) and outsider-user (other-team)"
s=$(sessions owner-user)
owner=$(field "$s" owner-user 3)
s2=$(SESSION_WS=other-team sessions outsider-user)
outsider=$(field "$s2" outsider-user 3)
[ ${#owner} -gt 20 ] && [ ${#outsider} -gt 20 ] || fail "could not open sessions"

step "seed: a filesystem backend, two objects and a catalog file dataset in each workspace (as each workspace's owner, through core's gateway)"
# Runs in-cluster: tokens must carry Keycloak's in-cluster issuer.
read -r -d '' seed <<'SH' || true
KC=http://keycloak.keycloak.svc:8080/realms/booth/protocol/openid-connect
GW=http://booth-core.booth-system.svc:8080/modules
tok() { curl -s -X POST "$KC/token" -d grant_type=password -d client_id=booth-design -d scope=openid -d "username=$1" -d "password=$PW" | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p'; }
# seed USER WS BACKEND OBJECT... : backend + objects + a dataset over the first object's directory
seed() {
  user=$1 ws=$2 backend=$3; shift 3
  t=$(tok "$user")
  c=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $t" -H "X-Workspace: $ws" -H 'Content-Type: application/json' \
    -d "{\"id\":\"$backend\",\"displayName\":\"$backend\",\"kind\":\"filesystem\",\"config\":{\"rootPath\":\"/data/$ws\"}}" "$GW/storage/api/admin/backends")
  echo "$ws backend $c"
  for o in "$@"; do
    c=$(curl -s -o /dev/null -w '%{http_code}' -X PUT -H "Authorization: Bearer $t" -H "X-Workspace: $ws" -H 'Content-Type: text/csv' \
      --data-binary "content of $o" "$GW/storage/api/backends/$backend/objects/$o")
    echo "$ws object $o $c"
  done
  dir=${1%/*}
  id=$(curl -s -X POST -H "Authorization: Bearer $t" -H "X-Workspace: $ws" -H 'Content-Type: application/json' \
    -d "{\"name\":\"$backend data\",\"description\":\"\",\"location\":{\"backendId\":\"$backend\",\"path\":\"$dir\"},\"format\":\"file\",\"owner\":\"\"}" \
    "$GW/catalog/api/datasets" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  echo "$ws dataset ${id:-none}"
}
seed owner-user acme-analytics files sales/q1.csv other/secret.csv
seed outsider-user other-team other-files theirs/x.csv
SH
out=$(probe "$probe_ns" seed-files "PW='$password'; $seed")
echo "$out"
echo "$out" | grep -qx "acme-analytics backend 201" || fail "registering acme's backend"
echo "$out" | grep -qx "acme-analytics object sales/q1.csv 201\|acme-analytics object sales/q1.csv 200" || fail "uploading acme's object"
ds=$(echo "$out" | sed -n 's/^acme-analytics dataset //p')
theirs=$(echo "$out" | sed -n 's/^other-team dataset //p')
[ -n "$ds" ] && [ "$ds" != none ] && [ -n "$theirs" ] && [ "$theirs" != none ] || fail "registering datasets"

src=$(node -e 'process.stdout.write(JSON.stringify("import streamlit as st\nst.write(\"files app\")\n"))')
api "$owner" POST /api/apps "{\"name\":\"Files A\",\"description\":\"\",\"source\":$src,\"shared\":true}"; [ "$code" = 201 ] || fail "create A: $code $body"
A=$(echo "$body" | json 'd.id')
api "$outsider" POST /api/apps "{\"name\":\"Files B\",\"description\":\"\",\"source\":$src,\"shared\":true}"; [ "$code" = 201 ] || fail "create B: $code $body"
B=$(echo "$body" | json 'd.id')
api "$owner" POST "/api/apps/$A/start"; [ "$code" = 200 ] || fail "start A: $code $body"
# Keep A active through core's iframe proxy (idle timeout is 60s; kubectl exec isn't proxy traffic).
( while true; do curl -s -o /dev/null -H "Cookie: booth_iframe_session=$owner" "$core/iframe/streamlit/apps/$A/_stcore/health"; sleep 10; done ) &
cleanup_pids+=($!)
ready=""
for _ in $(seq 1 120); do [ -n "$(app_pod "$A")" ] && [ "$(kubectl -n "$ns" get deploy "app-$A" -o jsonpath='{.status.readyReplicas}')" = 1 ] && { ready=1; break; }; sleep 2; done
[ -n "$ready" ] || fail "app A never became ready"
echo "apps: A=$A (acme-analytics, owner-user) B=$B (other-team, outsider-user, not running)"

# Raw requests to the gate's loopback /files listener, exactly as written (no path normalization),
# printing "<label> <status>". Run as user code.
RAW='import sys, os, urllib.request as u, urllib.error as e, http.client as hc
base = os.environ["BOOTH_FILES_URL"]  # http://127.0.0.1:8090/files
host, prefix = base.split("//", 1)[1].split("/", 1)
for label, method, path in [x.split(" ", 2) for x in sys.argv[1:]]:
    c = hc.HTTPConnection(host, timeout=30)
    c.request(method, "/" + prefix + path)
    r = c.getresponse()
    body = r.read(300).decode(errors="replace").replace("\n", " ")
    print(label, r.status, body[:160])'
raw() { kubectl -n "$ns" exec "$(app_pod "$A")" -c streamlit -- python -c "$RAW" "$@"; }

step "1. reads a storage object and a catalog file dataset through booth_streamlit.files"
out=$(as_user_code "$A" "
from booth_streamlit import files
print('object=' + files.read('files', 'sales/q1.csv').decode())
print('list=' + ','.join(e['path'] for e in files.list('files', prefix='sales/')))
d = files.dataset('$ds')
print('dataset=%s %s %s' % (d['format'], d['location']['backendId'], d['location']['path']))
print('content=' + files.read_dataset('$ds', 'sales/q1.csv').decode())
")
echo "$out"
echo "$out" | grep -qx "object=content of sales/q1.csv" || fail "reading the storage object"
echo "$out" | grep -q "^list=.*sales/q1.csv" || fail "listing"
echo "$out" | grep -qx "dataset=file files sales" || fail "the dataset record"
echo "$out" | grep -qx "content=content of sales/q1.csv" || fail "reading the dataset's file"

step "2-5. refusals: methods, traversal, encoded separators, outside the location, other workspaces"
out=$(raw "put PUT /storage/files/sales/q1.csv" "post POST /storage/files" "delete DELETE /datasets/$ds" \
  "dotdot GET /storage/files/sales/../other/secret.csv" \
  "encdotdot GET /storage/files/sales/%2e%2e/other/secret.csv" \
  "encslash GET /storage/files/sales%2Fq1.csv" \
  "encslashdotdot GET /storage/files/sales%2F..%2Fother%2Fsecret.csv" \
  "outside GET /datasets/$ds/content/other/secret.csv" \
  "otherbackend GET /storage/other-files/theirs/x.csv" \
  "otherdataset GET /datasets/$theirs")
echo "$out"
for want in "put 405" "post 405" "delete 405" "dotdot 400" "encdotdot 400" "encslash 400" "encslashdotdot 400" "outside 403" "otherbackend 404" "otherdataset 404"; do
  echo "$out" | grep -q "^$want " || fail "expected '$want'"
done

step "6. app B's bearer (other-team) reads nothing of app A's"
bearer_b=$(kubectl -n "$ns" get secret "app-$B-gate" -o jsonpath='{.data.bearer}' | base64 -d)
STEAL='import sys, urllib.request as u, urllib.error as e
url, bearer = sys.argv[1], sys.argv[2]
for label, path in [x.split(" ", 1) for x in sys.argv[3:]]:
    try:
        with u.urlopen(u.Request(url + path, headers={"Authorization": "Bearer " + bearer}), timeout=30) as r:
            print(label, r.status, r.read(80).decode())
    except e.HTTPError as err:
        print(label, err.code)'
out=$(kubectl -n "$ns" exec "$(app_pod "$A")" -c streamlit -- python -c "$STEAL" "http://booth-streamlit.$ns.svc:8081" "$bearer_b" \
  "a-object /files/storage/files/sales/q1.csv" "a-dataset /files/datasets/$ds" "a-content /files/datasets/$ds/content/sales/q1.csv" \
  "b-own /files/storage/other-files/theirs/x.csv")
echo "$out"
for want in "a-object 404" "a-dataset 404" "a-content 404"; do echo "$out" | grep -q "^$want" || fail "B's bearer: expected '$want'"; done
echo "$out" | grep -q "^b-own 200 content of theirs/x.csv" || fail "B's bearer should still resolve to B (its own workspace's file)"

step "7. a paused owner gets 403 with the reason"
kc_groups owner-user remove /workspaces/acme-analytics/owner
sessions owner-user >/dev/null # core records the lost role
got=""
for _ in $(seq 1 45); do
  got=$(raw "paused GET /storage/files/sales/q1.csv" || true)
  echo "$got" | grep -q '^paused 403 .*data_access_paused' && break
  sleep 2
done
echo "$got"
echo "$got" | grep -q '^paused 403 .*data_access_paused.*no longer has access' || fail "no 403 with the reason for a paused owner"
out=$(as_user_code "$A" "
import booth_streamlit as b
from booth_streamlit import files
try:
    files.read('files', 'sales/q1.csv'); print('read=allowed')
except b.DataAccessPaused as err:
    print('helper=DataAccessPaused: %s' % err)
")
echo "$out"
echo "$out" | grep -q "^helper=DataAccessPaused: .*no longer has access" || fail "the helper did not raise DataAccessPaused with the reason"

step "restore owner-user for the next script"
kc_groups owner-user add /workspaces/acme-analytics/owner
sessions owner-user >/dev/null
s=$(sessions owner2-user); owner2=$(field "$s" owner2-user 3)
api "$owner2" DELETE "/api/apps/$A"; [ "$code" = 204 ] || fail "delete A: $code"
api "$outsider" DELETE "/api/apps/$B"
echo "files access: all checks passed"
