-- 007_budget_usage.sql
--
-- S7 (S2 fix round): persists internal/worker.Budgets' daily call counters
-- so a process restart or a second worker instance doesn't silently reset
-- or duplicate a cap — an in-memory-only counter (S2's original
-- implementation) gives every new process its own zeroed budget, which
-- defeats the whole point of a shared daily limit the moment there's more
-- than one worker instance.
--
-- One row per (day, dim, key): dim is "adapter" | "subject" | "tenant"
-- (internal/worker.Budgets' three enforcement dimensions — see its own
-- doc comment on why "producer" from design §4.8 became "tenant" here);
-- key is the adapter name, "tenant/subject", or the tenant name
-- respectively. A day rolling over simply starts a new set of rows; old
-- ones are harmless and small (see internal/worker's own retention note —
-- v0 does not prune this table, matching how rule_state/verdicts are
-- retained too).
--
-- Expand-only, per AGENTS.md's migration policy.

CREATE TABLE IF NOT EXISTS budget_usage (
    day        DATE NOT NULL,
    dim        TEXT NOT NULL,
    key        TEXT NOT NULL,
    count      INT NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (day, dim, key)
);
