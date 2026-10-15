-- App writes (ADR 0117 and its "Reconciled design" items 1a and 7; docs/design-app-writes.md).
--
-- write_targets: the app-side declaration, a JSON array of {"api": "<booth-api slug>",
-- "operations": ["insert", ...]}. booth-api holds the grant itself; a write needs both. Kept here
-- for the header every viewer sees, the early refusal in the write proxy, and the per-app egress
-- closure. Cleared on take-ownership.
--
-- write_targets_by / write_targets_at: the owner who last confirmed the declaration, and when.
ALTER TABLE apps ADD COLUMN write_targets    JSONB       NOT NULL DEFAULT '[]';
ALTER TABLE apps ADD COLUMN write_targets_by TEXT        NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN write_targets_at TIMESTAMPTZ;
