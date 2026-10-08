# booth-streamlit data access (ADR 0104): plan for review

Status: **plan only, for the coordinator's ruling**, 2026-10-08. No implementation code exists.

Inputs:
- ADR 0104 and 0105, and ADR 0095 with `contracts/credential-sidecar.md`.
- ADR 0056/0058, and ADR 0103 with its per-(workspace, creator) amendment.
- booth-api's `docs/decisions/0006` and `0007`.
- Core's mint check (`internal/workload/service.go`, `ownerRole`). ADR 0103 item 4 confirmed it on
  2026-10-08: minting refuses unless the owner currently holds a role in that workspace and was
  seen within `BOOTH_WORKLOAD_OWNER_MAX_AGE`. The minted role is the lesser of the ceiling and the
  owner's role. I'll re-read that code before declaring `workloadIdentity: {mint: true}`.

## 1. Owner identity

The backend mints one workload token per app:
- `roleCeiling: viewer`
- `owner: <app's owner sub>`
- `workspace: <app's workspace>`
- `subject: streamlit:<workspace>`, as you specified; see item 8.4 for one suggestion.

Each app pod has exactly one identity, so this is ADR 0103's per-(workspace, owner) pattern with
one identity per pod. booth-api's in-process sidecar manager isn't needed.

The backend keeps the token only in memory and re-mints at two thirds of its life (booth-api's
rule). A refusal (403, `ErrOwnerNoAccess`) is remembered for 30 s.

**What the user sees when core refuses:**
- **In the module UI**, owners see the app as "Data access paused" with the reason: "<owner> no
  longer has access to <workspace>, or hasn't signed in for 7 days. Ask them to sign in, or take
  ownership." Taking ownership is a "Take ownership" button for any current workspace owner (the
  ADR 0088 fallback); it sets the app's owner and the next mint uses it.
- **Inside the app**, viewers see a clear failure: `booth_streamlit` helpers raise
  `DataAccessPaused` with that text, and the Postgres proxy refuses connections.
- **The app still renders.** Only its data stops.

## 2. The token is never readable by user code

**Which containers hold what.** Containers in a pod share the network namespace but not their
filesystems, and process-namespace sharing stays off (the default):

| Container | Runs | Mounts |
|---|---|---|
| `streamlit` | user code + pip packages | source (ro), `/tmp`, `site-packages` (ro), `s3-creds` (ro) |
| `gate` | module code | bearer Secret (ro), `token` (rw) |
| `pg-sidecar` | credential-sidecar, `postgres` | `token` (ro) |
| `s3-sidecar` | credential-sidecar, `s3` | `token` (ro), `s3-creds` (rw) |
| `pip` (init) | `pip install` (unvetted) | `requirements.txt` (ro), `site-packages` (rw). No token, no bearer |

`token` is a memory-backed `emptyDir`.

**Renewal without widening the Role.** The credential-sidecar binary can't fetch a token over
HTTP; it only re-reads `--token-file` on every broker call. So the gate (our code) is the
refresher:
1. It calls `POST /internal/apps/token` on the backend's internal port (8081), authenticated by the
   per-app bearer.
2. The backend resolves the bearer to an app and returns that app's current token.
3. The gate writes the token to `token/token` (0400, write-then-rename) and re-fetches at two
   thirds of its life.

No Kubernetes object ever holds the token, and the Role is unchanged (Secrets stay create-only).
I don't need any wider Role.

**What user code can reach on loopback:**
- `127.0.0.1:5432`: the Postgres proxy. That's the intended path.
- The gate's file listener (item 4).
- The gate's public port, which refuses anything without the bearer.

Nothing serves the token, and it's never in an environment variable or command line, only the
file path is.

**Proof, not assertion:** an integration test runs a **scanner app** as user code.
- **What it searches:**
  - every readable file on its filesystem;
  - every `/proc/*/environ` and `cmdline` it can see;
  - the HTTP answers of every loopback port the pod listens on.
- **What it looks for:**
  1. any JWT whose `iss` is core's workload issuer;
  2. the app's exact bearer, which the test reads from the Secret as cluster admin;
  3. the Kubernetes service-account token.
- **Control:** the test plants a known decoy JWT in the app's own `/tmp`. The scanner must find
  it, so a broken scanner fails instead of passing. This is booth-pipeline's isolation-test
  pattern.

## 3. Database and lakehouse (ADR 0095 sidecars)

