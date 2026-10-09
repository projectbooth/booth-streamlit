-- Per-app Python packages (ADR 0107; docs/design-data-access.md item 5): an optional
-- requirements.txt, stored and delivered with the source. The app's pod installs it on every start.
ALTER TABLE apps ADD COLUMN requirements TEXT NOT NULL DEFAULT '';
