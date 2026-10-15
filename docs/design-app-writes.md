# booth-streamlit app writes and uploads (ADR 0117 points 2 and 6): design note for review

Status: proposal 2026-10-14, reconciled by the coordinator in ADR 0117 ("Reconciled design",
items 1, 1a, 2 and 7); phase 1 built 2026-10-15, see "As built" at the end. Where the two differ,
"As built" and the ADR win. It follows the read path in `docs/design-data-access.md`
(the file proxy, item 4), which is built and tested. Checked against the code on `main` (`4b94b0c`)
and booth-storage `main` (`7e4b555`).

## 1. The write proxy

**Recommendation:** the write proxy is a copy of the file read proxy, with one new rule: the
backend refuses anything that isn't a declared target before it calls booth-api.

**Path of a write:**
1. App code calls the gate's existing loopback listener, `127.0.0.1:8090/writes/{apiId}/{datasetId}`
   (`BOOTH_WRITES_URL`).
2. The gate strips any `Authorization` header, adds the app's bearer, and forwards to the backend's
   internal port `:8081/writes/...`. This is exactly how `/files/` works (`internal/gate/loopback.go`).
3. The backend resolves the bearer to the app and its workspace. It refuses the request unless
   `(apiId, datasetId, operation)` is one of the app's confirmed write targets.
4. It then calls booth-api **through core's gateway**, with the app's workload token
   (`streamlit:<ws>:<appId>`, viewer cap, unchanged) and `X-Workspace`. It passes `Idempotency-Key`
   through.
5. booth-api makes the real check against the declaration's record (item 2) and writes with its
   own narrow lease (ADR 0117 point 4).

**Operations:** insert, update by key, and delete by key. The backend forwards only the operations
the target allows, so booth-api's own request and response shape passes through unchanged.

**Limits:**
- 1 MiB body, a 30 s timeout, at most 4 concurrent writes per app.
- No body logged; the backend logs the app, the target and the status.

**Refusals:**
- 405 for other methods.
- 403 `not_a_write_target`.
- 403 `data_access_paused` when the owner has lost access, as reads do.
- 401 for an unknown bearer.

**NetworkPolicy is unchanged.** App pods already reach only DNS, their sidecars' backends and the
backend's 8081. The backend reaches core's gateway as it does for reads. App code never holds a key,
a token or the bearer, and the scanner test (design-data-access item 2) still applies.

**Helper**, `booth_streamlit.api`:

```python
api.insert(api_id, dataset_id, row, *, entered_by="entered_by", idempotency_key=None) -> dict
api.update(api_id, dataset_id, key, changes, *, entered_by="entered_by", idempotency_key=None) -> dict
api.delete(api_id, dataset_id, key, *, idempotency_key=None) -> None
```

- `entered_by` names a column. The helper sets it to `user().subject` unless the row already has
  it. `None` turns this off.
- With no signed-in viewer (`user()` is `None`), a write raises `NotSignedIn` rather than writing
  an anonymous row.
- One idempotency key (uuid4) is generated per call, so the helper's own retries reuse it.
- Errors raise:
  - `WriteRefused(status, reason)`;
  - `InvalidRow(fields)`, carrying booth-api's per-field errors;
  - `DataAccessPaused`.

**Honest limit:** the gate verifies the viewer on the iframe request, but it can't tell which
viewer made a loopback call. So `entered_by` is **stated by the app's code**, as ADR 0117 point 3
accepts. booth-api's log records the app as the writer.

## 2. App settings: "Write targets"

**In the editor:** a section next to "Sources", owner-only.
- The owner adds a target by picking an API, then one of its write-enabled datasets. The list is
  read from booth-api **as that owner**.
- The editor shows the operations and writable columns booth-api allows. The owner can narrow the
  operations, never widen them.
- Upload targets (item 3) sit in the same section.

**Confirmation**, a modal like sharing:

> "Anyone who can open this app, including viewers, can **add and change rows** in *Household
> transactions* through *Finance API*. The change is made as the app, not as them. Who entered it
> is recorded only if the app's code records it."

The owner must tick the box before saving. Removing a target needs no confirmation.

**Header disclosure:** a strip above the iframe, rendered by the module UI and never by app code,
so an app can't hide it.
- Text: "This app can write to: Household transactions (add, change) · upload to
  local:finance/inbox/".
- Every viewer sees it on every visit, and it can't be dismissed.

**Ownership handover:** take-ownership clears every write and upload target in the same
transaction that changes the owner. Writes are then refused until the new owner re-declares and
confirms.

**Where the declaration lives — recommended: core.**
- The backend registers the confirmed targets with core using **the owner's own token**, so core
  can check that the writer is an owner of the workspace at that moment.
- Core keeps the record with the app's subject and the declaring owner. It voids the record if the
  app's owner changes.
