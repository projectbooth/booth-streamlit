#!/usr/bin/env bash
# Deploys a real Keycloak and a real booth-core (built from a pinned checkout), then booth-streamlit
# with its chart defaults, so core itself provisions the module's database (ADR 0053) and mints its
# event-bus credential from the manifest's `events` (ADR 0050), the path a real install takes.
# The module is installed with test values for the lifecycle checks (lifecycle.sh): a running-app
# cap of 1 and a 60s idle timeout, and the app runtime image loaded into the cluster.
#
#   test/integration/deploy-realcore.sh <booth-core checkout> <core image> <streamlit image> <app-runtime image>
#     <booth-database checkout> <booth-database image> \
#     <booth-storage checkout> <booth-storage image> <booth-catalog checkout> <booth-catalog image> \
#     <booth-lakehouse checkout> <booth-lakehouse image>
#
# booth-storage (a filesystem backend root per workspace on an emptyDir) and booth-catalog are the
# real modules the file read proxy reads through (files-access.sh). Both are told to trust
# booth-core's workload-token issuer, which is how an app's token is accepted; core's issuer URL is
# set explicitly so all three agree on it character for character.
#
# booth-lakehouse (with its bundled Lakekeeper) and a MinIO (realcore/minio.yaml) are what the lakehouse
# checks read (lakehouse-access.sh). booth-storage runs its s3-kind credential provider
# (credentialBroker.enabled) over that MinIO. booth-lakehouse trusts core's workload issuer through the
# booth-workload-minting-credentials Secret core writes (its chart default), not a value set here.
#
# booth-database (ADR 0081) is the real workspace database the data-access checks read through the
# credential sidecar; it is installed as release "db" in namespace booth-database, as booth-api's
# realstack test does.
#
# All images must already be loaded into the cluster (pullPolicy Never). The test password is
# generated per run and kept in the Secret keycloak/realcore-test-password for the later scripts.
set -euo pipefail

core_dir=$1
core_image=$2
image=$3
runtime_image=$4
database_dir=$5
database_image=$6
storage_dir=$7
storage_image=$8
catalog_dir=$9
catalog_image=${10}
lakehouse_dir=${11}
lakehouse_image=${12}
issuer=http://keycloak.keycloak.svc:8080/realms/booth
workload_issuer=http://booth-core.booth-system.svc:8080
ns=booth-streamlit
repo=$(cd "$(dirname "$0")/../.." && pwd)
here="$repo/test/integration/realcore"

echo "--- Keycloak (realm: owner, editor and viewer of acme-analytics; an owner of other-team)"
kubectl create namespace keycloak --dry-run=client -o yaml | kubectl apply -f - >/dev/null
password=$(openssl rand -hex 12)
kubectl -n keycloak create secret generic realcore-test-password --from-literal=password="$password" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n keycloak create secret generic keycloak-admin --from-literal=password="$(openssl rand -hex 12)" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null
realm=$(mktemp)
sed "s/__TEST_PASSWORD__/$password/g" "$here/realm-booth.json.tpl" >"$realm"
kubectl -n keycloak create configmap keycloak-realm --from-file=realm-booth.json="$realm" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null
rm -f "$realm"
kubectl apply -f "$here/keycloak.yaml" >/dev/null
# booth-core starts without Keycloak (it retries OIDC discovery), so don't wait here; wait at the end.

echo "--- booth-core from $core_dir ($(git -C "$core_dir" rev-parse --short HEAD 2>/dev/null || echo '?'))"
kubectl create namespace booth-system --dry-run=client -o yaml | kubectl apply -f -
# Core's own CRD, applied explicitly: helm never updates a CRD that already exists.
kubectl apply -f "$core_dir/charts/booth-core/crds/"
helm upgrade --install booth-core "$core_dir/charts/booth-core" --namespace booth-system \
  --set oidc.issuerUrl=http://keycloak.keycloak.svc:8080/realms/booth --set oidc.clientId=booth-design \
  --set-string iframeSigningKey="$(openssl rand -hex 32)" \
  --set workloadIdentity.issuerUrl="$workload_issuer" \
  --set image.repository="${core_image%:*}" --set image.tag="${core_image##*:}" --set image.pullPolicy=Never \
  --wait --timeout 10m
kubectl -n booth-system rollout status deploy/booth-core --timeout=300s

echo "--- booth-database from $database_dir ($(git -C "$database_dir" rev-parse --short HEAD 2>/dev/null || echo '?'))"
kubectl create namespace booth-database --dry-run=client -o yaml | kubectl apply -f - >/dev/null
helm upgrade --install db "$database_dir/charts/booth-database" -n booth-database \
  --set image.repository="${database_image%:*}" --set image.tag="${database_image##*:}" --set image.pullPolicy=Never \
  --wait --timeout 10m

echo "--- MinIO (the lakehouse's object store; a test fixture)"
kubectl apply -f "$here/minio.yaml" >/dev/null

echo "--- the test package index (fixtures/pypi_index.py, from the runtime image; for pip-access.sh)"
kubectl create namespace booth-pypi --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n booth-pypi create configmap pypi-index --from-file=pypi_index.py="$repo/test/integration/fixtures/pypi_index.py" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null
sed "s|__RUNTIME_IMAGE__|$runtime_image|" "$here/pypi.yaml" | kubectl apply -f - >/dev/null

