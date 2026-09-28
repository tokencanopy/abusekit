package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrNotScorable is returned by ClaimSubjectForEvaluate when the subject
// is class internal/synthetic (design §4.3: those are "stored but never
// scored"). internal/serve's S1 fix-round contract: this is NOT a failure
// — a caller (e.g. e2a's own prober, which uses synthetic accounts) gets
// back the subject's stored view with evaluated_now:false, never a 500.
var ErrNotScorable = errors.New("store: subject class is not scored (internal/synthetic)")

// ErrBusy is returned by ClaimSubjectForEvaluate when (tenant, subject)
// exists and is scorable but currently claimed by another in-flight
// scoring pass (a worker Tick, or a concurrent evaluate call) or within
// its failure-backoff window. RetryAt is the actual instant that lease or
// backoff is expected to clear — S1 fix round: internal/serve derives a
// real Retry-After from this rather than a fixed guess.
type ErrBusy struct {
	RetryAt time.Time
}

func (e *ErrBusy) Error() string {
	// R8 (round 2 fix round): format RetryAt in UTC explicitly so the wire
	// value doesn't depend on the server process's local timezone
	// (RetryAt is typically already UTC coming out of Postgres, but .UTC()
	// makes that a guarantee rather than an assumption).
	return "store: subject is busy (claimed or in backoff) until " + e.RetryAt.UTC().Format(time.RFC3339)
}

// ErrClaimAmbiguous wraps a failure from ClaimSubjectForEvaluate's own
// COMMIT step specifically (round 2 fix round, R2): once the claim UPDATE
// has executed inside the transaction, a context deadline/cancellation
// during tx.Commit(ctx) leaves the outcome genuinely ambiguous — the
// COMMIT may have already landed durably on the server before the
// client-side context tore down the connection, or it may not have. The
// caller (worker.EvaluateSubject) can't tell which happened from the
// error alone, so it treats this specific error as "may have claimed the
// subject" and issues a best-effort release with its own timeout,
// independent of the (already expired) caller context — ReleaseClaim is
// unconditional and idempotent, so releasing a claim that never actually
// landed is a harmless no-op. This is deliberately NOT used for ErrBusy,
// ErrNotFound, or ErrNotScorable: those are returned before any commit is
// attempted (or, for the lost-race case, after a definitive
// zero-rows-affected UPDATE), so there is nothing of ours to release —
// treating them as ambiguous too would risk releasing a DIFFERENT,
// legitimate concurrent claimant's lease.
type ErrClaimAmbiguous struct {
	Err error
}

func (e *ErrClaimAmbiguous) Error() string {
	return "store: evaluate-claim commit outcome is ambiguous: " + e.Err.Error()
}

func (e *ErrClaimAmbiguous) Unwrap() error { return e.Err }

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
// ErrNotScorable if it's class internal/synthetic, or *ErrBusy (with a
// real RetryAt) if it exists and is scorable but couldn't be claimed right
// now.
func (s *Store) ClaimSubjectForEvaluate(ctx context.Context, tenant, subject string, now time.Time) (DirtySubject, error) {
	claimedUntil := now.Add(s.claimLeaseOrDefault())

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return DirtySubject{}, fmt.Errorf("store: begin evaluate-claim transaction: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	var (
		d             DirtySubject
		class         string
		claimedUntilD *time.Time
		nextAttemptAt *time.Time
	)
	row := tx.QueryRow(ctx, `
		SELECT `+claimCandidateColumns+`, class, claimed_until, next_attempt_at
		FROM subjects
		WHERE tenant = $1 AND subject = $2
		FOR UPDATE
	`, tenant, subject)
	if err := row.Scan(&d.Tenant, &d.Subject, &d.DirtySeq, &d.ScoredSeq, &d.CurrentTier, &d.FailCount, &class, &claimedUntilD, &nextAttemptAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DirtySubject{}, ErrNotFound
		}
		return DirtySubject{}, fmt.Errorf("store: query subject %s for evaluate claim: %w", subject, err)
	}
	if class == "internal" || class == "synthetic" {
		return DirtySubject{}, ErrNotScorable
	}
	// S1 fix round: report busy (not claimable now) BEFORE attempting the
	// claim UPDATE below, using the SAME two columns that UPDATE's WHERE
	// clause gates on, so the RetryAt in the error is computed from
	// EXACTLY what's blocking the claim, not a guess.
	if (claimedUntilD != nil && claimedUntilD.After(now)) || (nextAttemptAt != nil && nextAttemptAt.After(now)) {
		retryAt := now
		if claimedUntilD != nil && claimedUntilD.After(retryAt) {
			retryAt = *claimedUntilD
		}
		if nextAttemptAt != nil && nextAttemptAt.After(retryAt) {
			retryAt = *nextAttemptAt
		}
		return DirtySubject{}, &ErrBusy{RetryAt: retryAt}
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
		// Lost a race against a concurrent claimant between the SELECT
		// above and this UPDATE — report busy with a short, conservative
		// retry rather than failing outright; the exact remaining lease
		// isn't known at this point (the other claimant just set it), so a
		// caller retries shortly rather than being told a stale time.
		return DirtySubject{}, &ErrBusy{RetryAt: now.Add(time.Second)}
	}

	if err := tx.Commit(ctx); err != nil {
		// R2 (round 2 fix round): wrap distinctly from the other error
		// paths above — see ErrClaimAmbiguous.
		return DirtySubject{}, &ErrClaimAmbiguous{Err: fmt.Errorf("store: commit evaluate-claim transaction: %w", err)}
	}
	return d, nil
}
