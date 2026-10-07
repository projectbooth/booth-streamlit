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
inside the shell's iframe; the Helm chart; CI. What does not exist yet: apps. No app model, no
per-app containers, no `dashboard.*` publishing, no identity verification (nothing served today
reads user data). Streamlit runs only inside the per-app containers, never in the backend.

## Layout

| Path | What |
|---|---|
| `cmd/streamlit` | Backend entrypoint. |
| `internal/api` | HTTP: `/livez`, `/healthz`, and the UI. |
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
