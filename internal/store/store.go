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

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tokencanopy/abusekit/internal/store/migrations"
)

// migrationLockKey is the bigint key ApplyMigrations passes to
// pg_advisory_xact_lock (B4) to serialize concurrent migration runs.
// Postgres advisory locks share one 64-bit keyspace per database
// cluster-wide, so this is a fixed, arbitrary constant rather than
// anything derived from abusekit's own data — any int64 works as long as
// every abusekit instance agrees on it, which a literal constant
// guarantees for free.
const migrationLockKey = 0x61627573656b6974 // "abusekit" in ASCII hex, truncated to fit int64

// undefinedTable is the Postgres SQLSTATE for "undefined_table" (42P01):
// the only error ApplyMigrations' tracker-lookup treats as "the tracker
// table doesn't exist yet on a brand-new database" rather than a real
// failure to propagate.
const undefinedTable = "42P01"

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

// Close releases the underlying pool's connections. Callers that
// constructed a Store for a process's lifetime (cmd/abusekit's `serve`
// and `migrate`) should defer this right after a successful New.
//
// N8: this replaces an earlier exported Pool() method — nothing outside
// this package needed the raw *pgxpool.Pool for anything other than
// closing it, so the narrower Close() is the whole surface a caller
// actually needs, and it doesn't leak pgx's own types into callers that
// otherwise have no reason to import pgx directly.
func (s *Store) Close() { s.pool.Close() }

// ErrNotFound is returned by read methods (SubjectView) when the
// requested row does not exist. Callers use errors.Is against this rather
// than a package-specific sentinel per method.
var ErrNotFound = errors.New("store: not found")

// ApplyMigrations runs every embedded .sql file (in filename order) that
// isn't already recorded in schema_migrations_abusekit. Each file is
// itself idempotent (CREATE ... IF NOT EXISTS), so re-running a migration
// that partially applied before a crash is safe; the tracker just avoids
// re-executing everything on every boot.
//
// Safe to call concurrently from multiple instances at startup (B4): the
// whole run happens inside one transaction serialized against every other
// concurrent caller's transaction by pg_advisory_xact_lock(migrationLockKey)
// — Postgres blocks the second caller until the first commits or rolls
// back, so two instances can never both observe "not yet applied" for the
// same migration and race their own CREATE TABLE/CREATE INDEX statements
// against each other. Proven without the lock: ~35-39/40 concurrent calls
// against a fresh database failed with a duplicate pg_type key.
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

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin migration transaction: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	// Blocks until acquired; auto-released on commit or rollback, so there
	// is no separate unlock call.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLockKey); err != nil {
		return fmt.Errorf("store: acquire migration lock: %w", err)
	}

	for _, name := range names {
		body, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			return fmt.Errorf("store: read migration %s: %w", name, err)
		}

		// Postgres aborts an ENTIRE transaction on any statement error
		// (even one this code expects and handles below) until a ROLLBACK
		// or ROLLBACK TO SAVEPOINT runs — so the tracker-existence check
		// (which errors with 42P01 exactly once, on a brand-new database
		// before this loop's first CREATE TABLE runs) needs its own
		// savepoint to avoid poisoning every statement after it in this
		// migration transaction.
		if _, err := tx.Exec(ctx, "SAVEPOINT check_tracker"); err != nil {
			return fmt.Errorf("store: create savepoint before checking tracker for %s: %w", name, err)
		}
		var applied bool
		row := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM schema_migrations_abusekit WHERE version = $1)`, name)
		if err := row.Scan(&applied); err != nil {
			// The ONLY error this tolerates is "the tracker table itself
			// doesn't exist yet" (42P01 undefined_table). Any other error
			// (a transient connection failure, a permissions problem, a
			// typo'd table name) propagates rather than being silently
			// treated as "nothing applied yet", which would otherwise mask
			// a real failure as a fresh install.
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != undefinedTable {
				return fmt.Errorf("store: check migration tracker for %s: %w", name, err)
			}
			if _, rbErr := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT check_tracker"); rbErr != nil {
				return fmt.Errorf("store: rollback to savepoint for %s: %w", name, rbErr)
			}
			applied = false
		} else if _, err := tx.Exec(ctx, "RELEASE SAVEPOINT check_tracker"); err != nil {
			return fmt.Errorf("store: release savepoint for %s: %w", name, err)
		}
		if applied {
			continue
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			return fmt.Errorf("store: apply migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations_abusekit (version) VALUES ($1) ON CONFLICT DO NOTHING`,
			name,
		); err != nil {
			return fmt.Errorf("store: record migration %s: %w", name, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit migration transaction: %w", err)
	}
	return nil
}
