# Integration tests (layer 3)

`contracts/testing-strategy.md` layer 3: a real kind cluster, run by `.github/workflows/integration.yml`
on merge to `main`, nightly, and by hand.

| Job | Deploys | Covers |
|---|---|---|
| `standins` | booth-core's BoothModule CRD (vendored, `fixtures/boothmodule-crd.yaml`), a throwaway PostgreSQL and an unauthenticated NATS, then this chart with its own database Secret and the bus URL given by hand | The chart installs; core's real CRD schema keeps every manifest field (nothing pruned); the backend reaches Postgres and NATS; `/healthz`; the UI is served with relative asset URLs; the service account has no Kubernetes API access. |
| `real-core` | A real Keycloak, a real booth-core built at a pinned commit (`CORE_REF`), and this chart with its defaults plus lifecycle test values (`apps.maxRunning=1`, `apps.idleTimeout=60s`) | Everything above, plus what only core can do: it provisions `booth-database-credentials` (ADR 0053), mints `booth-event-bus-credentials` from `events` (ADR 0050), the backend connects to core's bus with it, and core marks the module `Healthy`; the backend's API access is checked live against its Role. Then `api-authz.sh` (ADR 0105): through core's real session, an owner can create, edit, start, stop and delete an app; an editor and a viewer get 403 on every write; an owner of another workspace gets 404; the app is unchanged. Then `lifecycle.sh`: **start** (an owner's Start creates the app's objects and it becomes ready; then `iframe-path.sh`, a real Chromium through core, sees the app render over its websocket, round-trip a click, see the verified viewer and none of `X-Booth-Identity`, core's cookie or the gate bearer); **gate** (a forged `X-Booth-User` without the bearer, from a pod the NetworkPolicy admits, is refused 401 and logged; and, informationally, whether an outside pod is blocked by NetworkPolicy); **cap** (a second Start at the cap is 409 and creates no pod); **idle** (no viewer for the timeout suspends the app to 0 replicas without changing the owner's choice, and a viewer's visit wakes it); **reconcile** (a backend restart leaves the running app's pod alone); **stop** (0 replicas, and a viewer can't wake it); **delete** (the Deployment, Service, ConfigMap and Secret are all removed). Then `files-access.sh` (ADR 0107 step b, with real booth-storage and booth-catalog): an owner's app reads a storage object and a catalog file dataset through `booth_streamlit.files`; PUT, POST and DELETE get 405; `..`, an encoded `..` and encoded separators get 400; a content path outside the dataset's location gets 403; another workspace's backend and dataset get 404; another workspace's app's bearer reads nothing of app A's; a paused owner gets 403 with the reason. Then `lakehouse-access.sh` (ADR 0107 step c, with real booth-lakehouse and Lakekeeper, booth-storage's s3 credential provider and a MinIO): the workspace owner creates a warehouse; an app started afterwards has the s3 sidecar, read only, scoped to it; the app reads a Parquet file from the warehouse through `booth_streamlit.pyarrow_fs()`; its write there is refused by the object store (the lease is read); the S3 keys file **is** readable by user code (`0600`, the app's own uid), which ADR 0107 accepts; the scanner, with the s3 sidecar present and its health port probed, finds no workload token, bearer or service-account token, and reports the S3 keys as expected rather than as a finding; with the owner removed, the pod rolls, the new pod has no keys and the helper raises `DataAccessPaused` with the reason (and, informationally, keys copied out before the pause still read until their lease expires). Then `data-access.sh` (ADR 0107, with a real booth-database): an app queries a seeded Postgres table through `DATABASE_URL` and can't write; a scanner run as user code finds no workload token, bearer or service-account token anywhere it can read (files, `/proc`, loopback ports) but does find its planted decoy; another app's bearer resolves only to that app, forged headers and a readwrite request are refused; with the owner demoted to editor every mint is `granted=viewer` and writes stay refused; with the owner removed the app shows "data access paused", the pod rolls and data stops; another workspace owner takes ownership (logged) and data comes back. |

## Not covered yet

- Data access steps d and e (per-app `pip install`, `dashboard.*` publishing): not built yet.
- `booth_streamlit.duckdb_secret()` against real DuckDB: DuckDB isn't in the runtime image (it arrives with per-app `pip install`, step d), so it's unit-tested for the SQL it issues; the real-core read goes through `pyarrow_fs()`.
- Iceberg tables through the lakehouse's REST catalog: the s3 lease covers the warehouse's files, and the real-core check reads a Parquet object there; reading an Iceberg table needs PyIceberg or DuckDB's iceberg extension, neither in the image.
- An external object store (AWS S3, reached through `apps.egress.mode=open`): only the in-cluster MinIO with its egress rule is exercised.
- The file proxy's 512 MiB size limit and its 5-minute timeout against real storage (unit-tested only; a real half-gigabyte object would make the job slow for little gain).

## Running locally

```sh
kind create cluster --name booth-streamlit-ci
docker build -t booth-streamlit:ci . && kind load docker-image booth-streamlit:ci --name booth-streamlit-ci
test/integration/deploy-standins.sh booth-streamlit:ci
test/integration/verify.sh
```

For `real-core`, check out booth-core at `CORE_REF`, build and load its image and
`images/app-runtime` (as `booth-streamlit-app-runtime:ci`) the same way, then on a fresh cluster run
`deploy-realcore.sh <core checkout> booth-core:ci booth-streamlit:ci booth-streamlit-app-runtime:ci`,
`REAL_CORE=1 verify.sh`, and (after `npm ci && npx playwright install chromium` in `browser/`)
`iframe-path.sh`.

Point `KUBECONFIG` at a file of your own (`kind get kubeconfig --name <cluster> > file`) when other
work shares this machine: `kind create` and `kind delete` change the shared kubeconfig's current
context, and these scripts follow whatever it names.
