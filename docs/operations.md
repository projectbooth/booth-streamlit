# Operating booth-streamlit

For an operator installing booth-streamlit with Helm, for example on a homelab. What the module is
and how apps work is in the README; the data-access design is `docs/design-data-access.md`.

## What it needs

- **booth-core.** CI tests against booth-core `9e29999` (`CORE_REF` in
  `.github/workflows/integration.yml`). `Chart.yaml` declares no compatible range. The chart uses
  core's `BoothModule` CRD (`booth.projectbooth.io/v1alpha1`), which core's chart installs.
- **A dedicated namespace.** The backend creates, updates and deletes one set of objects per app
  in its own namespace (a Deployment, Service, ConfigMap and Secret, labelled
  `booth.projectbooth.io/component=app`). The chart's ResourceQuota applies to the whole namespace
  and requires every pod in it to declare limits. Don't share the namespace with other workloads.
- **One install per cluster.** The `BoothModule` is always named `streamlit` (id `streamlit`).
- **A CNI that enforces NetworkPolicy, egress included.** All of the egress rules below, and the
  rule that only app pods reach the backend's internal port, depend on it (ARCHITECTURE.md item 37b).
  - kind's default CNI (kindnet) enforces ingress but not egress: booth-spark measured it on kind
    v0.33.0. On such a cluster `apps.egress.mode` and every egress rule are silently not enforced.
  - Calico and k3s's default (flannel with its network-policy controller) are examples that enforce
    both. Check yours.
  - This repo's Integration runs on Calico, and `test/integration/egress.sh` asserts the denials.
  - Every app pod's gate also refuses requests without that app's bearer, whatever the CNI does.

booth-core writes Secrets into the namespace once it sees the `BoothModule`. **You don't create
any of them.**

| Secret (keys) | Written because | Needed by |
|---|---|---|
| `booth-database-credentials` (`dsn`) | The manifest declares `database: {enabled: true}` (`postgres.provisionedByCore`, default `true`; ADR 0053) | The backend: app definitions and the event outbox |
| `booth-event-bus-credentials` (`url`, `nats.creds`) | The manifest declares `events: {publish: [dashboard.*]}` (`eventBus.enabled`, default `true`; ADR 0050) | The backend: `dashboard.*` events |
| `booth-workload-minting-credentials` (`url`, `credential`) | The manifest declares `workloadIdentity: {mint: true}` (`dataAccess.enabled`) | The backend: minting each app's token |

None of these references is `optional`. **Until core has written a Secret, the backend pod waits
in `CreateContainerConfigError`.** That's normal for a minute after install. If it lasts, check
that core sees the module: `kubectl get boothmodules -A`.

Core also labels the namespace `booth.projectbooth.io/database-client=true` and
`booth.projectbooth.io/event-bus-client=true`. Core's own NetworkPolicies admit its PostgreSQL and
NATS only from namespaces with those labels, so don't remove them. booth-database's ingress uses
the first label as well.

## Chart values to set

