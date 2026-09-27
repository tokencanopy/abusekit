-- 005_subjects_class_set_at.sql
--
-- R11 (round 2): tracks the `at` of the subject.class event that most
-- recently set subjects.class, so a later-delivered but chronologically
-- OLDER classification (a retried or reordered delivery) can never
-- overwrite a newer one. Expand-only, per AGENTS.md's migration policy;
-- existing rows default to NULL (unknown "as of" time — the very next
-- subject.class event for that subject always wins, which is the
-- correct, safe default for rows that predate this column).

ALTER TABLE subjects ADD COLUMN IF NOT EXISTS class_set_at TIMESTAMPTZ;
