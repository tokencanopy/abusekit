package store_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tokencanopy/abusekit/internal/store"
)

// defaultTestDBURL matches AGENTS.md/the task brief: "default
// postgres://e2a:e2a@localhost:5433/abusekit_test?sslmode=disable" — the
// same shared local Postgres e2a's own testutil uses (see
// ~/Desktop/e2a/internal/testutil/db.go), just a different database name
// so the two repos' test suites never collide on the same one.
const defaultTestDBURL = "postgres://e2a:e2a@localhost:5433/abusekit_test?sslmode=disable"

// testDBURL returns the configured test database URL: the
// ABUSEKIT_TEST_DATABASE_URL override, or defaultTestDBURL.
func testDBURL() string {
	if u := os.Getenv("ABUSEKIT_TEST_DATABASE_URL"); u != "" {
		return u
	}
	return defaultTestDBURL
}

// newTestStore opens (self-provisioning the database if it doesn't exist
// yet, mirroring e2a's testutil), migrates, and truncates a Store for one
// test. Skips the test — rather than failing it — when the configured
// Postgres server itself is unreachable, so `go test ./...` stays green
// on a machine with no local Postgres; a database that IS reachable but
// fails to migrate or truncate is a real failure, not a skip.
//
// The local Postgres at localhost:5433 is shared with other repos' (and
// other agents') test runs — a flake here should be baselined against a
// clean rerun before it's attributed to this package's code.
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping DB-backed test in -short mode")
	}

	ctx := context.Background()
	dbURL := testDBURL()

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Skipf("test database not available: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "3D000" { // invalid_catalog_name: DB missing
			if cerr := createDatabase(ctx, dbURL); cerr != nil {
				t.Fatalf("failed to create test database: %v", cerr)
			}
			pool, err = pgxpool.New(ctx, dbURL)
			if err != nil {
				t.Fatalf("reopen pool after creating database: %v", err)
			}
			if err := pool.Ping(ctx); err != nil {
				pool.Close()
				t.Fatalf("ping after creating database: %v", err)
			}
		} else {
			t.Skipf("test database not available: %v", err)
		}
	}

	s := store.New(pool)
	if err := s.ApplyMigrations(ctx); err != nil {
		pool.Close()
		t.Fatalf("apply migrations: %v", err)
	}
	if err := truncateAll(ctx, pool); err != nil {
		pool.Close()
		t.Fatalf("truncate tables: %v", err)
	}

	t.Cleanup(func() {
		_ = truncateAll(context.Background(), pool)
		pool.Close()
	})

	return s
}

// createDatabase creates dbURL's database via the server, using the
// server's default "postgres" maintenance database as the admin
// connection target. A concurrent creator racing us is treated as
// success (42P04 duplicate_database / 23505 unique violation), matching
// e2a's testutil.
func createDatabase(ctx context.Context, dbURL string) error {
	target, err := url.Parse(dbURL)
	if err != nil {
		return fmt.Errorf("parse test db url: %w", err)
	}
	name := strings.TrimPrefix(target.Path, "/")
	if name == "" {
		return fmt.Errorf("no database name in %s", dbURL)
	}

	admin := *target
	admin.Path = "/postgres"
	conn, err := pgx.Connect(ctx, admin.String())
	if err != nil {
		return fmt.Errorf("connect admin db to create %s: %w", name, err)
	}
	defer conn.Close(ctx)

	if _, err := conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && (pgErr.Code == "42P04" || pgErr.Code == "23505") {
			return nil
		}
		return fmt.Errorf("create database %s: %w", name, err)
	}
	return nil
}

// dropDatabase drops dbURL's database via the server's "postgres"
// maintenance database, forcibly disconnecting any remaining sessions
// first (WITH (FORCE), Postgres 13+) so a caller doesn't have to
// carefully sequence pool.Close() before this runs. Best-effort: a
// caller-side cleanup that fails to drop a throwaway database is a (rare)
// leaked scratch database on the test server, not a test failure.
func dropDatabase(ctx context.Context, dbURL string) error {
	target, err := url.Parse(dbURL)
	if err != nil {
		return fmt.Errorf("parse test db url: %w", err)
	}
	name := strings.TrimPrefix(target.Path, "/")
	if name == "" {
		return fmt.Errorf("no database name in %s", dbURL)
	}

	admin := *target
	admin.Path = "/postgres"
	conn, err := pgx.Connect(ctx, admin.String())
	if err != nil {
		return fmt.Errorf("connect admin db to drop %s: %w", name, err)
	}
	defer conn.Close(ctx)

	_, err = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	return err
}

// newThrowawayDatabaseURL creates a uniquely named, empty database on the
// same Postgres server as testDBURL() and returns its connection URL plus
// a cleanup func that drops it. Unlike newTestStore (which reuses one
// shared database across a whole test run — see S11's per-run schema),
// this is for tests that specifically need to observe behavior against a
// database with NOTHING in it yet, such as a from-scratch migration race.
//
// Skips the test if the server itself is unreachable, same as
// newTestStore.
func newThrowawayDatabaseURL(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping DB-backed test in -short mode")
	}

	base := testDBURL()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse test db url: %v", err)
	}
	name := fmt.Sprintf("abusekit_throwaway_%d_%d", os.Getpid(), time.Now().UnixNano())
	fresh := *u
	fresh.Path = "/" + name
	target := fresh.String()

	if err := createDatabase(context.Background(), target); err != nil {
		t.Skipf("test database not available: %v", err)
	}
	t.Cleanup(func() {
		_ = dropDatabase(context.Background(), target)
	})
	return target
}

// truncateAll resets every abusekit table between tests. All eight tables
// are reachable without FK CASCADE concerns since S1's schema has no
// foreign keys between them (see 001_core.sql's comment on
// current_verdict_id) — a plain multi-table TRUNCATE is enough.
func truncateAll(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `
		TRUNCATE events, links, subjects, verdicts, rule_state, labels, corpus_examples, calibrations
	`)
	return err
}