| Value | Default | Set it when, and what it does |
|---|---|---|
| `identity.issuerUrl` | `http://booth-core.booth-system.svc.cluster.local:8080/iframe-identity` | **Must equal booth-core's `BOOTH_IFRAME_IDENTITY_ISSUER_URL` character for character.** It is compared exactly with the `iss` of core's `X-Booth-Identity` assertion. See below. |
| `identity.groupsClaim` | `groups` | Only if booth-core's groups claim differs. |
| `apps.maxRunning` | `5` | How many apps may run at once across the install (`0` = no cap). Raising it also needs `quota.hard` raised. At the cap, Start answers 409 ("too many apps are running; stop one first"), and opening a sleeping app shows "Too many apps are running right now…". |
| `apps.maxRunningPerWorkspace` | `0` (no cap) | The same, per workspace. |
| `apps.idleTimeout` | `30m` | An app with no open tab and no request for this long is suspended; opening it wakes it. A viewer waiting on the starting page counts as activity. `0` disables idle shutdown. |
| `apps.maxWebsocket` | `8h` | One tab's websocket is closed after this long; Streamlit reconnects and the viewer's identity is checked again. |
| `apps.egress.mode` | `open` | `open`: app pods may reach the internet over IPv4. They can't reach the private ranges `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16` or `100.64.0.0/10`, nor link-local `169.254.0.0/16` (the cloud metadata address). That means no booth-core, no other apps, and **no hosts on a 192.168.x home LAN**. `closed`: DNS only, plus the specific rules below. Any other value fails to render. |
| `apps.egress.dns.*` | kube-dns in `kube-system` | Your cluster DNS has other labels. |
| `apps.pip.indexUrl` | empty (PyPI) | You run a package mirror. Passed to pip as `PIP_INDEX_URL`. PyPI is only reachable with `apps.egress.mode=open`. |
| `apps.pip.trustedHost` | empty | The index is plain HTTP. pip ignores an HTTP index unless its host is trusted (`PIP_TRUSTED_HOST`). This gives up transport integrity for that host, so use it only for an index you control. |
| `apps.pip.egress.{namespaceSelector,podSelector,port}` | off (`port: 8080`) | The index is in the cluster: adds an egress rule for app pods. Both selectors are required; a podSelector alone fails to render. NetworkPolicy is per pod, so app code can reach the index too. |
| `apps.pip.timeout` | `5m` | The whole install's limit. A hung install fails the app instead of leaving it starting. |
| `apps.pip.siteSizeLimit` | `1Gi` | Room for an app's installed packages. It is added to that app's ephemeral-storage limit, so it counts in the quota. |
| `quota.hard` | pods 8, `limits.cpu` 9, `limits.memory` 8Gi, `limits.ephemeral-storage` 10Gi | Sized for `maxRunning=5` with both data-access sidecars and packages. Raise it with `maxRunning`. `quota.enabled=false` removes it. |
| `dataAccess.coreUrl` | `http://booth-core.booth-system.svc:8080` | booth-core is under another release name or namespace: core's in-cluster URL. Used for the broker, the gateway calls to storage, catalog and lakehouse, and minting. |

**When booth-core isn't release `booth-core` in namespace `booth-system`.** Core's chart builds the
issuer from its own release: `http://<core fullname>.<core namespace>.svc.cluster.local:<port>/iframe-identity`,
unless core's `iframeIdentity.issuerUrl` is set. Read the value core actually uses and copy it
exactly:

```sh
kubectl -n <core namespace> get deploy <core deployment> \
  -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="BOOTH_IFRAME_IDENTITY_ISSUER_URL")].value}'
```

Note the long `…svc.cluster.local` form. The shorter `…svc:8080` other Booth URLs use is a
different string and doesn't match. **If it doesn't match:**
- every app request, and every call the module's page makes to its API, is refused with 401 (the
  page's own files load, but it can't list anything);
- the backend logs `proxy: refused …` or `api: refused …` for each;
- nothing fails at startup, because the backend doesn't contact the issuer until a request
  arrives.

Set `dataAccess.coreUrl` to that core as well.

### Data access toggles

Data access (ADR 0104/0107) is off by default. Each toggle adds to every app pod:

| Value | Adds | Also needs |
|---|---|---|
| `dataAccess.enabled` | The minting declaration (and the Secret above). The backend's internal port 8081, admitted from app pods only. A `token` volume (memory) in the gate. `booth_streamlit.files` (the file read proxy, through the gate). The editor's catalog-dataset list for declared sources. No sidecar on its own. | booth-core's workload identity. Owners who have signed in within core's `workloadIdentity.ownerMaxAge` (default `168h`), or their apps' data pauses. |
| `dataAccess.database.enabled` | A `pg-sidecar` (core's credential sidecar, postgres mode, read only, the app's workspace) and `DATABASE_URL`. An egress rule to booth-database's Postgres (`dataAccess.database.egress.*`, port 5432). | booth-database (ADR 0081). |
| `dataAccess.lakehouse.enabled` | When the workspace **has a warehouse at the moment the app starts**: an `s3-sidecar` (s3 mode, read only, the warehouse prefix), the keys file mounted read-only in the app, and `AWS_SHARED_CREDENTIALS_FILE`, `AWS_CONFIG_FILE` and `BOOTH_WAREHOUSE_ROOT`. A warehouse created later reaches an app at its next start. | booth-lakehouse, and booth-storage with its s3 credential provider. For an in-cluster object store, `dataAccess.lakehouse.egress.*` (both selectors required). An external store goes through `apps.egress.mode=open`. |

