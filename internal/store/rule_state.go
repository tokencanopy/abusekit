package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// RuleBackoff is one (subject, rule)'s backoff bookkeeping (design §4.8):
// how many consecutive scoring attempts have errored, and — if any have —
// when the worker may try again. A rule that has never errored for this
// subject has no row at all; GetRuleBackoff reports that as the zero
// RuleBackoff (Attempts 0, RetryAt nil), which is also exactly the state
// that never blocks a call.
type RuleBackoff struct {
	Attempts  int
	RetryAt   *time.Time
	LastError string
}

// InBackoff reports whether now is still within the recorded retry
// window — i.e. the worker should NOT call this (subject, rule)'s scorer
// yet and should instead record the round as unscored ("backoff").
func (b RuleBackoff) InBackoff(now time.Time) bool {
	return b.RetryAt != nil && now.Before(*b.RetryAt)
}

// GetRuleBackoff returns the recorded backoff state for (tenant, subject,
// rule), or the zero RuleBackoff if no row exists (the common case: a rule
// that has never errored for this subject).
func (s *Store) GetRuleBackoff(ctx context.Context, tenant, subject, rule string) (RuleBackoff, error) {
	var b RuleBackoff
	err := s.pool.QueryRow(ctx, `
		SELECT attempts, retry_at, last_error FROM rule_state
		WHERE tenant = $1 AND subject = $2 AND rule = $3
	`, tenant, subject, rule).Scan(&b.Attempts, &b.RetryAt, &b.LastError)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RuleBackoff{}, nil
		}
		return RuleBackoff{}, fmt.Errorf("store: get rule backoff for %s/%s: %w", subject, rule, err)
	}
	return b, nil
}

// RecordRuleError upserts (subject, rule)'s backoff state after a scorer
// call has errored: attempts increments by one, retry_at is set to the
// caller-computed next retry instant (design §4.8's 30s/2m/5m/15m-capped
// schedule — internal/worker owns that schedule, not this method, so a
// policy change never touches store), and last_error records the error
// for operator visibility.
func (s *Store) RecordRuleError(ctx context.Context, tenant, subject, rule string, retryAt time.Time, lastError string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO rule_state (tenant, subject, rule, attempts, retry_at, last_error, updated_at)
		VALUES ($1, $2, $3, 1, $4, $5, now())
		ON CONFLICT (tenant, subject, rule) DO UPDATE SET
			attempts   = rule_state.attempts + 1,
			retry_at   = EXCLUDED.retry_at,
			last_error = EXCLUDED.last_error,
			updated_at = now()
	`, tenant, subject, rule, retryAt, lastError)
	if err != nil {
		return fmt.Errorf("store: record rule error for %s/%s: %w", subject, rule, err)
	}
	return nil
}

// ClearRuleBackoff deletes (subject, rule)'s backoff row after a
// successful score, so a recovered rule doesn't carry a stale attempts
// count or retry_at forward into its next error. Deleting a row that
// doesn't exist (the common case: the rule never errored) is a no-op, not
// an error.
func (s *Store) ClearRuleBackoff(ctx context.Context, tenant, subject, rule string) error {
	_, err := s.pool.Exec(ctx, `
		DELETE FROM rule_state WHERE tenant = $1 AND subject = $2 AND rule = $3
	`, tenant, subject, rule)
	if err != nil {
		return fmt.Errorf("store: clear rule backoff for %s/%s: %w", subject, rule, err)
	}
	return nil
}
