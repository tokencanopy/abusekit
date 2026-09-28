package store

import (
	"context"
	"fmt"
	"time"
)

// DefaultListLimit and MaxListLimit bound GET /v1/subjects' page size
// (design §4.4: "limit≤100").
const (
	DefaultListLimit = 50
	MaxListLimit     = 100
)

// SubjectListItem is one row of a ListSubjects page: the same materialized
// columns SubjectView reads off subjects.current_*, without per-rule
// Signals (design §4.4's list endpoint is a summary view; a caller wanting
// signals for one subject calls GET /v1/subjects/{subject}).
type SubjectListItem struct {
	Subject string
	Tier    string
	Score   float64
	// VerdictID is 0 for a never-scored subject.
	VerdictID int64
	// ScoredAt is nil for a never-scored subject.
	ScoredAt *time.Time
}

// ListCursor positions a ListSubjects page: the (scored_at, subject) of
// the last row on the previous page, in the SAME stable sort ListSubjects
// itself uses (scored_at DESC, subject ASC — design §4.4's keyset
// pagination). ScoredAt nil means the cursor row was itself never scored
// (sorts after every real timestamp) — this must stay a distinct "no
// value" rather than any sentinel finite time.Time, because the query
// below needs to reproduce Postgres's own `COALESCE(..., '-infinity')`
// exactly on BOTH sides of the keyset comparison for the two to agree.
type ListCursor struct {
	ScoredAt *time.Time
	Subject  string
}

// ListSubjectsOptions filters and pages a ListSubjects call. Tier and
// Class, when non-empty, restrict to an exact match (design §4.4:
// "tier=&class="). Since, when non-zero, restricts to subjects scored at
// or after it — an addition beyond design §4.4's own query parameters,
// requested for this slice's HTTP surface directly (an operator wanting
// "what changed since I last looked" without walking the whole list).
// Limit <= 0 uses DefaultListLimit; > MaxListLimit is clamped down to it.
type ListSubjectsOptions struct {
	Tier   string
	Class  string
	Since  time.Time
	Cursor *ListCursor
	Limit  int
}

// ListSubjects returns one page of (tenant)'s subjects, sorted by
// current_scored_at DESC (a never-scored subject, current_scored_at NULL,
// sorts last), subject ASC as a stable tiebreak (design §4.4). The
// returned cursor is nil when this was the last page.
func (s *Store) ListSubjects(ctx context.Context, tenant string, opts ListSubjectsOptions) ([]SubjectListItem, *ListCursor, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = DefaultListLimit
	}
	if limit > MaxListLimit {
		limit = MaxListLimit
	}

	// coalescedScoredAt gives every row a comparable sort key: a real
	// current_scored_at value, or the sentinel '-infinity' for a
	// never-scored subject, so it sorts LAST under DESC ordering without a
	// separate NULLS-handling branch in either the ORDER BY or the keyset
	// predicate below.
	const coalescedScoredAt = `COALESCE(current_scored_at, '-infinity'::timestamptz)`

	query := `
		SELECT subject, current_tier, current_score, current_verdict_id, current_scored_at
		FROM subjects
		WHERE tenant = $1
	`
	args := []any{tenant}
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}

	if opts.Tier != "" {
		query += " AND current_tier = " + arg(opts.Tier)
	}
	if opts.Class != "" {
		query += " AND class = " + arg(opts.Class)
	}
	if !opts.Since.IsZero() {
		query += " AND " + coalescedScoredAt + " >= " + arg(opts.Since)
	}
	if opts.Cursor != nil {
		// $N::timestamptz binds NULL when opts.Cursor.ScoredAt is nil (pgx
		// encodes a nil *time.Time as SQL NULL), and COALESCE(...,
		// '-infinity') on THIS parameter reproduces exactly the same
		// sentinel the row-side coalescedScoredAt uses for a never-scored
		// row — the two must agree bit-for-bit for the tiebreak below to
		// ever hit its "equal" branch for a page boundary that fell inside
		// a run of never-scored subjects.
		scoredAtParam := fmt.Sprintf("COALESCE(%s::timestamptz, '-infinity'::timestamptz)", arg(opts.Cursor.ScoredAt))
		subjectArg := arg(opts.Cursor.Subject)
		query += fmt.Sprintf(" AND (%s < %s OR (%s = %s AND subject > %s))",
			coalescedScoredAt, scoredAtParam, coalescedScoredAt, scoredAtParam, subjectArg)
	}

	query += " ORDER BY " + coalescedScoredAt + " DESC, subject ASC LIMIT " + arg(limit+1)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("store: list subjects for tenant %s: %w", tenant, err)
	}
	defer rows.Close()

	var out []SubjectListItem
	for rows.Next() {
		var (
			item      SubjectListItem
			score     *float64
			verdictID *int64
			scoredAt  *time.Time
		)
		if err := rows.Scan(&item.Subject, &item.Tier, &score, &verdictID, &scoredAt); err != nil {
			return nil, nil, fmt.Errorf("store: scan subject list row: %w", err)
		}
		if score != nil {
			item.Score = *score
		}
		if verdictID != nil {
			item.VerdictID = *verdictID
		}
		item.ScoredAt = scoredAt
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("store: iterate subject list for tenant %s: %w", tenant, err)
	}

	var next *ListCursor
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = &ListCursor{ScoredAt: last.ScoredAt, Subject: last.Subject}
	}
	return out, next, nil
}