Both sidecars use `dataAccess.sidecar.image` (digest-pinned) and `dataAccess.sidecar.resources`.
`dataAccess.files.*` sets the file proxy's limits: `maxObjectBytes` 512 MiB, `objectTimeout` 5m,
`metaTimeout` 30s, `maxConcurrent` 4 per app. `dataAccess.refreshMax` is a test setting; leave it
empty.

## Trust settings other modules need

Apps call booth-storage, booth-catalog and booth-lakehouse with a **workload token** that booth-core
mints, so each of those modules must trust core's workload-token issuer. That issuer is core's
`BOOTH_WORKLOAD_ISSUER_URL`: core's `workloadIdentity.issuerUrl`, or by default
`http://<core fullname>.<core namespace>.svc.cluster.local:<port>`. The setting is spelled
differently in each module:

| Module | Value | If it's missing or doesn't match |
|---|---|---|
| booth-storage | `oidc.workloadIssuerUrl` | Storage refuses the app's token. `booth_streamlit.files.read(...)` raises `PermissionError: storage refused the app's access`, and the backend logs `files: storage refused the app's token (<status>): …`. |
| booth-catalog | `workloadIdentity.issuerUrl` | Catalog refuses the token. Reading a dataset raises `PermissionError: catalog refused the app's access`, and the backend logs `files: catalog refused the app's token …`. The editor's source picker shows "booth-catalog could not be read", with `api: listing catalog datasets …` in the backend log. |
| booth-lakehouse | `identity.workloadIssuerUrl`, or nothing: when it's empty and booth-lakehouse's `workloadIdentity.enabled` is `true` (its default), it takes the `issuer` key of the Secret core writes for it | Apps start **without** the s3 sidecar. The backend logs `lifecycle: app <id> starts without lakehouse access: booth-lakehouse answered <status>`, and `booth_streamlit.pyarrow_fs()` raises `DataAccessOff` ("this app has no lakehouse access…"). |

**Read the value from core, don't copy a default:**

```sh
kubectl -n <core namespace> get deploy <core deployment> \
  -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="BOOTH_WORKLOAD_ISSUER_URL")].value}'
```

booth-catalog's chart comment calls core's default `http://booth-core.booth-system.svc:8080`, but
core's chart (at `9e29999`) defaults to the `.svc.cluster.local` form. Booth's real-core tests set
core's `workloadIdentity.issuerUrl` explicitly, so they don't exercise the default.

The Postgres sidecar doesn't involve these settings: it calls core's credential broker (through the
backend), and booth-database serves the lease.

## Events and health

`/healthz` is the backend's readiness probe and what booth-core polls. Its fields are strings:

| Field | Meaning |
|---|---|
| `status` | `ok`; `degraded` while the bus is connecting or any event has failed; `unavailable` (HTTP 503) when the database is unreachable. Only the database makes it 503. |
| `database` | `ok`, `unreachable` or `unconfigured`. |
| `eventBus` | `connected`, `connecting` or `disabled`. |
| `eventsPending` | `dashboard.*` events written but not yet acknowledged by the bus. Non-zero briefly after any change to a shared app, and for as long as the bus is down. |
| `eventsFailed` | Events given up on. Should be `0`. |
| `eventsLastError` | Present only while `eventsFailed` > 0: the most recent error. |

