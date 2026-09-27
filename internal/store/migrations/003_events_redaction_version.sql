-- 003_events_redaction_version.sql
--
-- N6: records which version of internal/event's static redaction schema
-- (event.RedactionSchemaVersion) produced a given row's `data`, so a
-- future schema change can identify rows redacted under an older rule
-- set without guessing from `received_at`. Expand-only, per AGENTS.md's
-- migration policy: existing rows default to 1 (the only version that
-- has ever existed), which is correct since this column is being added
-- retroactively for the first time.

ALTER TABLE events ADD COLUMN IF NOT EXISTS redaction_version INT NOT NULL DEFAULT 1;
