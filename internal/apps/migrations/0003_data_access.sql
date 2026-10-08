-- Data access (ADR 0104/0107, docs/design-data-access.md).
--
-- owner: whose read access the app uses. It starts as the creator and changes only through
-- "take ownership" (ADR 0107 item 7), which any current workspace owner may do when the owner has
-- lost access.
--
-- data_paused_reason / data_paused_at: set when core refuses to mint for the owner (no role in the
-- workspace any more, or not seen within BOOTH_WORKLOAD_OWNER_MAX_AGE). The app still runs; its data
-- doesn't. Cleared by a successful mint.
--
-- data_epoch: bumped to roll the app's pod (it is on the pod template), which is how a refused
-- renewal ends Postgres connections opened under the old lease (ADR 0107 item 6).
ALTER TABLE apps ADD COLUMN owner              TEXT        NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN data_paused_reason TEXT        NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN data_paused_at     TIMESTAMPTZ;
ALTER TABLE apps ADD COLUMN data_epoch         INT         NOT NULL DEFAULT 0;
UPDATE apps SET owner = created_by WHERE owner = '';

-- The gate presents its bearer; the backend finds the app by it.
CREATE UNIQUE INDEX apps_gate_bearer ON apps (gate_bearer);

-- Every take-over, kept (ADR 0107 item 7: who, which app, the previous owner).
CREATE TABLE app_ownership_changes (
    app_id         TEXT        NOT NULL,
    workspace      TEXT        NOT NULL,
    previous_owner TEXT        NOT NULL,
    new_owner      TEXT        NOT NULL,
    reason         TEXT        NOT NULL,
    at             TIMESTAMPTZ NOT NULL
);
CREATE INDEX app_ownership_changes_app ON app_ownership_changes (app_id, at);
