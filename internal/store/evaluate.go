package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrAlreadyClaimed is returned by ClaimSubjectForEvaluate when (tenant,
// subject) is currently claimed by another in-flight scoring pass — a
// worker Tick that picked it up via ClaimDirtySubjects, or a concurrent
// evaluate call for the same subject. internal/serve maps this to 429
// (design §4.4: evaluate is "rate-limited per subject (1/s)") rather than
// treating it as a real failure.
var ErrAlreadyClaimed = errors.New("store: subject is currently claimed by another scoring pass")

// ClaimSubjectForEvaluate claims exactly (tenant, subject) for a
// synchronous POST .../evaluate call (design §4.4), sharing the EXACT same
// claimed_until lease ClaimDirtySubjects uses — so a worker Tick and a
// concurrent evaluate call can never both be mid-scoring-round for the
// same subject at once, the same "same lease/timeout/budget rules"
// guarantee design asks evaluate to honour.
//
// Unlike ClaimDirtySubjects, this does not select by priority or
// dirty_seq/next_rescore_at — it claims the NAMED subject unconditionally
// (evaluate exists precisely so a product doesn't have to wait for the
// queue), as long as it exists, isn't class internal/synthetic (design
// §4.3: those are "stored but never scored" — evaluate must honour the
// same rule ClaimDirtySubjects' WHERE clause does), and isn't already
// claimed or in its failure backoff window.
//
// Returns ErrNotFound if the subject has never been seen by this tenant,
// or ErrAlreadyClaimed if it exists but couldn't be claimed right now.
func (s *Store) ClaimSubjectForEvaluate(ctx context.Context, tenant, subject string, now time.Time) (DirtySubject, error) {
	claimedUntil := now.Add(s.claimLeaseOrDefault())

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return DirtySubject{}, fmt.Errorf("store: begin evaluate-claim transaction: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	var (
		d     DirtySubject
		class string
	)
	row := tx.QueryRow(ctx, `
		SELECT `+claimCandidateColumns+`, class
		FROM subjects
		WHERE tenant = $1 AND subject = $2
		FOR UPDATE
	`, tenant, subject)
	if err := row.Scan(&d.Tenant, &d.Subject, &d.DirtySeq, &d.ScoredSeq, &d.CurrentTier, &d.FailCount, &class); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DirtySubject{}, ErrNotFound
		}
		return DirtySubject{}, fmt.Errorf("store: query subject %s for evaluate claim: %w", subject, err)
	}
	if class == "internal" || class == "synthetic" {
		return DirtySubject{}, fmt.Errorf("store: subject %s is class %q, which evaluate never scores", subject, class)
	}

	cmdTag, err := tx.Exec(ctx, `
		UPDATE subjects SET claimed_until = $1
		WHERE tenant = $2 AND subject = $3
		  AND (claimed_until IS NULL OR claimed_until < $4)
		  AND (next_attempt_at IS NULL OR next_attempt_at <= $4)
	`, claimedUntil, tenant, subject, now)
	if err != nil {
		return DirtySubject{}, fmt.Errorf("store: claim subject %s for evaluate: %w", subject, err)
	}
	if cmdTag.RowsAffected() == 0 {
		return DirtySubject{}, ErrAlreadyClaimed
	}

	if err := tx.Commit(ctx); err != nil {
		return DirtySubject{}, fmt.Errorf("store: commit evaluate-claim transaction: %w", err)
	}
	return d, nil
}
