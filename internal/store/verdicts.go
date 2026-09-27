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

// ErrStaleRound is returned by UpsertVerdicts when a newer round already
// recorded a higher scored_seq for the subject (R2, round 2): the whole
// transaction — verdict inserts included — is rolled back rather than
// partially committed, so a stale round can never leave rows that
// SubjectView would show as "the latest signal per rule" beside a
// current_tier the stale round didn't actually win. Not a failure the
// caller needs to report anywhere; it means exactly what it says, "this
// round is obsolete, someone else already scored a newer one."
var ErrStaleRound = errors.New("store: stale round: a newer round already scored this subject")

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
	// S5: only materialize this round's summary if it is at least as new
	// as whatever scored_seq the subject already has recorded. Without the
	// "AND scored_seq <= $4" guard, a round that started reading dirty_seq
	// early (dirtySeqAtStart is small) but COMMITS after a later, newer
	// round already advanced scored_seq would overwrite current_tier with
	// its own, older, now-stale summary — proven: seq=3 recording "high"
	// followed by a late-committing seq=1 round recording "low" left the
	// subject at "low". GREATEST(scored_seq, $4) in the SET list is now
	// only reached when the WHERE clause's own scored_seq <= $4 already
	// holds, so it can never regress either.
	cmdTag, err := tx.Exec(ctx, `
		UPDATE subjects SET
			current_tier       = $1,
			current_score      = $2,
			current_verdict_id = $3,
			current_scored_at  = now(),
			scored_seq         = GREATEST(scored_seq, $4)
		WHERE tenant = $5 AND subject = $6 AND scored_seq <= $4
	`, summary.Tier, summary.Score, lastID, dirtySeqAtStart, tenant, subject)
	if err != nil {
		return nil, fmt.Errorf("store: update subject summary for %s: %w", subject, err)
	}
	if cmdTag.RowsAffected() == 0 {
		// Zero rows affected means either (a) the subject row doesn't
		// exist at all — a real error, not a silent no-op, since the
		// caller believes it just recorded a scoring round for a real
		// subject — or (b) the subject exists but a newer round already
		// recorded a higher scored_seq. R2 (round 2): case (b) used to
		// fall through and commit anyway, which left this round's verdict
		// INSERTs visible even though its summary was correctly rejected —
		// SubjectView's "latest signal per rule" could then show a stale
		// round's risk/flagged right beside a current_tier that round
		// never actually won. Both cases now roll back the whole
		// transaction (the deferred tx.Rollback below) instead of
		// committing a partial result.
		var exists bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM subjects WHERE tenant = $1 AND subject = $2)`,
			tenant, subject,
		).Scan(&exists); err != nil {
			return nil, fmt.Errorf("store: check subject existence for %s: %w", subject, err)
		}
		if !exists {
			return nil, fmt.Errorf("store: cannot record verdicts for %s/%s: subject row does not exist", tenant, subject)
		}
		return nil, ErrStaleRound
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
//
// currentRules, when non-nil, restricts Signals (and the Degraded
// computed from them) to rules present in the caller's current config
// (S14): a rule renamed or dropped from config.rules.yaml still has old
// verdicts rows in history, but they must stop contributing to the live
// view. Pass nil to see every rule that ever recorded a verdict for this
// subject (e.g. for an operator/debug view), matching the pre-S14
// behavior.
func (s *Store) SubjectView(ctx context.Context, tenant, subject string, currentRules []string) (*SubjectView, error) {
	v := &SubjectView{Subject: subject}
	var (
		currentScore    *float64
		currentScoredAt *time.Time
		dirtySeq        int64
		scoredSeq       int64
	)
	err := s.pool.QueryRow(ctx, `
		SELECT class, current_tier, current_score, current_scored_at, dirty_seq, scored_seq
		FROM subjects WHERE tenant = $1 AND subject = $2
	`, tenant, subject).Scan(&v.Class, &v.Tier, &currentScore, &currentScoredAt, &dirtySeq, &scoredSeq)
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
	// S4: Stale is purely dirty_seq > scored_seq — comparing wall-clock
	// timestamps (last_event_at vs. current_scored_at) from two different
	// clocks (the app server that stamped the event's `at`, and Postgres's
	// own now()) is wrong even a few hundred milliseconds of drift away,
	// and is *always* wrong for a fictional test timestamp far from the
	// real clock.
	v.Stale = dirtySeq > scoredSeq

	query := `
		SELECT DISTINCT ON (rule)
			rule, mode, status, risk, flagged, model, checkpoint, calibration, reason, error_code, scored_at
		FROM verdicts
		WHERE tenant = $1 AND subject = $2`
	args := []any{tenant, subject}
	if len(currentRules) > 0 {
		query += ` AND rule = ANY($3)`
		args = append(args, currentRules)
	}
	// S14: a DISTINCT ON tiebreak solely on scored_at is non-deterministic
	// when two verdicts for the same rule share an identical timestamp
	// (e.g. two records from the same UpsertVerdicts call, which share one
	// transaction's now()) — id DESC breaks the tie toward the
	// later-inserted row.
	query += ` ORDER BY rule, scored_at DESC, id DESC`

	rows, err := s.pool.Query(ctx, query, args...)
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