How events are delivered:
- Every change to a shared app writes its event to the `app_events_outbox` table in the change's
  own transaction. A drainer publishes them oldest first to booth-core's JetStream stream, and marks
  each one published only after the bus acknowledges it.
- **While the bus is disconnected, nothing is attempted.** An outage of any length costs no
  attempts; the events wait in Postgres and go out when the bus is back, including across a backend
  restart.
- **While connected, a failed publish is retried after 2s, then 4s, 8s and so on, capped at 5
  minutes.** The 20th failed attempt marks the event failed for good. That's about an hour of a bus
  that is up but keeps refusing the event (the waits alone add up to about 63 minutes, plus up to
  10 seconds per attempt).
- A failed event never holds up later events. It stays in the table, is logged as
  `events: giving up on <type> for app <id> …`, and `/healthz` turns `degraded`.
- Published events are deleted from the table after 7 days.

**When `eventsFailed` isn't 0:**
- Look at the failed rows in the module's database (`booth_mod_streamlit`; the DSN is in the
  `booth-database-credentials` Secret):

  ```sql
  SELECT id, workspace, app_id, event_type, attempts, last_error FROM app_events_outbox WHERE failed;
  ```

- After fixing the cause, setting `failed = false, attempts = 0, next_attempt_at = now()` on those
  rows makes the drainer pick them up again. That is how the code reads the table, but no test
  exercises this manual step.
- Editing a shared app's name, description or sources also publishes its full current state. A
  code-only edit publishes nothing.

Events are at-least-once, and booth-catalog applies them as idempotent upserts with tombstones
(ADR 0046), so a duplicate is harmless.

## What lives where, and what a backup must include

| State | Where | In a backup? |
|---|---|---|
| App definitions: name, description, source, `requirements.txt`, declared sources, sharing, desired state, owner, data-access pause, gate bearer. Also take-over history and the event outbox. | **Postgres:** the database `booth_mod_streamlit` that core provisions on its shared PostgreSQL (core's `postgresql` StatefulSet, on its PVC) | **Yes. This is the module's only state that can't be rebuilt.** Core's scheduled backup (ADR 0054; `postgresql.backup`, on by default, daily at 03:00) runs `pg_dump` for every database, this one included. By default it writes to a PVC in the same cluster; ADR 0109 item 1 requires setting `postgresql.backup.s3.bucket`, or copying the dumps, so they leave the node. |
| Per-app ConfigMaps (`app-<id>-src`), Services and Deployments | Cluster objects | No. The backend rebuilds them from Postgres within seconds (it reconciles every 5 seconds). |
| Per-app gate Secrets (`app-<id>-gate`, the bearer) | Cluster Secrets | No. The bearer is also in Postgres (`gate_bearer`), and the backend recreates a missing Secret from it. |
| `booth-database-credentials`, `booth-event-bus-credentials`, `booth-workload-minting-credentials` | Cluster Secrets, written by core | Only through core's own state: ADR 0109 item 2. The database password is in `booth-database-credentials` and in the Postgres role (core's backup also writes `globals.sql`, with roles and their password hashes). If the Secret is missing, core issues a new password. |
| Workload tokens, S3 keys, Postgres leases | Memory: the backend, and app pods' memory volumes | No. Re-minted and re-leased on demand. |
| Installed packages, app `/tmp` | App pods' `emptyDir` volumes | No. Recreated on every pod start by design. |
| `dashboard.*` events already published | booth-core's JetStream stream (core's NATS PVC, 7-day retention) | Core's concern. Pending events are in Postgres. |

**booth-streamlit itself has no PersistentVolumeClaims.** What ADR 0109's restore drill needs from
this module is the `booth_mod_streamlit` database. After a restore, the backend recreates every
app's cluster objects from it.

**No restore of this module has been tested.** ADR 0109 requires a drill before the standby counts.

## Known limits

