#!/usr/bin/env bash
# Data access, step a (ADR 0104/0107; docs/design-data-access.md item 7), against a real booth-core,
# a real booth-database and real Keycloak users, after deploy-realcore.sh installed the module with
# dataAccess.enabled, dataAccess.database.enabled and dataAccess.refreshMax=20s.
#
# "As user code" below means `kubectl exec` into the app pod's streamlit container: the same
# filesystem, environment, uid and process namespace an app's own code runs with.
#
#   1. reads as its owner: an app queries a Postgres table through DATABASE_URL
#   2. the token is unreadable by user code: the scanner (with its decoy control)
#   3. a stolen bearer from another app, forged headers, and a readwrite request get nothing
#   4. capped at viewer: with its owner demoted to editor the app still reads, never writes, and core
#      logs every mint for it as granted=viewer
#   5. the owner loses access: data stops, the app says why, the pod rolls
#   6. take ownership by another workspace owner: logged, and data comes back
set -euo pipefail

source "$(cd "$(dirname "$0")" && pwd)/lib.sh"

QUERY='import psycopg, booth_streamlit as b
try:
    with psycopg.connect(b.database_url(), connect_timeout=5) as c:
        print("rows=" + ",".join(r[0] for r in c.execute("SELECT name FROM demo ORDER BY id")))
        try:
            c.execute("INSERT INTO demo VALUES (99, %s)", ("written",))
            c.commit()
            print("insert=ALLOWED")
        except Exception as e:
            print("insert=refused " + type(e).__name__)
except b.DataAccessPaused as e:
    print("paused=" + str(e))
except Exception as e:
    print("error=" + type(e).__name__ + ": " + str(e).splitlines()[0])'
wait_query() { # APP-ID PATTERN SECONDS
  local out=""
  for _ in $(seq 1 "$3"); do
    out=$(as_user_code "$1" "$QUERY" 2>&1 || true)
    echo "$out" | grep -q "$2" && { echo "$out"; return 0; }
    sleep 2
  done
  echo "$out"
  fail "app $1: no '$2' within $(( $3 * 2 ))s"
}

