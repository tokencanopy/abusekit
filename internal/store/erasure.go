package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErasureMode records which of design §4.4's two DELETE /v1/subjects/{id}
// paths a subject took.
type ErasureMode string

const (
	// ErasureModePurged means every row for the subject was deleted
	// outright (design: "purges everything for other subjects" — i.e.
	// subjects with no "abusive" label).
	ErasureModePurged ErasureMode = "purged"
	// ErasureModeTombstoned means the subject carries an "abusive" label:
	// event/verdict/corpus TEXT was destroyed but numeric features and
	// link hashes were kept under design's 24-month fraud-prevention
	// legitimate-interest basis.
	ErasureModeTombstoned ErasureMode = "tombstoned"
)

// ErasureResult reports what EraseSubject actually did, for the DELETE
// handler's response body.
type ErasureResult struct {
	Mode     ErasureMode
	ErasedAt time.Time
	// AlreadyErased is true when the subject had already been erased by an
	// earlier call — EraseSubject is idempotent (S3): it reports the
	// PREVIOUSLY recorded outcome rather than erroring or re-running the
	// purge/tombstone logic a second time.
	AlreadyErased bool
}

// HasAbusiveLabel reports whether subject carries at least one "abusive"
// label (subject-level or per-rule — design §4.4's erasure rule doesn't
// distinguish) for tenant. Used by EraseSubject to decide which of its two
// paths applies; exported separately because internal/serve's label
// handler and a future harness both have independent reasons to ask the
// same question.
func (s *Store) HasAbusiveLabel(ctx context.Context, tenant, subject string) (bool, error) {
	var has bool
	if err := s.pool.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM labels WHERE tenant = $1 AND subject = $2 AND label = 'abusive')
	`, tenant, subject).Scan(&has); err != nil {
		return false, fmt.Errorf("store: check abusive label for %s: %w", subject, err)
	}
	return has, nil
}

// hashSubjectID irreversibly pseudonymizes a subject id for a tombstoned
// corpus_examples row (design §4.4 / the erasure endpoint's own summary:
// "keep labelled corpus rows with the id hashed") — sha256, not a keyed
// HMAC, since nothing needs to reverse or re-join this value the way the
// tenant-keyed link hashes (design §4.2) do; it exists only so a
// retained-for-training corpus row no longer carries the subject's real
// identifier in plain text.
func hashSubjectID(subject string) string {
	sum := sha256.Sum256([]byte("abusekit-erasure/" + subject))
	return "erased_" + hex.EncodeToString(sum[:16])
}

// EraseSubject performs design §4.4's DELETE /v1/subjects/{subject} legal
// erasure request. Returns ErrNotFound if the subject has never been seen
// by this tenant — which, after a PURGE (below), also covers a REPEAT
// erasure of the same subject: purging deletes the subjects row itself,
// so there is nothing left to record "already erased" against, and a
// second call correctly 404s the same way a genuinely-never-seen subject
// would. A TOMBSTONED subject's row is deliberately kept (its numeric
// score is retained evidence), so a repeat call on THAT path finds it and
// returns ErasureResult.AlreadyErased=true instead of re-running the
// tombstone transformation a second time.
//
// Two paths, chosen by HasAbusiveLabel:
//   - Not abusive-labelled: PURGE. Every row for (tenant, subject) across
//     events, links, verdicts, rule_state, labels, corpus_examples and
//     subjects itself is deleted. A later event for the same subject id
//     starts a brand-new history, exactly as if it had never been seen.
//   - Abusive-labelled: TOMBSTONE, under design's 24-month fraud-
//     prevention legitimate-interest basis. events.data, verdicts.reason/
//     llm_reason, and corpus_examples.event_slice are overwritten to
//     destroy their TEXT content; corpus_examples.subject is replaced with
//     an irreversible hash (hashSubjectID); links, labels, and every
//     NUMERIC column (verdicts.risk/probs, corpus_examples.features,
//     subjects.current_score/current_tier) are left untouched. The
//     subjects row itself is kept (marked erased_at/erasure_mode) so
//     GET /v1/subjects/{subject} keeps answering with its retained
//     numeric score rather than 404ing.
//
// A scheduled purge once the 24-month retention window itself elapses is
// NOT implemented here — no janitor/cron process exists anywhere in this
// repo yet (S3's own scope is the point-in-time transformation this
// endpoint performs on request, not a time-based sweep); see the S3 PR
// body for this as an explicitly flagged follow-up.
func (s *Store) EraseSubject(ctx context.Context, tenant, subject string, now time.Time) (ErasureResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ErasureResult{}, fmt.Errorf("store: begin erasure transaction: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	var (
		existingErasedAt *time.Time
		existingModeRaw  string
	)
	err = tx.QueryRow(ctx, `
		SELECT erased_at, erasure_mode FROM subjects WHERE tenant = $1 AND subject = $2
	`, tenant, subject).Scan(&existingErasedAt, &existingModeRaw)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErasureResult{}, ErrNotFound
		}
		return ErasureResult{}, fmt.Errorf("store: query subject %s for erasure: %w", subject, err)
	}
	if existingErasedAt != nil {
		return ErasureResult{Mode: ErasureMode(existingModeRaw), ErasedAt: *existingErasedAt, AlreadyErased: true}, nil
	}

	abusive, err := s.HasAbusiveLabel(ctx, tenant, subject)
	if err != nil {
		return ErasureResult{}, err
	}

	mode := ErasureModePurged
	if abusive {
		mode = ErasureModeTombstoned
		if err := tombstoneSubjectTx(ctx, tx, tenant, subject, now); err != nil {
			return ErasureResult{}, err
		}
	} else {
		if err := purgeSubjectTx(ctx, tx, tenant, subject); err != nil {
			return ErasureResult{}, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return ErasureResult{}, fmt.Errorf("store: commit erasure transaction: %w", err)
	}
	return ErasureResult{Mode: mode, ErasedAt: now}, nil
}

// purgeSubjectTx deletes every row for (tenant, subject) outright. Order
// doesn't matter (S1's schema has no foreign keys between these tables —
// see 001_core.sql), but the subjects row is deleted LAST so a caller
// racing this transaction (a worker Tick that already claimed the subject
// before this ran) still finds a real row to fail its own UPDATE against
// rather than silently no-oping into ErrStaleRound-shaped confusion.
func purgeSubjectTx(ctx context.Context, tx pgx.Tx, tenant, subject string) error {
	stmts := []struct {
		table string
		col   string
	}{
		{"events", "subject"},
		{"links", "subject"},
		{"verdicts", "subject"},
		{"rule_state", "subject"},
		{"labels", "subject"},
		{"corpus_examples", "subject"},
	}
	for _, st := range stmts {
		if _, err := tx.Exec(ctx, `DELETE FROM `+st.table+` WHERE tenant = $1 AND `+st.col+` = $2`, tenant, subject); err != nil {
			return fmt.Errorf("store: purge %s for %s: %w", st.table, subject, err)
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM subjects WHERE tenant = $1 AND subject = $2`, tenant, subject); err != nil {
		return fmt.Errorf("store: purge subjects row for %s: %w", subject, err)
	}
	return nil
}

