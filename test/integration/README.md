# Integration tests (layer 3)

`contracts/testing-strategy.md` layer 3: a real kind cluster, run by `.github/workflows/integration.yml`
on merge to `main`, nightly, and by hand.

| Job | Deploys | Covers |
|---|---|---|
| `standins` | booth-core's BoothModule CRD (vendored, `fixtures/boothmodule-crd.yaml`), a throwaway PostgreSQL and an unauthenticated NATS, then this chart with its own database Secret and the bus URL given by hand | The chart installs; core's real CRD schema keeps every manifest field (nothing pruned); the backend reaches Postgres and NATS; `/healthz`; the UI is served with relative asset URLs; the service account has no Kubernetes API access. |
| `real-core` | A real booth-core built at a pinned commit (`CORE_REF`), then this chart with its defaults | Everything above, plus what only core can do: it provisions `booth-database-credentials` from `database: {enabled: true}` (ADR 0053), mints `booth-event-bus-credentials` from `events: {publish: [dashboard.*]}` (ADR 0050), the backend connects to core's authenticated bus with that credential, and core's health reconciler marks the module `Healthy`. |

## Not covered yet

- Publishing a `dashboard.*` event and booth-catalog indexing it: there is no publisher until the app model exists.
- `X-Booth-Identity` verification through core's iframe proxy: needs an identity provider in the cluster (booth-logging's `test/integration/realcore` shows the Keycloak setup to reuse) and the module's own verification code, neither of which exists yet.
- Per-app containers, limits, idle shutdown: not built.

## Running locally

```sh
kind create cluster --name booth-streamlit-ci
docker build -t booth-streamlit:ci . && kind load docker-image booth-streamlit:ci --name booth-streamlit-ci
test/integration/deploy-standins.sh booth-streamlit:ci
test/integration/verify.sh
```

For `real-core`, check out booth-core at `CORE_REF`, build and load its image the same way, and run
`deploy-realcore.sh <core checkout> booth-core:ci booth-streamlit:ci` then `REAL_CORE=1 verify.sh`
on a fresh cluster.