step "sign in owner, second owner, viewer"
s=$(sessions owner-user owner2-user viewer-user)
owner=$(field "$s" owner-user 3); owner_sub=$(field "$s" owner-user 2)
owner2=$(field "$s" owner2-user 3); owner2_sub=$(field "$s" owner2-user 2)
for v in "$owner" "$owner2"; do [ ${#v} -gt 20 ] || fail "could not open sessions"; done

step "core labelled this namespace a booth-database client (its ingress policy admits it)"
test "$(kubectl get ns "$ns" -o jsonpath='{.metadata.labels.booth\.projectbooth\.io/database-client}')" = true || fail "namespace not labelled database-client"

step "seed acme-analytics's database (a readwrite credential from core's broker, as owner-user)"
seed='KC=http://keycloak.keycloak.svc:8080/realms/booth/protocol/openid-connect
tok=$(wget -qO- --post-data "grant_type=password&client_id=booth-design&scope=openid&username=owner-user&password=$PW" "$KC/token" | sed -n "s/.*\"access_token\":\"\([^\"]*\)\".*/\1/p")
cred=$(wget -qO- --header "Authorization: Bearer $tok" --header "X-Workspace: acme-analytics" --header "Content-Type: application/json" \
  --post-data "{\"kind\":\"postgres\",\"access\":\"readwrite\",\"scope\":{\"workspace\":\"acme-analytics\"}}" http://booth-core.booth-system.svc:8080/api/credentials)
g() { echo "$cred" | sed -n "s/.*\"$1\":\"\{0,1\}\([^\",}]*\)\"\{0,1\}.*/\1/p"; }
export PGPASSWORD=$(g password)
psql -h "$(g host)" -p "$(g port)" -U "$(g username)" -d "$(g database)" -v ON_ERROR_STOP=1 -q \
  -c "CREATE TABLE IF NOT EXISTS demo (id int PRIMARY KEY, name text)" \
  -c "INSERT INTO demo VALUES (1, '"'"'alpha'"'"'), (2, '"'"'beta'"'"') ON CONFLICT DO NOTHING" && echo seeded'
out=$(PROBE_IMAGE=postgres:16-alpine probe "$ns" seed-db "PW='$password'; $seed" --override-type=strategic --overrides="${limits/NAME/seed-db}")
echo "$out" | grep -qx seeded || fail "seeding failed: $out"

src=$(node -e 'process.stdout.write(JSON.stringify("import streamlit as st\nst.write(\"data app\")\n"))')
api "$owner" POST /api/apps "{\"name\":\"Data A\",\"description\":\"\",\"source\":$src,\"shared\":true}"; [ "$code" = 201 ] || fail "create A: $code $body"
A=$(echo "$body" | json 'd.id')
api "$owner2" POST /api/apps "{\"name\":\"Data B\",\"description\":\"\",\"source\":$src,\"shared\":true}"; [ "$code" = 201 ] || fail "create B: $code $body"
B=$(echo "$body" | json 'd.id')
api "$owner" POST "/api/apps/$A/start"; [ "$code" = 200 ] || fail "start A: $code $body"
# Keep A active the way a viewer does: a request through core's iframe proxy every 10s. The install
# uses a 60s idle timeout (for lifecycle.sh), and `kubectl exec` below never goes through the
# module's proxy, so without this idle shutdown would suspend A mid-test (it did, on the first run).
# A request also wakes A if it was suspended.
( while true; do curl -s -o /dev/null -H "Cookie: booth_iframe_session=$owner2" "$core/iframe/streamlit/apps/$A/_stcore/health"; sleep 10; done ) &
keepalive=$!
cleanup_pids+=("$keepalive")
ready=""
for _ in $(seq 1 120); do [ -n "$(app_pod "$A")" ] && [ "$(kubectl -n "$ns" get deploy "app-$A" -o jsonpath='{.status.readyReplicas}')" = 1 ] && { ready=1; break; }; sleep 2; done
[ -n "$ready" ] || { kubectl -n "$ns" describe deploy "app-$A" | tail -20; fail "app A never became ready"; }
echo "apps: A=$A (owner-user) B=$B (owner2-user)"

step "1. reads as its owner: the app queries a Postgres table through DATABASE_URL"
wait_query "$A" "rows=alpha,beta" 60 | tee /tmp/q1
grep -q "insert=refused" /tmp/q1 || fail "the app could write (viewer cap)"
as_user_code "$A" 'import os; print(os.environ["DATABASE_URL"])' | grep -q "^postgresql://localhost:5432/bdb_ws_" || fail "DATABASE_URL"

step "2. the workload token and the bearer are unreadable by user code (scanner, with a decoy control)"
bearer=$(kubectl -n "$ns" get secret "app-$A-gate" -o jsonpath='{.data.bearer}' | base64 -d)
bearer_sha=$(printf '%s' "$bearer" | sha256sum | cut -d' ' -f1)
scan=$(kubectl -n "$ns" exec -i "$(app_pod "$A")" -c streamlit -- python - "$issuer" "$bearer_sha" <"$here/fixtures/scanner.py")
echo "$scan"
[ "$(echo "$scan" | json 'd.found.decoy')" = true ] || fail "the scanner did not find its own decoy: the scan is broken, not clean"
[ "$(echo "$scan" | json 'd.found.workload_jwt.length')" = 0 ] || fail "user code can read a workload token: $(echo "$scan" | json 'd.found.workload_jwt')"
[ "$(echo "$scan" | json 'd.found.bearer.length')" = 0 ] || fail "user code can read the gate bearer: $(echo "$scan" | json 'd.found.bearer')"
[ "$(echo "$scan" | json 'd.found.sa_token')" = false ] || fail "user code has a service-account token"
[ "$(echo "$scan" | json 'd.counts.files')" -gt 500 ] || fail "the scanner read suspiciously few files"

step "3. a stolen bearer, forged headers, readwrite: nothing of app A's"
bearer_b=$(kubectl -n "$ns" get secret "app-$B-gate" -o jsonpath='{.data.bearer}' | base64 -d)
internal="http://booth-streamlit.$ns.svc:8081"
STEAL='import sys, json, base64, urllib.request as u
url, bearer, a, b, ws = sys.argv[1:6]
def post(path, headers, body=b""):
    try:
        with u.urlopen(u.Request(url + path, data=body, headers=headers, method="POST"), timeout=10) as r:
            return r.status, r.read()
    except u.HTTPError as e:
        return e.code, e.read()
code, body = post("/internal/token", {"Authorization": "Bearer " + bearer})
sub = json.loads(base64.urlsafe_b64decode(json.loads(body)["token"].split(".")[1] + "==")).get("sub") if code == 200 else None
print("stolen-bearer", code, "is-app-b" if sub == "streamlit:%s:%s" % (ws, b) else sub)
print("no-bearer", post("/internal/token", {"X-Booth-User": "owner", "X-Workspace": ws})[0])
print("forged-bearer", post("/internal/token", {"Authorization": "Bearer " + "0" * 64})[0])
fake = "e30." + base64.urlsafe_b64encode(json.dumps({"sub": "streamlit:%s:%s" % (ws, a)}).encode()).decode().rstrip("=") + ".x"
rw = json.dumps({"kind": "postgres", "access": "readwrite", "scope": {"workspace": ws}}).encode()
print("readwrite", *post("/internal/broker/api/credentials", {"Authorization": "Bearer " + fake, "X-Workspace": ws, "Content-Type": "application/json"}, rw))'
out=$(kubectl -n "$ns" exec "$(app_pod "$A")" -c streamlit -- python -c "$STEAL" "$internal" "$bearer_b" "$A" "$B" acme-analytics)
echo "$out"
echo "$out" | grep -qx "stolen-bearer 200 is-app-b" || fail "B's bearer did not resolve to B (and only B)"
echo "$out" | grep -qx "no-bearer 401" || fail "forged headers without a bearer"
echo "$out" | grep -qx "forged-bearer 401" || fail "a made-up bearer"
echo "$out" | grep -q "^readwrite 403 .*read access" || fail "the forwarder passed a readwrite request"
np=$(probe "$probe_ns" internal-outside "curl -s -o /dev/null -w '%{http_code}' -m 5 -X POST $internal/internal/token; true")
echo "info: unlabelled pod in another namespace -> backend :8081: $np (000 = blocked by NetworkPolicy; 401 = not enforced, the bearer check refused)"

step "4. capped at viewer: owner demoted to editor; the app reads, never writes, every mint granted=viewer"
kc_groups owner-user remove /workspaces/acme-analytics/owner
kc_groups owner-user add /workspaces/acme-analytics/editor
since=$(date -u +%Y-%m-%dT%H:%M:%SZ)
sessions owner-user >/dev/null # sign in again, so core records the new role
sleep 45 # past refreshMax (20s) for both the backend's re-mint and the gate's re-fetch
wait_query "$A" "rows=alpha,beta" 30 | tee /tmp/q4
grep -q "insert=refused" /tmp/q4 || fail "the editor-owned app could write"
mints=$(kubectl -n booth-system logs deploy/booth-core --since-time="$since" | grep "subject=streamlit:acme-analytics:$A " || true)
echo "$mints" | tail -2
[ -n "$mints" ] || fail "no mint for app A since the demotion"
echo "$mints" | grep -v "granted=viewer" && fail "a mint for app A was granted more than viewer"
echo "$mints" | grep -q "owner=$owner_sub ceiling=viewer granted=viewer" || fail "mint log shape"

step "5. the owner loses access: data stops, the app says why, the pod rolls"
old_pod=$(app_pod "$A")
kc_groups owner-user remove /workspaces/acme-analytics/editor
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
for _ in $(seq 1 90); do
  p=$(app_pod "$A"); [ -n "$p" ] && [ "$p" != "$old_pod" ] && [ "$(kubectl -n "$ns" get deploy "app-$A" -o jsonpath='{.status.readyReplicas}')" = 1 ] && break
  sleep 2
done
[ "$(app_pod "$A")" != "$old_pod" ] || fail "the pod was not rolled, so connections under the old lease could survive"
wait_query "$A" "paused=" 45

step "6. take ownership by another workspace owner: logged, and data comes back"
api "$owner2" POST "/api/apps/$A/take-ownership"
[ "$code" = 200 ] && [ "$(echo "$body" | json 'd.owner')" = "$owner2_sub" ] || fail "take ownership: $code $body"
kubectl -n "$ns" logs deploy/booth-streamlit --since=2m | grep -q "take ownership: app=$A workspace=acme-analytics by=$owner2_sub previous_owner=$owner_sub" \
  || fail "the take-over was not logged with who, which app and the previous owner"
api "$owner2" GET "/api/apps/$A/ownership-changes"
[ "$(echo "$body" | json 'd.changes.length')" = 1 ] || fail "ownership history: $body"
wait_query "$A" "rows=alpha,beta" 60

for id in "$A" "$B"; do api "$owner2" DELETE "/api/apps/$id"; done
echo "data access: all checks passed"
