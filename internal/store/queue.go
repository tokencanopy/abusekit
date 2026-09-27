package store

import (
	"context"
	"fmt"
	"time"
)

// DefaultClaimBatchSize is design §4.8's "batch 200 per 10s tick".
const DefaultClaimBatchSize = 200

// newSubjectAge is design §4.8's "new subjects (age < 24h)" priority
// bucket.
const newSubjectAge = 24 * time.Hour

// DirtySubject is one row ClaimDirtySubjects selected: enough for
// internal/worker to load the subject's events, extract features, and —
// after scoring — record the round against the exact dirty_seq it read
// here (design §4.8's compare-and-clear).
type DirtySubject struct {
	Tenant      string
	Subject     string
	DirtySeq    int64
	ScoredSeq   int64
	CurrentTier string // "unknown" for a never-scored subject; used to size an elevated-subject's budget headroom (design §4.8's 25% reserve).
}

// ClaimDirtySubjects selects up to limit subjects that need (re)scoring —
// design §4.8: dirty_seq > scored_seq, or a subject whose next_rescore_at
// has arrived even without a new event — ordered by priority (new
// subjects first, then higher current_score, then the subject whose most
// recent event is oldest) and internal/synthetic subjects excluded
// entirely (design §4.3's subject.class: "internal/synthetic subjects are
// stored but never scored").
//
// The selection runs inside a short SELECT ... FOR UPDATE SKIP LOCKED
// transaction that commits immediately after reading (S2): the lock's
// only job is to stop two worker instances ticking in the same instant
// from both claiming the identical row, not to hold the row for the
// whole scoring pass that follows — that would mean holding a
// transaction open across a scorer call, undesirable even for the fast
// local scorer and actively wrong once a network-bound vendor adapter
// (S5) is in the mix. Correctness against genuine double-scoring is
// UpsertVerdicts' own compare-and-clear (ErrStaleRound), not this lock:
// two instances racing to score the same subject can both proceed, but
// only the round with the higher dirty_seq-at-start ever commits its
// summary, and ClaimDirtySubjects' lock only makes that race the
// exception rather than the common case across many instances hammering
// the same queue.
//
// "oldest dirty" is approximated with last_event_at ASC: S1's schema has
// no separate "became dirty at" timestamp, so a subject whose most recent
// event is furthest in the past is treated as the most overdue. A future
// migration could add a dirty_since column for a precise version of this
// if it turns out to matter in practice.
func (s *Store) ClaimDirtySubjects(ctx context.Context, now time.Time, limit int) ([]DirtySubject, error) {
	if limit <= 0 {
		limit = DefaultClaimBatchSize
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: begin claim transaction: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	rows, err := tx.Query(ctx, `
		SELECT tenant, subject, dirty_seq, scored_seq, current_tier
		FROM subjects
		WHERE class NOT IN ('internal', 'synthetic')
		  AND (dirty_seq > scored_seq OR (next_rescore_at IS NOT NULL AND next_rescore_at <= $1))
		ORDER BY
			(first_seen_at > $2) DESC,
			current_score DESC NULLS LAST,
			last_event_at ASC,
			tenant, subject
		LIMIT $3
		FOR UPDATE SKIP LOCKED
	`, now, now.Add(-newSubjectAge), limit)
	if err != nil {
		return nil, fmt.Errorf("store: query dirty subjects: %w", err)
	}

	var out []DirtySubject
	for rows.Next() {
		var d DirtySubject
		if err := rows.Scan(&d.Tenant, &d.Subject, &d.DirtySeq, &d.ScoredSeq, &d.CurrentTier); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: scan dirty subject: %w", err)
		}
		out = append(out, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate dirty subjects: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("store: commit claim transaction: %w", err)
	}
	return out, nil
}
