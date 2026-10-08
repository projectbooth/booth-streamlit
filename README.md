# booth-streamlit

Project Booth's code-first dashboard/app module: a user writes a Streamlit app in Python and
runs it as a hosted app, reached through the shell via `iframe-proxy` (ADR 0016). One of three
modules in the "dashboards/apps" capability slot, alongside `booth-superset` and
`booth-metabase`.

Architecture, contracts and decisions live in `booth-architecture`; this repo's brief is
`agent-briefs/streamlit.md` there.

## Status: scaffold

What exists: the module backend (Go, chi), its manifest, health checks, database connection and
event-bus connection; a placeholder UI (React, TypeScript, Vite, Tailwind) served by the backend
inside the shell's iframe; the per-app proxy (`/apps/{id}/…`), which verifies booth-core's
`X-Booth-Identity`, limits access to the app's own workspace, and strips everything replayable
before user code sees the request; the Streamlit runtime image (`images/app-runtime`); the Helm
chart; CI. The app model and its API (`/api/apps`) with the UI to create, edit, share, start, stop
and delete apps. What does not exist yet: per-app container lifecycle (Start records the desired
state; nothing runs an app's container yet, so opening one says it is not running), and
`dashboard.*` publishing.

**Who may do what (ADR 0105, interim while ARCHITECTURE.md item 55 is open):** only owners of the
app's workspace may create, edit, start, stop or delete apps. Editors and viewers can open apps an
owner has shared with the workspace. Nobody outside the workspace can see an app. The backend
enforces this from the verified role; the UI only hides what would be refused. Streamlit runs only inside the per-app
containers, never in the backend. Design: `docs/design-v0.md`; data access: ADR 0104 (not built).

## Layout

| Path | What |
|---|---|
| `cmd/streamlit` | Backend entrypoint. |
| `internal/api` | HTTP: `/livez`, `/healthz`, the per-app proxy, and the UI. |
| `internal/identity` | Verifies booth-core's `X-Booth-Identity` assertion (ADR 0069, 0041). |
| `internal/proxy` | The per-app proxy, including Streamlit's websocket. |
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
