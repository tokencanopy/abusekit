-- 006_subjects_lease_and_failure_backoff.sql
--
-- B3 (S2 fix round): a per-subject claim lease and whole-pass failure
-- backoff, on top of design §4.8's dirty_seq/scored_seq queue.
--
-- claimed_until protects the window between ClaimDirtySubjects claiming a
-- subject and the worker committing (or failing) its scoring round: while
-- set and in the future, no instance's ClaimDirtySubjects call will select
-- this row again, closing the double-claim/double-scoring race a bare
-- FOR UPDATE SKIP LOCKED transaction that commits before scoring begins
-- cannot close (proven: two instances issued 40 scorer calls for 20
-- subjects). It is cleared as soon as a scoring pass concludes, success or
-- failure, rather than held for its full duration.
--
-- fail_count/next_attempt_at back off a subject whose scoring pass fails
-- OUTRIGHT (EventsForSubject, feature.Extract, or a store call erroring —
-- as opposed to one rule's scorer erroring, which internal/worker already
-- handles via rule_state without failing the whole round): proven, a
-- subject whose scoring always errors was reclaimed and retried every
-- single tick forever, starving the rest of the batch of worker capacity.
-- Success (or losing the compare-and-clear race to a newer round) clears
-- both.
--
-- Expand-only, per AGENTS.md's migration policy.

ALTER TABLE subjects ADD COLUMN IF NOT EXISTS claimed_until TIMESTAMPTZ;
ALTER TABLE subjects ADD COLUMN IF NOT EXISTS fail_count INT NOT NULL DEFAULT 0;
ALTER TABLE subjects ADD COLUMN IF NOT EXISTS next_attempt_at TIMESTAMPTZ;

-- S4 (S2 fix round): the claim query's two selection arms (dirty_seq >
-- scored_seq; next_rescore_at due) are run as a UNION of two independently
-- indexed queries rather than one OR'd WHERE clause, since Postgres can
-- rarely use an index efficiently across an OR of two different columns.
-- These are PARTIAL indexes — only rows that could ever match are indexed
-- at all — so a subject that's already caught up (dirty_seq = scored_seq,
-- no rescore pending) costs nothing here.
CREATE INDEX IF NOT EXISTS idx_subjects_dirty
    ON subjects (tenant, subject)
    WHERE dirty_seq > scored_seq;

CREATE INDEX IF NOT EXISTS idx_subjects_next_rescore_at
    ON subjects (next_rescore_at)
    WHERE next_rescore_at IS NOT NULL;

-- S4: NeighborOutcomes' subject.deleted lookup filters on (tenant, type),
-- a condition 001_core.sql's events indexes (subject-scoped) don't cover,
-- forcing a sequential scan of the whole events table on every
-- linked_deleted_n computation.
CREATE INDEX IF NOT EXISTS idx_events_tenant_type
    ON events (tenant, type, subject)
    WHERE type = 'subject.deleted';