// tombstoneSubjectTx destroys TEXT content for (tenant, subject) while
// preserving numeric features and link hashes (design §4.4). See
// EraseSubject's doc comment for exactly which columns fall in which
// category.
func tombstoneSubjectTx(ctx context.Context, tx pgx.Tx, tenant, subject string, now time.Time) error {
	if _, err := tx.Exec(ctx, `
		UPDATE events SET data = '{}'::jsonb WHERE tenant = $1 AND subject = $2
	`, tenant, subject); err != nil {
		return fmt.Errorf("store: tombstone events for %s: %w", subject, err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE verdicts SET reason = '', llm_reason = NULL WHERE tenant = $1 AND subject = $2
	`, tenant, subject); err != nil {
		return fmt.Errorf("store: tombstone verdicts for %s: %w", subject, err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE corpus_examples SET event_slice = '{}'::jsonb, subject = $3
		WHERE tenant = $1 AND subject = $2
	`, tenant, subject, hashSubjectID(subject)); err != nil {
		return fmt.Errorf("store: tombstone corpus_examples for %s: %w", subject, err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE subjects SET erased_at = $3, erasure_mode = $4 WHERE tenant = $1 AND subject = $2
	`, tenant, subject, now, string(ErasureModeTombstoned)); err != nil {
		return fmt.Errorf("store: mark subject %s erased: %w", subject, err)
	}
	return nil
}
