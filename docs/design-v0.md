# booth-streamlit v0 design note

Status: **draft for coordinator review**, 2026-10-07. Nothing here is built yet. Sections (a) and
(b) describe what I intend to build. Section (c) is only a proposal: it puts user-written code
next to platform-issued credentials, so it needs an ADR before any of it is built. Open questions
are listed at the end instead of being settled here.

Inputs: ADR 0016, 0011, 0018, 0046, 0050, 0053, 0056/0058, 0069, 0077, 0095, 0096, the
`credential-sidecar` contract, and the kickoff defaults in `agent-briefs/streamlit.md` (apps are
self-contained Python, sharing is internal only, one container per app, identity comes from
`X-Booth-Identity`).

## (a) Per-app container lifecycle

### Precedent

ADR 0016 points at `booth-spark` for how to manage dynamically started user processes. **That repo
has no code yet** (an empty `.git` and nothing else), so there is nothing to borrow from it. I used
the two modules that already run user code in pods managed by a backend:

- `booth-pipeline`, Job per task (ADR 0096, `runners/kubejob.py`): a namespace-scoped Role, no
  ServiceAccount token in the user pod, a per-pod Secret that holds only a random bearer (never a
  platform token), a concurrency cap enforced by the backend, and cleanup on every exit path.
- `booth-notebooks`, KubeSpawner (ADR 0011): one long-lived pod per user, CPU/memory guarantees and
  limits, an idle culler (default 3600 s), and the `booth.projectbooth.io/workspace` label set by the
  spawner (ADR 0077).

A Streamlit app looks like a notebook server (long-lived, interactive, one websocket per browser
tab) but is managed the way pipeline manages Jobs (the module backend is the only controller).

### Shape

- **The database is the source of truth.** An app row holds id, workspace, owner `sub`, name, the
  Python source (self-contained, per the kickoff), its sharing list, a desired state
  (`running`/`stopped`), and the source's content hash.
- **One Deployment (replicas 0 or 1) plus one ClusterIP Service per app**, created by the backend in
  the module's own namespace, labelled `booth.projectbooth.io/app-id` and
  `booth.projectbooth.io/workspace` (ADR 0077, set from authenticated state, never from user code).
  I chose a Deployment over a bare Pod so the kubelet restarts a crashed app and a node restart
  doesn't lose it, and because "stop" is just scaling to 0, which keeps the object to scale back up.
  Each object carries an ownerReference to the module's own Deployment, so uninstalling the chart
  garbage-collects every app. Otherwise runtime-created objects would survive `helm uninstall`.
- **Code delivery:** the source goes into a per-app ConfigMap, mounted read-only. The content hash
  goes on the pod template as an annotation, so saving new code rolls the pod. ConfigMaps cap out
  around 1 MiB, which is fine for single-file apps; the backend enforces a smaller limit.
- **Runtime image:** one module-owned base image with Streamlit and a fixed set of data libraries
  (pandas, pyarrow, duckdb, SQLAlchemy, psycopg, boto3) plus a small `booth_streamlit` helper.
  v0 has no per-app `pip install` (see open question Q4).
- **States:** `stopped → starting → running → stopping → stopped`, plus `failed`. `failed` covers an
  image pull failure, CrashLoopBackOff, or a readiness timeout; the reason is reported (pipeline's
  `_FATAL_WAITING` list is the model). A reconcile loop on startup and every N seconds lists the
  labelled Deployments and converges them to the database, so a backend restart loses nothing.

### Start, stop, idle shutdown