- **`pg-sidecar`:**
  - Flags: `--kind=postgres --access=read --scope={"workspace":W} --workspace=W
    --token-file=/var/run/booth/token/token --listen=127.0.0.1:5432`.
  - App code gets `DATABASE_URL=postgresql://localhost:5432/<db>`, the same shape booth-notebooks
    uses, and works with `pandas.read_sql`, SQLAlchemy or psycopg.
  - Only when booth-database is set (`boothDatabase.url`, the ADR 0092 gate).
- **`s3-sidecar`:**
  - Flags: `--kind=s3 --access=read`, with scope `{backendId, path}` from booth-lakehouse
    `GET /api/warehouse`, which the backend calls with the app's token. A 404 means no s3 sidecar.
  - Two more flags: `--credentials-file=/var/run/booth/s3/aws` and `--health-listen=127.0.0.1:8091`.
    The default health port is 8080, which would collide with the gate in the same pod.
  - App code gets `AWS_SHARED_CREDENTIALS_FILE` and `AWS_CONFIG_FILE`.
  - `booth_streamlit.duckdb_secret()` and `pyarrow_fs()` wrap the endpoint, because pyarrow and
    DuckDB ignore the config file (ADR 0095 Finding 3). Same helper as booth-notebooks.
  - The sidecar runs as the Streamlit container's uid, as the contract requires.
- **`--core-url` points at the backend's internal port, not core.** The backend forwards only
  `POST /api/credentials` there, unchanged. App pods then need no egress to booth-core at all
  (see 8.1). I'll confirm against the binary that this is the only core call it makes.
- **What an app can see, honestly:**
  - A `read` Postgres lease is `SELECT` on every table in the workspace's `public` schema.
  - An `s3` read lease is the whole warehouse prefix, so every Iceberg table's data files.
  - The broker refuses narrower scopes, so **an app can read everything its owner, capped at
    viewer, can read in that workspace's database and lakehouse.**
  - Declared sources (item 6) describe the app; they don't restrict it.

## 4. Storage and catalog files: the read proxy

**Path of a request:** app code calls the gate's loopback listener `127.0.0.1:8090` (pod-local)
through `booth_streamlit.files`. The gate adds the bearer and forwards to the backend at
`:8081/files/...`. The backend resolves bearer to app, then calls booth-storage or booth-catalog
**through core's gateway** with the app's token and `X-Workspace: <app's workspace>`. Workload
tokens are accepted there (ADR 0059), and storage backends are keyed by (workspace, id), so no
other workspace is reachable.

**Endpoints (GET only):**
- `/files/storage/{backendId}?prefix=&cursor=`: list (booth-storage
  `GET /api/backends/{id}/objects`), at most 1000 entries per page.
- `/files/storage/{backendId}/{path}`: read an object, streamed through without buffering.
- `/files/datasets/{id}`: the catalog dataset (name, format, location).
- `/files/datasets/{id}/content/{path}`: read within the dataset's `{backendId, path}`. `format`
  must be `file`; `iceberg` and `postgres` tables are read through the sidecars instead.

**Limits** (chart values):
- Object reads: 512 MiB each, 5-minute timeout.
- Metadata and list calls: 30 s.
- At most 4 concurrent reads per app.

**Refused:**
- Any method but GET or HEAD: 405.
- `..`, a leading `/`, a backslash, NUL, or `%2f` decoding to a separator: 400.
- A content path outside the dataset's location: 403, with the same `/`-boundary rule as
  ADR 0046.
- A backend or dataset not in the app's workspace: 404, as booth-storage and catalog answer.
- An unknown or missing bearer: 401.
- A paused owner: 403 with the item 1 reason.

## 5. Egress and pip

**Egress** stays as built (open by default, every private and link-local range excluded, `closed`
allows DNS only). Two rules are added:
- to the backend's pods on 8081;
- to the data backends the sidecars must reach: booth-database's Postgres on 5432, and the
  storage backend's S3 port. Both are chart values and are only rendered when set.

**pip install:**
- **Where:** the app gets an optional `requirements.txt` (≤ 16 KiB), stored and delivered with the
  source. Changing it rolls the pod.
- **Who and how:** an **init container** from the runtime image, as uid 65532 with a read-only
  root filesystem, runs `pip install --no-cache-dir --target /opt/booth/site --timeout 20 -r ...`
  into an `emptyDir` (sizeLimit 1 GiB, counted in the quota). Streamlit mounts it read-only via
  `PYTHONPATH`. The init container mounts no token, bearer or credentials.