- booth-api (writes) and booth-storage (uploads) check against that one record.
- booth-streamlit keeps a copy for the UI and the early refusal, but its copy is never the
  authority.
- This fits candidate B in the booth-core prompt, or A with the record kept by core. I can work
  with either, as long as one module holds the record for both writes and uploads. The coordinator
  reconciles this with booth-api's and booth-core's notes.

## 3. Uploads: create-only

**What an owner declares, per upload target:**
- a Storage backend and a folder prefix, such as `local` and `finance/inbox/`;
- a size cap: default 25 MiB, never more than the chart value `dataAccess.uploads.maxBytes`;
- an extension allowlist, such as `csv`, `ofx`, `qfx`, `pdf`.

**Path:** loopback `POST /files/upload/{targetId}?name=<file name>`, raw body. The backend:
- requires a declared target;
- requires the name to be one segment: no `/`, `\`, `..`, NUL, `%2f`, or leading dot;
- accepts only allowlisted extensions (case-insensitive), else 415;
- enforces the cap while streaming, else 413;
- rejects every other method with 405.

**The stored object** is `{prefix}{UTC timestamp}-{6 random chars}-{name}`, so two members
uploading `statement.csv` don't collide. The helper returns the stored path.

**No overwrite and no delete:**
- the upload proxy has no list, read, delete or overwrite route;
- an existing name answers 409.

**The Storage call:** `PUT /api/backends/{id}/objects/{path}` with `If-None-Match: *`, through
core's gateway, with the app's workload token and `X-Workspace`.

**What booth-storage needs**, checked against its `main`: today that PUT overwrites, and it requires
an editor or owner (`auth.CanWrite`, `server.go:94`), so it refuses the app's viewer-capped token.
1. **Create-only writes.** `If-None-Match: *` must answer 412 if the object exists, atomically for
   each backend kind:
   - filesystem: `O_EXCL`;
   - S3: conditional PutObject;
   - Azure: `If-None-Match`;
   - GCS: `ifGenerationMatch=0`.

   Where a kind can't do it atomically, it should answer 501 rather than check-then-write.
2. **A narrow admission rule.** A create-only PUT from an app's workload token is allowed when
   core's record grants that app this backend and prefix. A plain PUT, a DELETE or a folder delete
   from such a token stays 403, as today.

**Helper:** `booth_streamlit.files.upload(target_id, name, data: bytes | file) -> str`, where the
string is the stored path. It raises `UploadRefused(status, reason)`.

## 4. What a viewer-role member sees and can do

**A member who isn't an owner:**
- sees a shared app and its header disclosure;
- can use its forms and uploads, which write as the app (ADR 0117 point 3);
- can't see or change settings, and can't declare targets. Only owners author apps (ADR 0105).

An editor is treated the same way. A member of another workspace still gets 404.

**A viewer has no direct write anywhere:** the proxy is reachable only from the app's own pod,
and the broker still refuses a viewer `readwrite`.

## 5. Per-app egress closure

**This is possible with no Role change.**
- Split the chart's app policy in two:
  - a base policy for every app pod: DNS, sidecar backends, the backend's 8081 and the chart's
    rules;
  - a second policy, selecting `booth.projectbooth.io/egress: open`, that adds only the internet
    `ipBlock`.
- NetworkPolicies add up, so a pod without the label gets no internet.
- The backend already writes the pod template, so it sets the label per app. Changing it rolls the
  pod.
- With the release's `apps.egress.mode=closed`, the second policy isn't rendered, so the release
  setting still wins.

**Recommendation:** an app with any write or upload target is **forced closed**, and the editor
says so. Such an app can then install packages only from an in-cluster index (`apps.pip.egress`),
with pip's existing "egress is closed" message otherwise. ADR 0117 calls this hardening, so it's
flagged as open question 1.

## 6. Tests

**Unit:**
- the gate forwards `/writes` and `/files/upload` with the bearer, and drops a caller's
  `Authorization`;
- the backend refuses an undeclared target or operation, oversize bodies, bad names, extensions
  and methods;
- take-ownership clears targets, in both stores;
- the helper fills `entered_by`, reuses the idempotency key, maps errors and raises `NotSignedIn`;
- the header strip renders for a viewer.

**Contract:**
- the base app policy has no internet rule;
- the open policy selects the label;
- `closed` renders no open policy.

**Real-core:** these need booth-api writes, booth-database's write role, core's record and
booth-storage's create-only PUT.
- **Writes and uploads:**
  - A viewer opens a shared app and inserts a row; the row lands with `entered_by` set to the
    viewer's subject.
  - Without the proxy's own check (a test build), booth-api still refuses an undeclared dataset.
    So does a target the app declares that core never received.
  - A column that isn't writable is refused with booth-api's field error.
  - Take-ownership blocks writes until the new owner re-declares.
  - An upload works once; the same stored name answers 409. Outside the prefix gives 403, a bad
    extension 415, too large 413, and DELETE 405.
- **Egress and isolation:**
  - A writing app's pod drops `1.1.1.1:443` (a timeout), while a non-writing app connects (the
    `egress.sh` pattern).
  - The scanner finds no token or bearer.
  - Chromium shows the header strip to a viewer.

## What I need from other modules

- **booth-api:**
  - write endpoints through core's gateway that accept an app's workload token;
  - a check of the token's subject against core's record;
  - an owner-readable list of write-enabled APIs, datasets, operations and columns;
  - support for idempotency keys;
  - the per-field error shape;
  - a log of the app as the writer.
- **booth-core:**
  - an app write record: written with an owner's token, voided on an owner change, and readable by
    booth-api and booth-storage;
  - workload tokens unchanged;
  - gateway routes to booth-api for workload tokens.
- **booth-storage:**
  - create-only `If-None-Match: *` on every backend kind;
  - the app-token admission rule from item 3.

## Open questions for the user

1. Force egress closed for apps that write (no PyPI packages without a mirror), or only recommend
   it?
2. Should delete be allowed through an app when the API enables it, or never for apps?
3. Uploaded names: timestamp-prefixed (recommended), or kept as given with a 409 on a clash?
4. ADR 0117 says "no listing outside the prefix". The upload proxy lists nothing, but the existing
   read proxy can list a whole backend in the app's workspace (viewer read, ADR 0104). Keep that,
   or narrow it?

Separately, a known issue: the runtime image's `psycopg[binary]==3.3.6` crashes the app container
(SIGSEGV, exit 139) when it decodes a `timestamptz` column. The workaround is to format timestamps
in SQL (`to_char`) and select explicit columns. The fix is tracked from `agent-briefs/streamlit.md`.

## As built: phase 1 (writes), 2026-10-15

Built against booth-api's contract (`docs/contracts/app-writes.md` on its `app-writes` branch) and
ADR 0117 as amended. What changed from this note:

- **The declaration isn't registered with core.** booth-api holds the grant; this module keeps the
  app-side declaration (`apps.write_targets`: API slug and operations) for the header, the early
  refusal and the egress label. A write needs both.
- **The owner grants on booth-api's page, not here** (item 1a): this module never holds a person's
  token. The Write targets panel links to `/apis?writeApi=<slug>&writePrincipal=streamlit:<workspace>:<appId>`
  in the top window; booth-api reading those parameters is booth-api's work. A `not_granted`
  answer is rewritten into plain words naming the API page and the principal.
- **Targets are booth-api API slugs**, one table each, not (API, dataset) pairs. The panel takes
  the slug as typed: it can't list booth-api's APIs, whose management routes accept only a person's
  token.
- **Operations:** insert by default; update and delete opt-in, delete unticked. booth-api's own
  settings and the grant can narrow further; this module can't read them, so it can't hide an
  operation the API doesn't allow.
- **Write proxy:** `/writes/{api}/rows[/{key}]` and `?key[field]=value` on the gate's loopback,
  forwarded to `/modules/api/app/v1/{api}/...` with the app's existing token, `X-Workspace`,
  `Idempotency-Key`, `If-Match` and `X-Booth-App-User`. One row, 64 KiB (booth-api's own limit),
  30 s, 4 at once per app. GraphQL isn't forwarded (see operations.md).
- **Helper:** `booth_streamlit.api.insert(api, row)`, `update(api, key, changes,
  expected_version=)`, `delete(api, key)`, returning `Written(row, version, replayed)`. One
  idempotency key per call, reused by its retries (429, 503, `idempotency_in_progress`, connection
  errors; three attempts). `BOOTH_WRITES_URL` is set only for an app with targets.
- **Take ownership:** clears the declaration, then calls booth-api's `revoke-principal` with a
  token minted for `streamlit:<workspace>:grants` (viewer cap, owner = the owner taking over), then
  changes the owner, which clears the declaration again in the same transaction. A failed revoke is
  503 and the owner doesn't change. With `dataAccess.writes.enabled` off there is no revoke.
- **Header:** an app with targets opens at `#app=<id>` on the module's own page: the header, then
  the app in a frame. An app without targets opens directly, as before. Any other page load of an
  app with targets (top-level, or a frame whose referrer isn't the module page or the app itself)
  gets 303 to that page, so the header shows on every visit (coordinator review of PR #24).
- **Egress:** as item 5, with the `booth.projectbooth.io/egress` label always set (`open` or
  `closed`) and pip told its egress is closed unless `apps.pip.egress` names an index.

**Tests:** unit (store against Postgres and memory, service, write proxy, revoker, gate, lifecycle,
API, config, chart contract, Python helper, UI) and real-core (`test/integration/writes-access.sh`,
against core's real gateway and a booth-api stand-in until booth-api's change lands).

**Phase 2 (uploads, item 3)** waits for review of phase 1; booth-storage's upload-grant contract is
on its PR #7.
