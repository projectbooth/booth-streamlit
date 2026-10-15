-- App uploads (ADR 0117 items 1a, 6 and 7; docs/design-app-writes.md item 3).
--
-- upload_targets: the app-side upload declaration, a JSON array of {"backend": "<booth-storage
-- backend id>", "prefix": "<folder>/"}. booth-storage holds the grant itself (extensions, size cap);
-- an upload needs both. Kept here for the header, the upload proxy's early refusal and the per-app
-- egress closure. Cleared on take-ownership. write_targets_by / write_targets_at cover both kinds:
-- the owner confirms one declaration.
ALTER TABLE apps ADD COLUMN upload_targets JSONB NOT NULL DEFAULT '[]';
