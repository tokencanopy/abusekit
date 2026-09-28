package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// DefaultClaimBatchSize is design §4.8's "batch 200 per 10s tick".
const DefaultClaimBatchSize = 200

// DefaultClaimLease bounds how long a claimed subject is excluded from
// re-selection by any instance's ClaimDirtySubjects call (B3 fix round) —
// long enough to comfortably outlast one subject's scoring pass, short
// enough that a crashed worker's claim doesn't strand a subject for long.
const DefaultClaimLease = 2 * time.Minute

// newSubjectAge is design §4.8's "new subjects (age < 24h)" priority
// bucket.
const newSubjectAge = 24 * time.Hour

// DirtySubject is one row ClaimDirtySubjects selected: enough for
// internal/worker to load the subject's events, extract features, and —
// after scoring — record the round against the exact dirty_seq it read
// here (design §4.8's compare-and-clear), or a failure against the exact
// fail_count it read here (B3 fix round).
type DirtySubject struct {
	Tenant      string
	Subject     string
	DirtySeq    int64
	ScoredSeq   int64
	CurrentTier string // "unknown" for a never-scored subject; used to size an elevated-subject's budget headroom (design §4.8's 25% reserve).
	FailCount   int    // consecutive whole-pass failures so far (B3 fix round) — 0 for a subject that has never failed outright.
	// ClaimedUntil is the EXACT claimed_until value this claim set (T3,
	// round 3) — the same value for every subject in one ClaimDirtySubjects
	// batch, or ClaimSubjectForEvaluate's own single-subject claim. Callers
	// pass this back to ReleaseClaim so it can compare-and-clear rather
	// than clearing unconditionally: see ReleaseClaim's own doc comment for
	// why an unconditional release is unsafe.
	ClaimedUntil time.Time
}

// claimCandidateColumns is the column list both claim-selection queries
// share.
const claimCandidateColumns = `tenant, subject, dirty_seq, scored_seq, current_tier, fail_count`

// claimCandidateOrder is the priority ordering (S5 fix round) EVERY
// candidate query below sorts by, apart from the is_dirty split itself
// (each query is already fixed to one is_dirty value, so it isn't a SQL
// column here): new subjects (age < 24h) first, then higher current_score
// first with a NEVER-SCORED subject (current_score IS NULL) ranked
// HIGHEST of all — an account with no evidence yet is the most urgent to
// get a first score, not the least — then the subject whose most recent
// event is oldest.
const claimCandidateOrder = `
	(first_seen_at > $2) DESC,
	current_score DESC NULLS FIRST,
	last_event_at ASC,
	tenant, subject
`

