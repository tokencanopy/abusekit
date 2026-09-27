-- 001_core.sql
--
-- abusekit's S1 schema (design §4.11): events, links, subjects, verdicts,
-- rule_state, labels, corpus_examples, calibrations.
--
-- Expand-only and idempotent (IF NOT EXISTS everywhere), per design's
-- migration policy and AGENTS.md — a later migration may add columns or
-- tables, never drop or rename what's here.
--
-- Tracked in its own schema_migrations_abusekit table (mirroring the
-- e2a-ops billing sidecar's pattern) so abusekit's migration runner never
-- collides with a host product's own tracker when both share a Postgres
-- instance in the future.

CREATE TABLE IF NOT EXISTS schema_migrations_abusekit (
    version    TEXT PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- events: append-only. (tenant, producer, id) is the idempotency key
-- design §4.2/§4.3 requires; body_hash is compared against a replay's
-- freshly computed hash to tell `duplicate` (identical body) from
-- `conflict` (same id, different body) — see internal/store's
-- AppendEvents and internal/event.Event.BodyHash.
CREATE TABLE IF NOT EXISTS events (
    seq         BIGSERIAL PRIMARY KEY,
    tenant      TEXT NOT NULL,
    producer    TEXT NOT NULL,
    id          TEXT NOT NULL,
    subject     TEXT NOT NULL,
    type        TEXT NOT NULL,
    at          TIMESTAMPTZ NOT NULL,
    links       JSONB NOT NULL DEFAULT '{}',
    data        JSONB NOT NULL DEFAULT '{}',
    body_hash   TEXT NOT NULL,
    received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant, producer, id)
);
CREATE INDEX IF NOT EXISTS idx_events_subject_at ON events (tenant, subject, at);

-- links: the keyed-hash identity graph (design §4.2). One row per
-- (tenant, kind, hash, subject) — a subject can appear under the same
-- link key more than once over time (first_seen/last_seen tracks that),
-- but never twice as separate rows.
CREATE TABLE IF NOT EXISTS links (
    tenant     TEXT NOT NULL,
    kind       TEXT NOT NULL,
    hash       TEXT NOT NULL,
    subject    TEXT NOT NULL,
    first_seen TIMESTAMPTZ NOT NULL,
    last_seen  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant, kind, hash, subject)
);
CREATE INDEX IF NOT EXISTS idx_links_kind_hash ON links (tenant, kind, hash);

-- subjects: one row per (tenant, subject). dirty_seq/scored_seq drive the
-- S2 worker's queue (design §4.8: "the worker selects dirty_seq >
-- scored_seq OR next_rescore_at <= now"); current_* columns are the
-- materialized read path for GET /v1/subjects/{subject} (S3) and the list
-- endpoint (v1, out of S1 scope).
CREATE TABLE IF NOT EXISTS subjects (
    tenant             TEXT NOT NULL,
    subject            TEXT NOT NULL,
    class              TEXT NOT NULL DEFAULT 'customer',
    dirty_seq          BIGINT NOT NULL DEFAULT 0,
    scored_seq         BIGINT NOT NULL DEFAULT 0,
    next_rescore_at    TIMESTAMPTZ,
    current_tier       TEXT NOT NULL DEFAULT 'unknown',
    current_score      DOUBLE PRECISION,
    current_verdict_id BIGINT,
    current_scored_at  TIMESTAMPTZ,
    first_seen_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_event_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant, subject)
);
CREATE INDEX IF NOT EXISTS idx_subjects_tier_scored_at ON subjects (tenant, current_tier, current_scored_at, subject);

-- verdicts: append-only scoring history, one row per (subject, rule) per
-- scoring round. No FK to subjects.current_verdict_id (subjects is
-- created first, before any verdict exists) — the relationship is
-- maintained by application code in UpsertVerdicts, not by the schema.
CREATE TABLE IF NOT EXISTS verdicts (
    id          BIGSERIAL PRIMARY KEY,
    tenant      TEXT NOT NULL,
    subject     TEXT NOT NULL,
    rule        TEXT NOT NULL,
    mode        TEXT NOT NULL,
    scorer      TEXT NOT NULL,
    model       TEXT NOT NULL,
    checkpoint  TEXT NOT NULL DEFAULT '',
    render      TEXT NOT NULL DEFAULT '',
    calibration TEXT NOT NULL DEFAULT '',
    probs       JSONB NOT NULL DEFAULT '{}',
    risk        DOUBLE PRECISION,
    flagged     BOOLEAN NOT NULL DEFAULT false,
    reason      TEXT NOT NULL DEFAULT '',
    llm_reason  TEXT,
    input_hash  TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL DEFAULT 'scored',
    error_code  TEXT NOT NULL DEFAULT '',
    scored_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_verdicts_subject_rule_scored_at ON verdicts (tenant, subject, rule, scored_at DESC);

-- rule_state: per-(subject, rule) backoff bookkeeping for a scorer that's
-- erroring or over budget (design §4.8).
CREATE TABLE IF NOT EXISTS rule_state (
    tenant     TEXT NOT NULL,
    subject    TEXT NOT NULL,
    rule       TEXT NOT NULL,
    attempts   INT NOT NULL DEFAULT 0,
    retry_at   TIMESTAMPTZ,
    last_error TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant, subject, rule)
);

-- labels: operator/outcome decisions (design §4.9). rule = '' means a
-- subject-level label rather than a per-rule one.
CREATE TABLE IF NOT EXISTS labels (
    id           BIGSERIAL PRIMARY KEY,
    tenant       TEXT NOT NULL,
    subject      TEXT NOT NULL,
    rule         TEXT NOT NULL DEFAULT '',
    label        TEXT NOT NULL,
    source       TEXT NOT NULL,
    actor        TEXT NOT NULL,
    note         TEXT NOT NULL DEFAULT '',
    evidence_ref TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_labels_subject ON labels (tenant, subject);

-- corpus_examples: the redacted event slice + extracted features a label
-- writes for harness/eval use (design §4.9). S1 only creates the table;
-- the write path lands with the harness (S4) and label API (S3).
CREATE TABLE IF NOT EXISTS corpus_examples (
    id          BIGSERIAL PRIMARY KEY,
    tenant      TEXT NOT NULL,
    subject     TEXT NOT NULL,
    label_id    BIGINT NOT NULL,
    decision_at TIMESTAMPTZ NOT NULL,
    event_slice JSONB NOT NULL,
    features    JSONB NOT NULL DEFAULT '{}',
    split       TEXT NOT NULL DEFAULT 'train',
    gated       BOOLEAN NOT NULL DEFAULT false,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_corpus_examples_subject ON corpus_examples (tenant, subject);

-- calibrations: one row per (rule, scorer, checkpoint) calibration map
-- (design §4.6/§4.10), identified by its cal_<hash> id. S1 only creates
-- the table; `abusekit calibrate` (S4/S5) is what writes to it.
CREATE TABLE IF NOT EXISTS calibrations (
    id         TEXT PRIMARY KEY,
    tenant     TEXT NOT NULL DEFAULT '',
    rule       TEXT NOT NULL,
    scorer     TEXT NOT NULL,
    checkpoint TEXT NOT NULL DEFAULT '',
    method     TEXT NOT NULL,
    params     JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_calibrations_rule_scorer ON calibrations (rule, scorer, checkpoint);
