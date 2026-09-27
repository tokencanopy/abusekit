-- 002_links_tenant_subject_index.sql
--
-- S13: Neighbors' reverse lookup ("every link key this subject has")
-- filters on (tenant, subject), which 001_core.sql's only links index
-- (tenant, kind, hash) does not cover — that query fell back to a
-- sequential scan of the whole links table. Expand-only, per AGENTS.md's
-- migration policy.

CREATE INDEX IF NOT EXISTS idx_links_tenant_subject ON links (tenant, subject);