- **Persistence: none.** Packages are reinstalled on every pod start, including waking from idle.
  A few seconds for a couple of pure-Python packages; a minute or more for heavy wheels. Status
  shows "Installing packages" meanwhile. A cache would need a PVC and a wider Role, so not in v0.
- **Failure:** the init container writes the tail of pip's output as its termination message,
  which the backend reads from the pod status with the `list` it already has. Owners see
  "Failed: pip install: <last lines>". With `egress.mode=closed` that becomes "pip install needs
  internet access, and apps.egress.mode is closed".

## 6. Lineage and events

**What an app declares:** an owner picks catalog datasets in the editor. The list comes from the
backend, read as that owner (a token minted for the caller). They're stored as the app's
`sources`.

**When it publishes:**
- **On create, edit, re-own and delete** of a **shared** app, it publishes `dashboard.created`,
  `updated` or `deleted` with:
  - `dashboardId=<app id>`, plus `name`, `description` and `owner`;
  - `path: /streamlit`, since deep links into iframe modules aren't defined;
  - `lineageComplete: false`;
  - `sources: [{type: dataset, datasetId}]`, or `[]`.
- **Only shared apps appear.** Unsharing publishes `deleted`, because the catalog shows dashboards
  to every member and an unshared app is owners-only (ADR 0105).
- **Delivery is reliable:** events are written to an **outbox** table in the same transaction as
  the change, and drained to NATS with acks and `publishedAt` from the database clock. That's
  at-least-once, which is safe under ADR 0046's upsert and tombstone rules.
- The manifest already declares `events: {publish: [dashboard.*]}`.

## 7. Tests

**Real-core**, extending the existing job with booth-database, booth-storage and booth-catalog
pinned and installed as booth-api's `realstack` does:
- **Reads as owner:** an owner's app queries a Postgres table through `DATABASE_URL` and reads a
  catalog-registered file through `booth_streamlit.files`. The browser sees both results.
- **Viewer cap:** the owner is demoted to editor, and the app re-mints. The token's role is
  `viewer`, `INSERT` through `DATABASE_URL` fails, and a PUT to the file proxy gets 405.
- **Loss of access:** the owner is removed from the workspace in Keycloak and signs in again.
  - New mints are refused. The file proxy answers 403 within one token lifetime, and the module UI
    says "Data access paused".
  - On a refused renewal the lifecycle rolls the pod, so the Postgres proxy dies with it.
  - "Take ownership" by another owner restores data.
- **Theft and forgery:**
  - App B's bearer used on app A's file and token endpoints gets nothing. The bearer resolves to
    B's own app, so the workspace and owner are B's.
  - A forged `X-Booth-User` or `X-Workspace` on `:8081` is ignored; only the bearer identifies the
    app.
  - No bearer gets 401.
  - A pod not labelled as an app can't reach `:8081` (informational, depends on the CNI).
- **Token unreadable:** the scanner app from item 2.

**Unit tests:** mint and refusal caching, two-thirds renewal, the file-proxy path rules and
limits, outbox ordering, and pip failure messages.

## 8. Where ADR 0104 can't hold as written

1. **NetworkPolicy is per pod, not per container.** "Every in-cluster destination is denied except
   the sidecars' backends" therefore means user code can also reach those backends:
   booth-database's Postgres and the storage S3 port. It has no credentials for Postgres, but the
   in-cluster denial is weaker than the ADR reads. Routing broker calls through the backend keeps
   booth-core itself unreachable. I see no fix inside a pod, short of moving the sidecars to a
   separate per-app pod, which is a heavier design. Please rule on whether this residual is
   accepted.
2. **The broker grants whole-workspace read** (item 3). Declared sources can't narrow it.
3. **"Never in user code" holds for the workload token and the bearer, not for S3 keys.** By
   ADR 0095's design the `s3` keys file is readable by the app. With open egress, app code can
   copy those keys out, and they stay valid until the lease expires, at least MinIO's 15-minute
   floor, even after the pod is gone.
4. **`subject: streamlit:<workspace>`** is shared by every app in a workspace, so core's audit
   can't tell apps apart. `streamlit:<workspace>:<appId>` fits core's subject pattern. I'll use
   your value unless you prefer that one.
5. **Revocation lag:** a refused renewal stops new data within about 6.7 minutes. Rolling the pod
   is what ends an open Postgres lease (up to one hour) early. Copied S3 keys (8.3) outlive both.

Not built until you rule.
