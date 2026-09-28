-- 008_subjects_erased_at.sql
--
-- S3: DELETE /v1/subjects/{subject} (design §4.4's legal-erasure request)
-- needs a durable marker so a repeat erasure call is a cheap idempotent
-- no-op (design: "Idempotent" is called out explicitly for evaluate; the
-- same property is desirable here — an operator's retried DELETE, or a
-- client's retry after a dropped response, must not error just because
-- the first call already ran) rather than re-running the purge/tombstone
-- logic — and so the erasure PATH taken (full purge vs. abusive-labelled
-- tombstone, design §4.4) is recorded for audit even after the subject's
-- own text has been destroyed.
--
-- NULL means "never erased" (the overwhelming common case). Expand-only,
-- per AGENTS.md's migration policy.

ALTER TABLE subjects ADD COLUMN IF NOT EXISTS erased_at TIMESTAMPTZ;
ALTER TABLE subjects ADD COLUMN IF NOT EXISTS erasure_mode TEXT NOT NULL DEFAULT '';