// ClaimDirtySubjects selects AND CLAIMS up to limit subjects that need
// (re)scoring — design §4.8: dirty_seq > scored_seq, or a subject whose
// next_rescore_at has arrived even without a new event — ordered by
// priority, excluding internal/synthetic subjects (design §4.3) and any
// subject currently claimed by another in-flight pass or still within its
// failure backoff window (B3 fix round: claimed_until/next_attempt_at).
//
// Priority order (S5 fix round): subjects with a real dirty_seq bump rank
// STRICTLY ahead of ones that are only due for a timer-driven rescore, so
// a burst of real activity is never starved behind window-decay busywork
// — see claimCandidateOrder for the ordering within each of those two
// groups.
//
// Implementation (B3/S4 fix round): the two selection arms (dirty vs.
// rescore-only) are two SEPARATE `FOR UPDATE SKIP LOCKED` queries — S4
// asked for a UNION of independently-indexed queries instead of one OR'd
// WHERE clause (each arm now has its own partial index, migrations/006),
// but Postgres flatly refuses FOR UPDATE anywhere inside a UNION (even
// nested in a subquery: "FOR UPDATE is not allowed with UNION"), so this
// runs them as two plain queries and merges in Go instead. That merge is
// simple, not a general one: since is_dirty is the TOP-priority sort key
// and is constant within each arm, EVERY dirty-arm row outranks EVERY
// rescore-arm row — so it's "take up to limit already-sorted rows from
// the dirty arm, then fill any remainder from the already-sorted rescore
// arm", not an interleaved merge. Both arms run inside one transaction so
// their FOR UPDATE SKIP LOCKED locks are held together with the final
// UPDATE that sets claimed_until, which is what actually claims them —
// proven necessary (B3): a bare "select then commit before scoring" claim
// let two instances issue 40 scorer calls for 20 subjects; claimed_until
// now excludes a subject from re-selection for the WHOLE scoring pass
// that follows, not just this one transaction.
// DirtyArmQuery and RescoreArmQuery are ClaimDirtySubjects' two claim-
// selection arms, exported as named constants (R10 round 2) so a test
// asserting they hit their partial indexes (EXPLAIN) runs the EXACT
// query string this method does — not a hand-copied literal that could
// silently drift out of sync with it. Each takes $1=now, $2=newSubjectCutoff
// (used only inside claimCandidateOrder), $3=limit, matching
// queryClaimCandidates' own documented parameter contract.
const (
	DirtyArmQuery = `
		SELECT ` + claimCandidateColumns + `
		FROM subjects
		WHERE class NOT IN ('internal', 'synthetic')
		  AND (claimed_until IS NULL OR claimed_until < $1)
		  AND (next_attempt_at IS NULL OR next_attempt_at <= $1)
		  AND dirty_seq > scored_seq
		ORDER BY ` + claimCandidateOrder + `
		LIMIT $3
		FOR UPDATE SKIP LOCKED
	`
	RescoreArmQuery = `
		SELECT ` + claimCandidateColumns + `
		FROM subjects
		WHERE class NOT IN ('internal', 'synthetic')
		  AND (claimed_until IS NULL OR claimed_until < $1)
		  AND (next_attempt_at IS NULL OR next_attempt_at <= $1)
		  AND dirty_seq <= scored_seq
		  AND next_rescore_at IS NOT NULL AND next_rescore_at <= $1
		ORDER BY ` + claimCandidateOrder + `
		LIMIT $3
		FOR UPDATE SKIP LOCKED
	`
)