- **Start:** an explicit Start action, or the first request through the proxy to a stopped app. The
  proxy scales it to 1 and serves a small "starting…" page that polls until the pod is Ready (a
  readiness probe on Streamlit's `/_stcore/health`). That is how idle apps can restart: a viewer
  opening a stopped app starts it, provided the cap allows it.
- **Stop:** an explicit Stop action, deletion, or idle shutdown. All of them scale to 0.
- **Idle shutdown:** the backend is the app's only ingress, so it knows activity exactly. It tracks
  the last request time and the number of open websockets per app. An app with no open websocket
  and no request for `idleTimeout` (default 30 min, chart value) is scaled to 0. An open browser tab
  holds Streamlit's websocket open and counts as activity. Because of that, an abandoned tab would
  keep an app up forever, so websockets also get a maximum lifetime (default 8 h). After that the
  backend closes the socket, Streamlit's client reconnects through the proxy, and the identity is
  re-checked (see (b)).

### Limits

- **Per app:** requests of 100m CPU / 256Mi, limits of 1 CPU / 1Gi, an ephemeral-storage limit, and
  a size-limited `emptyDir` for `/tmp`. All are chart values.
- **Running-app cap:** enforced by the backend. Default 5 for the whole install, plus an optional
  cap per workspace. When the cap is reached, a start is refused with a clear message. v0 does not
  stop someone else's app to make room.
- **Backstop:** a namespace ResourceQuota sized to cap × per-app limits plus the backend, so a
  backend bug can't over-commit a single node.
- **Hardening**, the same as pipeline task pods: non-root, read-only root filesystem, all
  capabilities dropped, `RuntimeDefault` seccomp, `automountServiceAccountToken: false`, no
  ServiceAccount permissions at all.
- **Network:** a NetworkPolicy on app pods allows ingress only from the backend pods on the app
  port. Egress allows DNS, plus (only once (c) is ratified) the data backends the sidecars need.
  Internet egress is closed by default; see Q4.
- **Backend RBAC**, namespace-scoped like pipeline's: create/update/delete/get/list/watch on
  Deployments, Services and ConfigMaps; get/list/watch on Pods; create/delete on Secrets (only for
  the per-app bearer in (b)). No `secrets get`. This deliberately breaks the scaffold's "no
  Kubernetes API access" contract test, which gets rewritten to pin exactly this Role.

## (b) Identity: how an app learns who the user is, and how its traffic is authenticated

### Path

```
browser ─ /iframe/streamlit/apps/<id>/… ─▶ booth-core iframe proxy ─▶ booth-streamlit backend ─▶ app pod
                                           (+ X-Booth-Identity)       verify, authorize,          gate ─▶ Streamlit
                                                                       re-header                  (127.0.0.1)
```

1. **Core → backend.** Core strips any client-supplied `X-Booth-Identity` and attaches a fresh one
   on every proxied request (ADR 0069). It is an RS256 JWT with `aud=streamlit`, `sub`, `groups`
   (ADR 0025 grammar plus `/platform/operator`), and expires in at most 2 min. Core forwards only
   the path remainder (`/apps/<id>/…`). A root-relative request that escapes the prefix is caught
   by core's cookie-keyed fallback and arrives with its original path.
2. **The backend is the trust boundary.** On every non-health request, including the websocket
   upgrade, it verifies the assertion against core's iframe-identity issuer (OIDC discovery + JWKS,
   asymmetric algorithms only, exact-string `iss` match; ADR 0069's notes warn about the
   `svc` vs `svc.cluster.local` spelling). It rejects workload-shaped subjects (`<kind>:<id>`), as
   notebooks does. It derives the role from `groups` itself (ADR 0041), not from `X-Booth-Role`.
   Then it checks the app: same workspace, and the caller is the owner or on the share list. Only
   then does it proxy.
3. **Backend → app.** The backend removes every inbound `X-Booth-*` header, **including
   `X-Booth-Identity`**. That assertion is a bearer credential for this module's own API, valid for
   up to 2 minutes. If it were forwarded, an app author's code could capture a viewer's assertion
   and replay it against the backend as that viewer. The backend then sets plain headers it has
   already verified: `X-Booth-User` (the `sub`), `X-Booth-Workspace`, `X-Booth-Role`, plus the
   per-app bearer described below.
4. **App code reads identity** through the base image's helper, `booth_streamlit.user()`, which
   reads `st.context.headers`. Streamlit documents those as the "headers sent in the initial
   request", so identity is fixed for the life of a Streamlit session. Since one session is one
   viewer's tab, that is correct. The backend's websocket lifetime cap bounds how long a session
   outlives a revoked membership.
5. **No Streamlit login.** The image ships no `[auth]` section in `secrets.toml`, so `st.login` has
   no provider and cannot be used. There is no second auth scheme anywhere.

Streamlit runs with `server.baseUrlPath=apps/<id>`, so its assets and `/_stcore/stream` websocket
resolve under the prefix in both the shell's view (`/iframe/streamlit/apps/<id>/…`) and the
backend's view (`/apps/<id>/…`). **Unverified:** Streamlit's frontend derives its base path from
`window.location`. I expect this to work through core's prefix-stripping, but the first build task
is to prove it against a real core and a real Streamlit, including the websocket, before building
on it.

Gap, not a blocker: the assertion carries a `sub` but no display name. Core's user directory
(ADR 0047) could resolve one, but it requires a bearer token, and the backend has none on the
iframe path. v0 shows the `sub`, or `preferred_username` if core adds it to the assertion.

### Authenticating backend → app traffic

App pods run another user's code, so the headers in step 3 are only trustworthy if nothing else
can reach the app pod.

- **NetworkPolicy:** app pod ingress is allowed only from the backend pods. ARCHITECTURE §7 item
  37(b) records that enforcement depends on the cluster's CNI. On a homelab I don't want this to be
  the only control.
