-- Lineage and dashboard events (ADR 0018/0046; docs/design-data-access.md item 6).
--
-- sources: catalog dataset ids the owner declared the app reads. Lineage only: they describe the
-- app and restrict nothing (ADR 0107 item 4).
ALTER TABLE apps ADD COLUMN sources TEXT[] NOT NULL DEFAULT '{}';

-- The outbox: every dashboard.* event, written in the same transaction as the change it describes
-- and drained to the bus afterwards (internal/events), at least once. created_at is the database
-- clock at the change and is the envelope's publishedAt. A row that keeps failing is marked failed
-- after a bounded number of attempts and left for an operator to see (/healthz, logs).
CREATE TABLE app_events_outbox (
    id              BIGSERIAL   PRIMARY KEY,
    workspace       TEXT        NOT NULL,
    app_id          TEXT        NOT NULL,
    event_type      TEXT        NOT NULL,
    data            JSONB       NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    published_at    TIMESTAMPTZ,
    attempts        INT         NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    last_error      TEXT        NOT NULL DEFAULT '',
    failed          BOOLEAN     NOT NULL DEFAULT FALSE
);
CREATE INDEX app_events_outbox_due ON app_events_outbox (next_attempt_at, id) WHERE published_at IS NULL AND NOT failed;
CREATE INDEX app_events_outbox_app ON app_events_outbox (workspace, app_id, created_at);
