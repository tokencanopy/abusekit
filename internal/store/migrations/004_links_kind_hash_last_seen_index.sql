-- 004_links_kind_hash_last_seen_index.sql
--
-- R4: Neighbors' per-key query orders by last_seen DESC (design §4.2's
-- fan-in cap keeps the most recently active neighbors), which
-- 001_core.sql's (tenant, kind, hash) index doesn't cover — every call
-- against a hot link key forced a full sort of that key's rows. Expand-
-- only, per AGENTS.md's migration policy.

CREATE INDEX IF NOT EXISTS idx_links_kind_hash_last_seen ON links (tenant, kind, hash, last_seen DESC);