func (s *Store) ClaimDirtySubjects(ctx context.Context, now time.Time, limit int) ([]DirtySubject, error) {
	if limit <= 0 {
		limit = DefaultClaimBatchSize
	}
	newSubjectCutoff := now.Add(-newSubjectAge)
	// Truncated to microsecond precision (timestamptz's own limit) before
	// ever being used as a write parameter, matching
	// ClaimSubjectForEvaluate's own fix for the same class of issue — kept
	// even though the RETURNING clause below is this method's primary
	// defense, since it's also the fallback value if that RETURNING result
	// set ever comes back empty (see below).
	claimedUntil := now.Add(s.claimLeaseOrDefault()).Truncate(time.Microsecond)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: begin claim transaction: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	dirty, err := queryClaimCandidates(ctx, tx, DirtyArmQuery, now, newSubjectCutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("store: query dirty claim candidates: %w", err)
	}

	out := dirty
	if len(out) < limit {
		rescoreOnly, err := queryClaimCandidates(ctx, tx, RescoreArmQuery, now, newSubjectCutoff, limit-len(out))
		if err != nil {
			return nil, fmt.Errorf("store: query rescore-only claim candidates: %w", err)
		}
		out = append(out, rescoreOnly...)
	}
	if len(out) > limit {
		out = out[:limit]
	}

	if len(out) > 0 {
		tenants := make([]string, len(out))
		subjects := make([]string, len(out))
		for i, d := range out {
			tenants[i] = d.Tenant
			subjects[i] = d.Subject
		}
		// Follow-up (CI red on Linux): RETURNING the value Postgres actually
		// stored, rather than trusting the Go-side claimedUntil to survive
		// its own round trip byte-for-byte — timestamptz is microsecond
		// precision, and a bare time.Time parameter's encoding path isn't
		// guaranteed to truncate identically on every OS/driver combination
		// (a Linux CI run surfaced exactly this: the value ReleaseClaim
		// later compared against didn't match what got stored, even though
		// both sides used "the same" Go value — see ReleaseClaim's own doc
		// comment for the full explanation). Every row in one batch shares
		// the identical claimed_until, so reading it back off any one
		// returned row is authoritative for all of them.
		rows, err := tx.Query(ctx, `
			UPDATE subjects s SET claimed_until = $1
			FROM unnest($2::text[], $3::text[]) AS c(tenant, subject)
			WHERE s.tenant = c.tenant AND s.subject = c.subject
			RETURNING s.claimed_until
		`, claimedUntil, tenants, subjects)
		if err != nil {
			return nil, fmt.Errorf("store: set claimed_until: %w", err)
		}
		storedClaims, err := pgx.CollectRows(rows, pgx.RowTo[time.Time])
		if err != nil {
			return nil, fmt.Errorf("store: collect returned claimed_until: %w", err)
		}
		// Every row in the batch shares the identical claimed_until (one
		// $1 parameter for the whole UPDATE) — any one of them is
		// authoritative for all of out. storedClaims is empty only if
		// every candidate lost a claim race between being SELECTed above
		// and this UPDATE (SKIP LOCKED already makes that vanishingly
		// unlikely, but fall back to the Go-side value rather than panic
		// if it ever happens).
		stored := claimedUntil
		if len(storedClaims) > 0 {
			stored = storedClaims[0]
		}
		for i := range out {
			out[i].ClaimedUntil = stored
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("store: commit claim transaction: %w", err)
	}
	return out, nil
}

// queryClaimCandidates runs one of ClaimDirtySubjects' two arm queries
// (query must select exactly claimCandidateColumns, in that order, and
// take $1=now, $2=newSubjectCutoff, $3=limit as its parameters) and scans
// the results.
func queryClaimCandidates(ctx context.Context, tx pgx.Tx, query string, now, newSubjectCutoff time.Time, limit int) ([]DirtySubject, error) {
	rows, err := tx.Query(ctx, query, now, newSubjectCutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DirtySubject
	for rows.Next() {
		var d DirtySubject
		if err := rows.Scan(&d.Tenant, &d.Subject, &d.DirtySeq, &d.ScoredSeq, &d.CurrentTier, &d.FailCount); err != nil {
			return nil, fmt.Errorf("scan claim candidate: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate claim candidates: %w", err)
	}
	return out, nil
}

// ReleaseClaim clears claimed_until for (tenant, subject) — called by
// internal/worker once a scoring pass concludes on a path that doesn't
// already touch subjects itself (a stale round; see UpsertVerdicts and
// RecordSubjectFailure for the success/failure paths, which clear it as
// part of their own update).
//
// claimedUntil is the EXACT value the caller's own claim set (T3, round
// 3): the clear is a compare-and-clear against it
// (WHERE claimed_until = $3), not unconditional. Proven necessary: an
// unconditional `SET claimed_until = NULL` released whatever lease
// happened to be on the row AT THE TIME this call ran — including a
// DIFFERENT, still-active lease a concurrent claim (the worker's own next
// Tick, or another evaluate call) took in the meantime, e.g. after the
// original caller's own claim attempt raced an ambiguous commit
// (store.ErrClaimAmbiguous) and its actual outcome was never certain.
// Releasing someone else's live lease early breaks the mutual exclusion
// claiming exists for in the first place — a second scoring pass could
// then start concurrently with the one whose lease was just stolen out
// from under it. A caller that never actually held a claim (claimedUntil
// is the zero value, or simply doesn't match what's in the row) safely
// no-ops here instead.
//
// claimedUntil PRECISION (CI red on Linux, follow-up to T3): timestamptz
// stores microsecond precision; Go's time.Time carries nanoseconds, and a
// real wall-clock-derived value can carry a nonzero sub-microsecond
// remainder that a round trip through Postgres silently drops (macOS's
// own clock reads happened to mask this locally, which is how it first
// shipped). Both claim methods now hand this method a value that is
// EITHER read back from Postgres itself via a RETURNING clause
// (ClaimDirtySubjects, ClaimSubjectForEvaluate's normal success path —
// see their own comments) OR truncated to time.Microsecond before it was
// ever used as a write parameter in the first place
// (ClaimSubjectForEvaluate's ErrClaimAmbiguous path, where no RETURNING
// value exists to read back because the commit's own outcome is what's
// uncertain) — either way, guaranteed to be exactly what a matching row
// would have stored, never a nanosecond-bearing value that could silently
// fail this WHERE clause's equality check against it.
func (s *Store) ReleaseClaim(ctx context.Context, tenant, subject string, claimedUntil time.Time) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE subjects SET claimed_until = NULL WHERE tenant = $1 AND subject = $2 AND claimed_until = $3
	`, tenant, subject, claimedUntil)
	if err != nil {
		return fmt.Errorf("store: release claim for %s: %w", subject, err)
	}
	return nil
}

// ExtendClaims pushes every (tenants[i], subjects[i])'s claimed_until out
// to now+the configured claim lease, in one statement (R4 round 2).
// tenants and subjects must be the same length; a length of 0 is a no-op.
//
// Called by internal/worker at the start of EVERY per-subject loop
// iteration, for the WHOLE REMAINING TAIL of the current batch (not just
// the one subject about to be processed), with a freshly-taken now — not
// the tick-level now ClaimDirtySubjects used to claim the whole batch.
// Proven necessary with only the ONE-subject-at-a-time version first
// tried: with a 200ms lease and three subjects each taking 150ms to
// score, the third subject's turn doesn't come up until 300ms in — 100ms
// past its UNTOUCHED original claim (set once, batch-wide, at t=0) — so a
// second instance's ClaimDirtySubjects call landing in that window would
// still have reclaimed it even though the first instance was working
// through the batch correctly. Re-extending the whole remaining tail on
// every iteration (not just the current subject) means no claimed
// subject's lease can ever lag behind actual elapsed time by more than
// one iteration's own processing time — bounded by a single subject's
// worst case (ScoreTimeout), never the whole batch's cumulative total.
//
// This intentionally does not check whether a subject is CURRENTLY
// claimed (by this or any instance) before extending — it always sets
// claimed_until unconditionally, the same as ClaimDirtySubjects' own
// claiming UPDATE. The caller only ever calls this for subjects it just
// claimed itself.
func (s *Store) ExtendClaims(ctx context.Context, tenants, subjects []string, now time.Time) error {
	if len(tenants) == 0 {
		return nil
	}
	until := now.Add(s.claimLeaseOrDefault())
	_, err := s.pool.Exec(ctx, `
		UPDATE subjects s SET claimed_until = $1
		FROM unnest($2::text[], $3::text[]) AS c(tenant, subject)
		WHERE s.tenant = c.tenant AND s.subject = c.subject
	`, until, tenants, subjects)
	if err != nil {
		return fmt.Errorf("store: extend claims: %w", err)
	}
	return nil
}

// RecordSubjectFailure is B3 fix round's whole-pass failure backoff:
// called when a scoring pass fails outright (EventsForSubject,
// feature.Extract, or another store call erroring — as opposed to one
// rule's scorer erroring, which internal/worker handles via rule_state
// without failing the round). It releases the claim, increments
// fail_count, and sets next_attempt_at so ClaimDirtySubjects won't
// reselect this subject until then — internal/worker owns the actual
// backoff SCHEDULE (the same 30s/2m/5m/15m-capped one rule_state uses),
// not this method, so a policy change never touches store.
func (s *Store) RecordSubjectFailure(ctx context.Context, tenant, subject string, nextAttemptAt time.Time) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE subjects SET
			claimed_until   = NULL,
			fail_count      = fail_count + 1,
			next_attempt_at = $3
		WHERE tenant = $1 AND subject = $2
	`, tenant, subject, nextAttemptAt)
	if err != nil {
		return fmt.Errorf("store: record subject failure for %s: %w", subject, err)
	}
	return nil
}

// QueueStats reports design §4.8's "queue depth, oldest dirty age" metrics
// (S8 fix round): how many subjects currently match ClaimDirtySubjects'
// own selection predicate (regardless of any claim lease or failure
// backoff — those are transient, and a subject temporarily excluded by
// one is still logically part of the backlog), and how long the least
// recently active of them has been waiting (last_event_at, the same proxy
// ClaimDirtySubjects' priority ordering uses for "oldest dirty" — S1's
// schema has no separate "became dirty at" column). oldestDirtyAge is 0
// when depth is 0.
func (s *Store) QueueStats(ctx context.Context, now time.Time) (depth int, oldestDirtyAge time.Duration, err error) {
	var oldestSeconds float64
	if err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*), COALESCE(EXTRACT(EPOCH FROM ($1 - MIN(last_event_at))), 0)
		FROM subjects
		WHERE class NOT IN ('internal', 'synthetic')
		  AND (dirty_seq > scored_seq OR (next_rescore_at IS NOT NULL AND next_rescore_at <= $1))
	`, now).Scan(&depth, &oldestSeconds); err != nil {
		return 0, 0, fmt.Errorf("store: queue stats: %w", err)
	}
	if depth == 0 {
		return 0, 0, nil
	}
	return depth, time.Duration(oldestSeconds * float64(time.Second)), nil
}
