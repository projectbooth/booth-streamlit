# booth-streamlit

Project Booth's code-first dashboard/app module: a user writes a Streamlit app in Python and
runs it as a hosted app, reached through the shell via `iframe-proxy` (ADR 0016). One of three
modules in the "dashboards/apps" capability slot, alongside `booth-superset` and
`booth-metabase`.

Architecture, contracts and decisions live in `booth-architecture`; this repo's brief is
`agent-briefs/streamlit.md` there.

## Status

Built: the module backend (Go, chi) with its manifest, health checks, database and event-bus
connections; the app model and API (`/api/apps`); the UI to create, edit, share, start, stop and
delete apps; and the per-app lifecycle. Each app runs as its own Deployment (Streamlit on
loopback, behind a gate that requires the app's bearer). The cap on running apps, idle shutdown
and a reconcile loop come with it. The per-app proxy (`/apps/{id}/...`) verifies booth-core's
`X-Booth-Identity`, limits access to the app's workspace, and strips everything replayable before
user code sees a request.

Data access (ADR 0104/0107, `docs/design-data-access.md`), built in five steps; done so far:
**(a)** each app's workload token (owner = the app's owner, capped at viewer), the gate's token
refresh over the backend's internal port, the Postgres credential sidecar (`DATABASE_URL` in the
app), "Data access paused" and Take ownership; **(b)** the file read proxy for booth-storage objects
and catalog file datasets (`booth_streamlit.files`); **(c)** the lakehouse: an s3 credential sidecar
scoped to the workspace's warehouse, with `booth_streamlit.pyarrow_fs()` and `duckdb_secret()`
(`dataAccess.lakehouse.enabled`; the S3 keys it writes are readable by app code, as ADR 0107 accepts).
Off unless `dataAccess.enabled`. **(d)** per-app packages: an optional `requirements.txt`, installed on
every start by an init container that holds no token, bearer or credentials (`apps.pip`); DuckDB
is in the runtime image. Not built yet: (e) `dashboard.*` publishing. Design: `docs/design-v0.md`, including its
"as built" notes.

**Who may do what (ADR 0105, interim while ARCHITECTURE.md item 55 is open):** only owners of the
app's workspace may create, edit, start, stop or delete apps. Editors and viewers can open apps an
owner has shared with the workspace; opening one that idle shutdown put to sleep wakes it.
Nobody outside the workspace can see an app. The backend enforces this from the verified role.

## Layout

| Path | What |
|---|---|
| `cmd/streamlit` | Backend entrypoint. |
| `internal/api` | HTTP: `/livez`, `/healthz`, the per-app proxy, and the UI. |
| `internal/identity` | Verifies booth-core's `X-Booth-Identity` assertion (ADR 0069, 0041). |
| `internal/proxy` | The per-app proxy, including Streamlit's websocket, idle tracking and the websocket lifetime cap. |
| `internal/apps` | The app model and every authorization rule (ADR 0105), the running-app cap. |
| `internal/lifecycle` | One Deployment/Service/ConfigMap/Secret per app, reconcile loop, idle shutdown. |
| `internal/gate`, `cmd/gate` | The gate in front of Streamlit in every app pod (requires the app's bearer). |
| `images/app-runtime` | The per-app Streamlit image and the `booth_streamlit` helper. |
| `internal/config` | Environment-variable configuration, 1:1 with chart values. |
| `internal/db` | Connection to this module's own database (ADR 0053). |
| `internal/events` | Event-bus connection (ADR 0050); the `dashboard.*` publisher will live here. |
| `web/` | The module's UI, built into the image and served by the backend. |
| `charts/booth-streamlit` | Helm chart, including the `BoothModule` manifest (ADR 0019). |
| `test/contract` | Manifest and chart contract tests (`helm template`, no cluster). |
| `test/integration` | kind-based tests, against stand-ins and against a real booth-core. |

## Health

- `/livez`: process is up. Never looks at dependencies.
- `/healthz` (readiness, and the manifest's `healthCheckPath`): 503 if the database is
  unreachable; 200 `"degraded"` while the event bus is still connecting (apps keep working
  without it; only catalog indexing stalls); 200 `"ok"` otherwise.

## Tests

```sh
docker compose -f hack/docker-compose.emulators.yml up -d --wait
eval "$(sh hack/test-env.sh)"
go test ./...                  # unit + contract (contract tests need helm on PATH)
(cd web && npm ci && npm run typecheck && npm run lint && npm test -- --run)
```

Layer-3 tests: see `test/integration/README.md`.