- **Per-app bearer, checked in the pod.** At start, the backend generates a random bearer and
  stores it in a per-app Secret owner-referenced to the app's Deployment. This is the same pattern
  as pipeline's per-task Secret: it never holds a platform credential. A small module-owned Go
  **gate** container in the app pod listens on the pod IP, rejects any request without the bearer,
  and forwards to Streamlit, which binds `127.0.0.1` only. The bearer is mounted into the gate
  container only, never into the Streamlit container. So another user's app that somehow reaches
  this pod can't forge `X-Booth-User`, and the app's own code can't learn its bearer from the
  filesystem. (It shares the pod's network namespace, but it can only talk to its own Streamlit.)
- App pods have no route to the backend or to each other (egress policy), so app code can't
  call the module API sideways either. Every backend route verifies `X-Booth-Identity` regardless.

## (c) Data access without pasted credentials (proposal for an ADR; not to be built until ruled on)

### Whose identity does an app read as?

One Streamlit process serves every viewer of an app, and the credential sidecar's credentials are
per pod. So per-viewer credentials would mean one pod per viewer per app. That defeats the
single-node sizing and turns idle shutdown into per-viewer process management. **I propose the app
reads as its owner, capped at read**, which is the same semantics as a Superset/Metabase dashboard
on a service connection.

- The manifest adds `workloadIdentity: {mint: true}` (ADR 0056). For each running app, the backend
  mints a workload token with `subject=app:<id>`, `owner=<owner sub>`, `workspace=<app's
  workspace>`, `roleCeiling=viewer`.
- The pod gets the credential sidecar (ADR 0095, digest-pinned): `postgres` mode with
  `--access=read` for the workspace's booth-database (`DATABASE_URL` points at the sidecar on
  localhost), and `s3` mode with `--access=read`, scoped by `GET /api/warehouse` for booth-lakehouse
  tables, exactly as pipeline and notebooks resolve it.
- **Token delivery stays out of Kubernetes objects**, following pipeline's rule. The gate container
  from (b) fetches the current token from the backend using the per-app bearer. It writes the token
  to a memory-backed `emptyDir` mounted **only** into the gate and the sidecars, never the Streamlit
  container, and refreshes it before expiry. This is notebooks' `booth-token` helper pattern folded
  into a container we already need.

### What reaches the user's code

| Path | In the Streamlit container | Exposure |
|---|---|---|
| Postgres via sidecar | `DATABASE_URL=postgresql://localhost:5432/<db>`, no credential | Read access for as long as the app runs |
| Lakehouse via `s3` sidecar | Short-lived, read-only, warehouse-scoped keys in files the app must be able to read (the contract requires a matching uid). `pyarrow`/DuckDB need a helper (ADR 0095, fourth amendment) | Keys are readable by app code until the lease expires |
| Platform token (storage/catalog HTTP APIs) | **Not provided in this proposal** | See Q3 |

### Why I think this is acceptable, and where it isn't

- **No escalation through code edits.** The token is workspace-scoped and capped at `viewer`. Only
  members of the app's workspace can edit its code (if Q2 rules that way), and every member already
  has at least viewer read in that workspace. Anyone who can change what the code does already has
  the access the code runs with.
- **Viewers see what the owner can read.** That is inherent to a shared dashboard and is the point
  of sharing. It needs to be stated in the UI ("this app reads data as <owner>").
- **Exfiltration.** App code can send query results or the `s3` keys anywhere it can reach. This is
  why internet egress must default to closed (Q4).
- **Persistence after the owner leaves.** Minting stops once the owner hasn't been seen for
  `ownerMaxAge` (7 days). The app then loses data access ("goes dark"), the same limitation ADR 0084
  accepted for lakehouse. ADR 0088 lets any current workspace owner stand in as owner; the app UI
  could offer "re-own" for that.
- **Revocation lag.** Stopping or deleting an app stops renewal, but an issued Postgres lease lives
  up to one hour and an `s3` lease until its TTL (credential-sidecar contract, "Connection lifetime").
- **Lineage (ADR 0046).** The backend knows what an app *can* read, not what it *does* read.
  Publishing the warehouse as a `location` source would be fabricated lineage. I propose
  `lineageComplete: false`, plus `dataset` sources the author declares in the app's settings.
  Otherwise `sources: []`. ADR 0018 says to flag weak lineage, and this is me flagging it.

### What the ADR would need to decide

1. Owner identity, capped at viewer/read, versus anything per-viewer.
2. The `workloadIdentity: {mint: true}` manifest change for this module.
3. Token delivery through the gate container, never stored in a Kubernetes object.
4. Which data paths v0 includes (database and lakehouse only, or more; see Q3).
5. Default-closed internet egress for app pods (Q4).
6. Lineage: declared sources only.

## Not decided here: raised for the coordinator

