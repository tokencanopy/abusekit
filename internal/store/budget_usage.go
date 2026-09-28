package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// IncrementBudgetUsage atomically increments and returns the NEW count for
// (day, dim, key) — internal/worker.Budgets' persisted daily counter (S7
// fix round), so a restart or a second worker instance shares the same
// count instead of each keeping (and silently resetting) its own.
//
// day is a plain "YYYY-MM-DD" UTC calendar day string, not a time.Time:
// internal/worker owns what "day" means (UTC, matching design §4.8), this
// method just stores whatever key it's given.
func (s *Store) IncrementBudgetUsage(ctx context.Context, day, dim, key string) (int, error) {
	var count int
	err := s.pool.QueryRow(ctx, `
		INSERT INTO budget_usage (day, dim, key, count, updated_at)
		VALUES ($1, $2, $3, 1, now())
		ON CONFLICT (day, dim, key) DO UPDATE SET
			count      = budget_usage.count + 1,
			updated_at = now()
		RETURNING count
	`, day, dim, key).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("store: increment budget usage (%s/%s/%s): %w", day, dim, key, err)
	}
	return count, nil
}

// GetBudgetUsage returns the current count for (day, dim, key), or 0 if no
// calls have been recorded for it yet (the common case: most adapter/
// subject/tenant combinations never approach any cap).
func (s *Store) GetBudgetUsage(ctx context.Context, day, dim, key string) (int, error) {
	var count int
	err := s.pool.QueryRow(ctx, `
		SELECT count FROM budget_usage WHERE day = $1 AND dim = $2 AND key = $3
	`, day, dim, key).Scan(&count)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("store: get budget usage (%s/%s/%s): %w", day, dim, key, err)
	}
	return count, nil
}
