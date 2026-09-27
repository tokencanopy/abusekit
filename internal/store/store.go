// Package store is abusekit's Postgres repository (design §4.11): the
// events/links/subjects/verdicts/rule_state/labels/corpus_examples/
// calibrations tables and the methods S1 needs on top of them. It is the
// only package that imports pgx — nothing else in abusekit talks to
// Postgres directly.
package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tokencanopy/abusekit/internal/store/migrations"
)

// Store wraps a pgxpool.Pool with abusekit's repository methods. The zero
// value is not usable; construct with New.
type Store struct {
	pool *pgxpool.Pool
}

// New wraps an already-configured pool. It does not apply migrations or
// otherwise touch the database — call ApplyMigrations explicitly (cmd's
// `migrate` subcommand and test setup both do this).
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Pool exposes the underlying pool for callers that need it directly
// (transactions spanning multiple Store calls, health checks).
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// ErrNotFound is returned by read methods (SubjectView) when the
// requested row does not exist. Callers use errors.Is against this rather
// than a package-specific sentinel per method.
var ErrNotFound = errors.New("store: not found")

// IsNoRows reports whether err is pgx's no-rows sentinel — a convenience
// so callers outside this package don't need to import pgx just to check.
func IsNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// ApplyMigrations runs every embedded .sql file (in filename order) that
// isn't already recorded in schema_migrations_abusekit. Each file is
// itself idempotent (CREATE ... IF NOT EXISTS), so re-running a migration
// that partially applied before a crash is safe; the tracker just avoids
// re-executing everything on every boot.
//
// Safe to call concurrently from multiple instances at startup: two
// instances racing to apply the same not-yet-tracked migration will both
// run CREATE TABLE IF NOT EXISTS harmlessly, and the second INSERT ...
// (see below) is a no-op conflict.
func (s *Store) ApplyMigrations(ctx context.Context) error {
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("store: read embedded migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if name := e.Name(); len(name) > 4 && name[len(name)-4:] == ".sql" {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	for _, name := range names {
		body, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			return fmt.Errorf("store: read migration %s: %w", name, err)
		}

		var applied bool
		row := s.pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM schema_migrations_abusekit WHERE version = $1)`, name)
		if err := row.Scan(&applied); err != nil {
			// The tracker table itself doesn't exist yet on a fresh
			// database — migration 001 creates it. Fall through and run
			// unconditionally; the INSERT below then creates the record.
			applied = false
		}
		if applied {
			continue
		}
		if _, err := s.pool.Exec(ctx, string(body)); err != nil {
			return fmt.Errorf("store: apply migration %s: %w", name, err)
		}
		if _, err := s.pool.Exec(ctx,
			`INSERT INTO schema_migrations_abusekit (version) VALUES ($1) ON CONFLICT DO NOTHING`,
			name,
		); err != nil {
			return fmt.Errorf("store: record migration %s: %w", name, err)
		}
	}
	return nil
}
