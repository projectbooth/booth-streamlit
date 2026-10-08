# Integration tests (layer 3)

`contracts/testing-strategy.md` layer 3: a real kind cluster, run by `.github/workflows/integration.yml`
on merge to `main`, nightly, and by hand.

| Job | Deploys | Covers |
|---|---|---|
| `standins` | booth-core's BoothModule CRD (vendored, `fixtures/boothmodule-crd.yaml`), a throwaway PostgreSQL and an unauthenticated NATS, then this chart with its own database Secret and the bus URL given by hand | The chart installs; core's real CRD schema keeps every manifest field (nothing pruned); the backend reaches Postgres and NATS; `/healthz`; the UI is served with relative asset URLs; the service account has no Kubernetes API access. |
| `real-core` | A real Keycloak, a real booth-core built at a pinned commit (`CORE_REF`), this chart with its defaults, and a hand-started Streamlit demo app (`fixtures/demo-app.yaml`) | Everything above, plus what only core can do: it provisions `booth-database-credentials` from `database: {enabled: true}` (ADR 0053), mints `booth-event-bus-credentials` from `events: {publish: [dashboard.*]}` (ADR 0050), the backend connects to core's authenticated bus with that credential, and core's health reconciler marks the module `Healthy`. Then `iframe-path.sh`: a real Chromium, with real Keycloak tokens and core's real iframe URLs, loads the app inside an iframe on core's origin through core's iframe proxy and this module's proxy. It checks the app renders over Streamlit's websocket, a click round-trips, the app sees the viewer's verified identity, neither `X-Booth-Identity` nor core's session cookie reaches user code, and a member of another workspace gets 404; then the module's own page, through core, as that viewer: trust note shown, no authoring controls. Then `api-authz.sh` (ADR 0105): with real Keycloak users and core's real session, an owner of the workspace can create, edit, start, stop and delete an app; an editor and a viewer get 403 on every write path (and can read the shared app); an owner of another workspace gets 404 on every path to it; the app is unchanged after the refused writes. |

## Not covered yet

- Publishing a `dashboard.*` event and booth-catalog indexing it: there is no publisher until the app model exists.
- Per-app containers, limits, idle shutdown: not built.

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
