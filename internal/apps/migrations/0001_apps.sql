-- One row per app (design note (a): the database is the source of truth; the lifecycle in step 3
-- converges containers to desired_state).
CREATE TABLE apps (
    id            TEXT        NOT NULL PRIMARY KEY,
    workspace     TEXT        NOT NULL,
    name          TEXT        NOT NULL,
    description   TEXT        NOT NULL DEFAULT '',
    source        TEXT        NOT NULL,
    -- ADR 0104 item 5 / ADR 0105: false = workspace owners only; true = every member of the
    -- app's workspace may open it. Never visible outside its workspace.
    shared        BOOLEAN     NOT NULL DEFAULT FALSE,
    desired_state TEXT        NOT NULL DEFAULT 'stopped' CHECK (desired_state IN ('stopped', 'running')),
    created_by    TEXT        NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL,
    updated_by    TEXT        NOT NULL,
    updated_at    TIMESTAMPTZ NOT NULL
);

CREATE INDEX apps_workspace_name ON apps (workspace, lower(name));