- **Q1 Code from the code catalog (ARCHITECTURE §7 item 6).** Not built and not designed. v0 apps
  are self-contained source stored in this module's database. Nothing here assumes or blocks a
  future `contracts/runnable-code.md`.
- **Q2 Sharing scope.** The kickoff says "any logged-in platform user the app is shared with". With
  (c), a viewer sees data read with the owner's workspace access. And the shell mints the iframe
  session for the viewer's *active* workspace, so a viewer outside the app's workspace has no
  natural way to open it at all. My recommendation: v0 shares only with members of the app's
  workspace (any role views; editors and owners edit their own apps). Cross-workspace sharing should
  wait for an explicit ruling. External/public sharing: none, per the kickoff. I have not designed
  any of it.
- **Q3 Storage and catalog files over HTTP.** Reading a catalog-registered file dataset from
  booth-storage needs a platform token in the app's code, or a read proxy in this backend. The brief's
  done-definition says "reads from a registered storage/catalog source". Database and lakehouse
  tables cover part of that. Is that enough for v0?
- **Q4 Packages and egress.** A fixed base image with no per-app `pip install` means no internet
  egress is needed. Notebooks defaults egress open and pipeline defaults it closed (ADR 0070 accepted
  both). I recommend closed for apps because of (c). Per-app requirements would need either egress
  or an in-cluster package mirror.
- **Q5 Credentials.** All of (c). Nothing in (c) gets built until there is an ADR.

## Build order once reviewed

1. Prove the URL/websocket path: a hand-started Streamlit pod behind a real core's iframe proxy.
2. Verify `X-Booth-Identity` in the backend, the app model and API, and the UI list/editor.
3. Lifecycle (Deployment/Service/ConfigMap, gate, limits, cap, idle shutdown, reconcile), with
   integration tests for start/stop/idle/cap and for "a forged `X-Booth-User` without the bearer is
   refused".
4. `dashboard.*` publishing on create/update/delete (ADR 0046), verified against booth-catalog.
5. Data access, only after the ADR for (c).

## As built: step 3 (lifecycle), 2026-10-08

Sections (a) and (b) are built as written, with these differences. Each one is deliberate, and
each is covered by a test.

- **Waking is not starting (ADR 0105).** (a) said a viewer opening a stopped app starts it. Since
  ADR 0105 only owners may start an app. So there are two kinds of "down":
  - **Stopped:** the owner chose Stop. It stays down for everyone else.
  - **Suspended:** idle shutdown. The owner's choice is still Running, and opening the app wakes
    it, within the cap, for anyone who may open it.
  The UI labels a suspended app "Sleeping".
- **The bearer lives in the database as well as the Secret.** The Role can create and delete
  Secrets but never read them, so the backend keeps its own copy of each app's bearer. It is
  generated when the app is created and never changes. The gate reads it from the Secret, which is
  mounted into the gate container only.
- **Owner references have `blockOwnerDeletion: false`.** Setting it true would need update on
  `deployments/finalizers` wherever the OwnerReferencesPermissionEnforcement admission plugin runs.
  The Role doesn't grant that. Garbage collection still removes an app's objects with its
  Deployment, and all apps with the backend's Deployment.
- **Reconcile polls** every 5 s, and runs immediately after any change through the API.
- **The Role is tighter than (a) listed, on purpose** (coordinator ruling on PR #5): exactly the
  calls the lifecycle makes, nothing more.
  - Deployments and ConfigMaps: create, update, delete, get, list.
  - Services: create, delete, list. A Service never changes once created.
  - Pods: list.
  - Secrets: create. A deleted app's Secret goes with its Deployment through its owner reference,
    removed by the garbage collector, not the backend.

  No `watch` anywhere, since the lifecycle polls. (a)'s "Backend RBAC" bullet is superseded by
  this. `TestChart_BackendRoleIsExactlyTheDesignNotes` pins it, and `verify.sh` checks it live in
  both directions.
- **Egress (ADR 0104 item 4)** is built as part of the app pods' NetworkPolicy:
  - `apps.egress.mode`: `open` (default) is internet only; `closed` is DNS only.
  - Every private, CGNAT and link-local range is excluded, which includes the metadata address.
  - Per-app `pip install` is not built yet.
- **A ResourceQuota on limits** means every pod in the namespace must declare limits. The backend
  and app pods do.
- **A change invalidates what was observed about an app, synchronously.** The lifecycle
  integration test found a real race: a viewer visiting two seconds after idle shutdown got a 502.
  The proxy had trusted a cached "running" from the reconcile that suspended the app, and forwarded
  to a pod already being removed. Now every start, stop, suspend, wake, edit and delete drops that
  app's cached state before the call returns. A reconcile that listed the apps before the change
  can't write its stale observation back (a per-app generation counter). Unit tests reproduce
  both halves, and fail with the fix removed.