- **User-authored app code runs same-origin with the shell** (ARCHITECTURE.md item 55). JavaScript
  in an app (`st.html`, components) can reach the shell's window and act as the signed-in viewer,
  including capturing their token, which is valid in all of their workspaces. The interim rule (ADR
  0105) is that only workspace owners may create or edit apps. The separate-origin fix isn't decided.
- **With `apps.egress.mode=open`, an app's code can send anything it can read to the internet.**
  That includes everything its owner can read in the workspace through data access, and the S3 keys
  in its keys file. Those keys keep working until their lease expires, even after the pod is gone.
  Use `closed` if that matters more than internet access.
- **What an app can read is its owner's whole workspace, capped at viewer** (ADR 0107): every table,
  file and warehouse object. Declared sources describe it; they don't restrict it.
- **NetworkPolicy is per pod, not per container.** App code can reach whatever its sidecars reach:
  booth-database's Postgres port, the object store, the package index, and the backend's internal
  port. It holds no credential for Postgres, and the internal port refuses requests without the
  app's bearer.
- **Revocation isn't instant.** An owner who loses access stops new data within about 6.7 minutes,
  when the app's token is renewed, and the pod is then rolled. S3 keys copied before that work until
  their lease expires.
- **Packages are installed on every start, with no cache.** Heavy packages make every start
  (including waking from idle) that much slower. A package can shadow a library the image ships,
  which affects only that app.
- `apps.egress.mode=open` allows IPv4 only.

## Upgrade and uninstall

**Upgrading the chart (`helm upgrade`):**
- **Migrations:** the backend Deployment uses Kubernetes' default rolling update (the chart sets no
  strategy). The new backend applies any pending database migrations at startup, each once, in one
  transaction under an advisory lock. There are no down-migrations; downgrading isn't tested.
- **Every running app restarts.** The gate in each app pod runs from the backend's own image, so a
  new backend image (or a new `apps.runtimeImage` tag, or a changed `apps.resources`) changes every
  app pod's spec. The backend then replaces each running app's pod.
  - App pods use the Recreate strategy, so each app is briefly down and its viewers reconnect.
  - A running app keeps the lakehouse warehouse it was started with.
  - Stopped and sleeping apps have no pod; they get the new spec when they next start.
- **Egress changes:**
  - Changing the egress *rules* (`apps.egress.dns.*`, `apps.pip.egress.*`, `dataAccess.*.egress.*`)
    changes only the NetworkPolicies, so no app restarts, except as below.
  - Apps with a `requirements.txt` do restart when `apps.egress.mode` or `apps.pip.egress.podSelector`
    changes. Their pip init container is told whether egress is closed with no index rule
    (`BOOTH_PIP_EGRESS_CLOSED`), so its spec changes.
- **Other changes that restart apps:**
  - Any `apps.pip.*` setting other than the egress rules restarts apps with a `requirements.txt`.
  - Turning `dataAccess.enabled` or `dataAccess.database.enabled` on or off restarts every running
    app.
  - Turning `dataAccess.lakehouse.enabled` **on** restarts nothing: running apps get the s3 sidecar at
    their next start, since the warehouse is only looked up then.
  - Turning it **off** restarts the running apps that have an s3 sidecar.

**Uninstalling (`helm uninstall`):**
- **Removed:** the chart's objects (the backend Deployment, Service, ServiceAccounts, Role,
  RoleBinding, NetworkPolicies, ResourceQuota and the `BoothModule`).
- **Garbage-collected by Kubernetes:** every app's Deployment is owned by the backend's Deployment,
  and each app's ConfigMap, Secret and Service by its own Deployment, so they go too.
- **Also garbage-collected:** core's three Secrets, which are owned by the `BoothModule` because
  they live beside it.
- **Kept:** the `booth_mod_streamlit` database and its role stay on core's PostgreSQL. booth-core has
  no code that drops a module's database. Whether a later reinstall picks the old apps back up isn't
  tested.
- **Left in booth-catalog:** nothing is published on uninstall, so shared apps' dashboards stay in
  booth-catalog.
- **Left on the namespace:** core's two labels.