echo "--- booth-storage from $storage_dir ($(git -C "$storage_dir" rev-parse --short HEAD 2>/dev/null || echo '?')): a filesystem root per workspace, and its s3 credential provider"
kubectl create namespace booth-storage --dry-run=client -o yaml | kubectl apply -f - >/dev/null
helm upgrade --install booth-storage "$storage_dir/charts/booth-storage" -n booth-storage \
  --set oidc.issuerUrl="$issuer" --set oidc.clientId=booth-design \
  --set oidc.workloadIssuerUrl="$workload_issuer" \
  --set credentialBroker.enabled=true \
  --set-json 'filesystem.roots=["/data/{workspace}"]' \
  --set-json 'filesystem.volumes=[{"name":"data","emptyDir":{}}]' \
  --set-json 'filesystem.volumeMounts=[{"name":"data","mountPath":"/data"}]' \
  --set image.repository="${storage_image%:*}" --set image.tag="${storage_image##*:}" --set image.pullPolicy=Never \
  --wait --timeout 10m

echo "--- booth-catalog from $catalog_dir ($(git -C "$catalog_dir" rev-parse --short HEAD 2>/dev/null || echo '?')), with a stand-in metadata Postgres (as booth-api's realstack test)"
kubectl create namespace booth-catalog --dry-run=client -o yaml | kubectl apply -f - >/dev/null
catpw=$(openssl rand -hex 12)
kubectl -n booth-catalog apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata: {name: catalog-pg, labels: {app: catalog-pg}}
spec:
  containers:
    - name: postgres
      image: postgres:16-alpine
      env:
        - {name: POSTGRES_USER, value: catalog}
        - {name: POSTGRES_PASSWORD, value: "$catpw"}
        - {name: POSTGRES_DB, value: catalog}
      readinessProbe: {exec: {command: ["pg_isready", "-U", "catalog"]}, periodSeconds: 2}
---
apiVersion: v1
kind: Service
metadata: {name: catalog-pg}
spec: {selector: {app: catalog-pg}, ports: [{port: 5432}]}
EOF
kubectl -n booth-catalog create secret generic catalog-db \
  --from-literal=dsn="postgres://catalog:$catpw@catalog-pg.booth-catalog.svc:5432/catalog?sslmode=disable" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n booth-catalog wait --for=condition=Ready pod/catalog-pg --timeout=180s >/dev/null
helm upgrade --install booth-catalog "$catalog_dir/charts/booth-catalog" -n booth-catalog \
  --set oidc.issuerUrl="$issuer" --set oidc.clientId=booth-design \
  --set workloadIdentity.issuerUrl="$workload_issuer" \
  --set nats.url=nats://booth-core-nats.booth-system.svc:4222 --set postgres.dsnSecret.name=catalog-db \
  --set image.repository="${catalog_image%:*}" --set image.tag="${catalog_image##*:}" --set image.pullPolicy=Never \
  --wait --timeout 10m

echo "--- booth-lakehouse from $lakehouse_dir ($(git -C "$lakehouse_dir" rev-parse --short HEAD 2>/dev/null || echo '?')), with its bundled Lakekeeper"
kubectl create namespace booth-lakehouse --dry-run=client -o yaml | kubectl apply -f - >/dev/null
# No --wait: its pods need the database and minting Secrets core writes once it sees the BoothModule.
helm upgrade --install booth-lakehouse "$lakehouse_dir/charts/booth-lakehouse" -n booth-lakehouse \
  --set identity.oidcIssuerUrl="$issuer" --set identity.oidcAudience=booth-design \
  --set image.repository="${lakehouse_image%:*}" --set image.tag="${lakehouse_image##*:}" --set image.pullPolicy=Never
kubectl -n booth-minio rollout status deploy/minio --timeout=300s

echo "--- booth-streamlit with chart defaults (database and bus credential from core), lifecycle test values"
kubectl create namespace "$ns" --dry-run=client -o yaml | kubectl apply -f -
# No --wait on purpose: the pod cannot start until core has written both Secrets, which it does
# only after it sees the BoothModule this install creates. verify.sh waits for that explicitly.
helm upgrade --install booth-streamlit "$repo/charts/booth-streamlit" --namespace "$ns" \
  --set image.repository="${image%:*}" --set image.tag="${image##*:}" --set image.pullPolicy=Never \
  --set apps.runtimeImage.repository="${runtime_image%:*}" --set apps.runtimeImage.tag="${runtime_image##*:}" \
  --set apps.imagePullPolicy=Never \
  --set apps.maxRunning=1 --set apps.idleTimeout=60s \
  --set dataAccess.enabled=true --set dataAccess.database.enabled=true --set dataAccess.refreshMax=20s \
  --set dataAccess.lakehouse.enabled=true --set dataAccess.lakehouse.egress.podSelector.app=minio \
  --set 'dataAccess.lakehouse.egress.namespaceSelector.kubernetes\.io/metadata\.name=booth-minio' \
  --set apps.pip.indexUrl=http://pypi.booth-pypi.svc:8080/simple --set apps.pip.trustedHost=pypi.booth-pypi.svc \
  --set apps.pip.timeout=60s \
  --set apps.pip.egress.podSelector.app=pypi \
  --set 'apps.pip.egress.namespaceSelector.kubernetes\.io/metadata\.name=booth-pypi'

kubectl -n keycloak rollout status deploy/keycloak --timeout=600s
kubectl -n booth-lakehouse rollout status deploy/booth-lakehouse-lakekeeper --timeout=600s
kubectl -n booth-lakehouse rollout status deploy/booth-lakehouse-api --timeout=600s
kubectl -n booth-pypi rollout status deploy/pypi --timeout=300s
