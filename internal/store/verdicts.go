package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// VerdictRecord is one rule's result for a scoring round, in the shape
// UpsertVerdicts writes to the verdicts table. It mirrors
// internal/core.Signal closely on purpose — the worker (S2) builds these
// directly from a core.Verdict's Signals.
type VerdictRecord struct {
	Rule        string
	Mode        string // "advise" | "shadow"
	Scorer      string
	Model       string
	Checkpoint  string
	Render      string
	Calibration string
	Probs       map[string]float64
	// Risk is nil when Status == "unscored".
	Risk      *float64
	Flagged   bool
	Reason    string
	LLMReason *string
	InputHash string
	Status    string // "scored" | "unscored"
	ErrorCode string
}

// SubjectSummary is the round-level result (internal/core.Verdict, minus
// per-rule Signals) that UpsertVerdicts materializes onto the subjects
// row's current_* columns.
type SubjectSummary struct {
	Tier  string
	Score float64
}

// UpsertVerdicts inserts one verdicts row per record (verdicts are
// append-only history — nothing is actually updated in place, despite the
// name; "Upsert" here matches design §4.1's naming for "record this
// round's results, whatever they were before") and materializes the
// round's summary onto subjects.current_tier/current_score/
// current_verdict_id/current_scored_at.
//
// dirtySeqAtStart is the subjects.dirty_seq value the caller read before
// beginning this scoring round; scored_seq is advanced to
// GREATEST(scored_seq, dirtySeqAtStart) rather than set outright, so a
// second instance's concurrent (and possibly newer) round for the same
// subject can never regress scored_seq backwards. This is the
// compare-and-clear design §4.8 describes: an event that arrives mid-scan
// bumps dirty_seq past dirtySeqAtStart, and the subject stays dirty for
// the next tick.
//
// current_verdict_id is set to the last record's id (records are inserted
// in the given order) — S1 has no concept of "the primary verdict of a
// round" beyond that; S3's score API is what actually needs to point at
// something meaningful here, and can revisit this once the API shape
// forces the decision.
//
// Returns the inserted verdict ids in the same order as records. Records
// input as empty is a no-op returning (nil, nil) — it does not touch
// subjects at all, since "no records" isn't a meaningful scoring round.
func (s *Store) UpsertVerdicts(ctx context.Context, tenant, subject string, dirtySeqAtStart int64, records []VerdictRecord, summary SubjectSummary) ([]int64, error) {
	if len(records) == 0 {
		return nil, nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: begin verdicts transaction: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	ids := make([]int64, 0, len(records))
	for _, r := range records {
		probsJSON, err := json.Marshal(r.Probs)
		if err != nil {
			return nil, fmt.Errorf("store: marshal probs for rule %s: %w", r.Rule, err)
		}
		var id int64
		err = tx.QueryRow(ctx, `
			INSERT INTO verdicts
				(tenant, subject, rule, mode, scorer, model, checkpoint, render, calibration,
				 probs, risk, flagged, reason, llm_reason, input_hash, status, error_code)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
			RETURNING id
		`, tenant, subject, r.Rule, r.Mode, r.Scorer, r.Model, r.Checkpoint, r.Render, r.Calibration,
			probsJSON, r.Risk, r.Flagged, r.Reason, r.LLMReason, r.InputHash, r.Status, r.ErrorCode,
		).Scan(&id)
		if err != nil {
			return nil, fmt.Errorf("store: insert verdict for rule %s: %w", r.Rule, err)
		}
		ids = append(ids, id)
	}

	lastID := ids[len(ids)-1]
	if _, err := tx.Exec(ctx, `
		UPDATE subjects SET
			current_tier       = $1,
			current_score      = $2,
			current_verdict_id = $3,
			current_scored_at  = now(),
			scored_seq         = GREATEST(scored_seq, $4)
		WHERE tenant = $5 AND subject = $6
	`, summary.Tier, summary.Score, lastID, dirtySeqAtStart, tenant, subject); err != nil {
		return nil, fmt.Errorf("store: update subject summary for %s: %w", subject, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("store: commit verdicts transaction: %w", err)
	}
	return ids, nil
}

// SubjectSignal is one rule's latest recorded verdict for a subject, as
// SubjectView reports it.
type SubjectSignal struct {
	Rule        string
	Mode        string
	Status      string
	Risk        float64
	Flagged     bool
	Model       string
	Checkpoint  string
	Calibration string
	Reason      string
	ErrorCode   string
	ScoredAt    time.Time
}

// SubjectView is the read model behind GET /v1/subjects/{subject}
// (design §4.4), independent of the HTTP layer that will wrap it in S3.
type SubjectView struct {
	Subject string
	Class   string
	// Tier and Score are the materialized subjects.current_tier/score —
	// "unknown"/0 for a subject that has never been scored.
	Tier  string
	Score float64
	// Degraded is derived here (not stored) from whether any advise-mode
	// rule's latest signal is unscored (design §4.4).
	Degraded bool
	// Stale is true when the subject has events after its last scoring
	// round, or has never been scored at all despite having events.
	Stale bool
	// EventsSinceScore is dirty_seq - scored_seq: how many "bumps" have
	// happened since the last scoring round started.
	EventsSinceScore int64
	// ScoredAt is nil for a never-scored subject.
	ScoredAt *time.Time
	// Signals holds each rule's most recent verdict, one per rule name.
	Signals []SubjectSignal
}

// SubjectView returns the read model for (tenant, subject), or
// ErrNotFound if the subject has never been seen by this tenant (design
// §4.4: "404 not_found only for a subject never seen in this tenant" — a
// seen-but-unscored subject is a normal SubjectView with Tier "unknown"
// and no Signals, not an error).
func (s *Store) SubjectView(ctx context.Context, tenant, subject string) (*SubjectView, error) {
	v := &SubjectView{Subject: subject}
	var (
		currentScore    *float64
		currentScoredAt *time.Time
		dirtySeq        int64
		scoredSeq       int64
		lastEventAt     time.Time
	)
	err := s.pool.QueryRow(ctx, `
		SELECT class, current_tier, current_score, current_scored_at, dirty_seq, scored_seq, last_event_at
		FROM subjects WHERE tenant = $1 AND subject = $2
	`, tenant, subject).Scan(&v.Class, &v.Tier, &currentScore, &currentScoredAt, &dirtySeq, &scoredSeq, &lastEventAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: query subject %s: %w", subject, err)
	}
	if currentScore != nil {
		v.Score = *currentScore
	}
	v.ScoredAt = currentScoredAt
	v.EventsSinceScore = dirtySeq - scoredSeq
	if currentScoredAt != nil {
		v.Stale = lastEventAt.After(*currentScoredAt)
	} else {
		v.Stale = dirtySeq > 0
	}

	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (rule)
			rule, mode, status, risk, flagged, model, checkpoint, calibration, reason, error_code, scored_at
		FROM verdicts
		WHERE tenant = $1 AND subject = $2
		ORDER BY rule, scored_at DESC
	`, tenant, subject)
	if err != nil {
		return nil, fmt.Errorf("store: query verdicts for subject %s: %w", subject, err)
	}
	defer rows.Close()

	for rows.Next() {
		var sig SubjectSignal
		var risk *float64
		if err := rows.Scan(&sig.Rule, &sig.Mode, &sig.Status, &risk, &sig.Flagged, &sig.Model,
			&sig.Checkpoint, &sig.Calibration, &sig.Reason, &sig.ErrorCode, &sig.ScoredAt); err != nil {
			return nil, fmt.Errorf("store: scan verdict row: %w", err)
		}
		if risk != nil {
			sig.Risk = *risk
		}
		if sig.Mode == "advise" && sig.Status == "unscored" {
			v.Degraded = true
		}
		v.Signals = append(v.Signals, sig)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate verdicts for subject %s: %w", subject, err)
	}

	return v, nil
}
