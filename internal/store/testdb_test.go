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
// same shared local Postgres convention e2a's own test suite uses, just a
// different database name so the two repos' test suites never collide on
// the same one.
const defaultTestDBURL = "postgres://e2a:e2a@localhost:5433/abusekit_test?sslmode=disable"

// testDBURL returns the configured test database URL: the
// ABUSEKIT_TEST_DATABASE_URL override, or defaultTestDBURL.
func testDBURL() string {
	if u := os.Getenv("ABUSEKIT_TEST_DATABASE_URL"); u != "" {
		return u
	}
	return defaultTestDBURL
}

// requireDB is S11: CI sets ABUSEKIT_REQUIRE_DB=1 so a DB-backed test
// FAILS (t.Fatalf) instead of silently SKIPPING when the configured
// Postgres server is unreachable — a skip is the right default for a
// laptop with no local Postgres running, but is exactly the wrong
// behavior in CI, where "no database reachable" should fail the build
// loudly rather than quietly report a smaller, green suite.
func requireDB() bool {
	return os.Getenv("ABUSEKIT_REQUIRE_DB") == "1"
}

// unavailable reports (via t.Fatalf under requireDB(), t.Skipf
// otherwise) that the test database could not be reached, and returns —
// the caller should return immediately after calling this.
func unavailable(t *testing.T, format string, args ...any) {
	t.Helper()
	if requireDB() {
		t.Fatalf(format, args...)
	}
	t.Skipf(format, args...)
}

// runSchema is a unique Postgres schema name generated once per test
// binary process (S11), shared by every newTestStore call in this run:
// each parallel/concurrent test run (a CI job, a worktree, another
// agent's session) against the SAME shared Postgres server gets its own
// isolated set of tables, so one run's TRUNCATE (see newTestStore) can
// never race another run's INSERTs on the literal same table. A single
// process's own tests still safely share this one schema and truncate it
// between each other, same as before S11.
var runSchema = fmt.Sprintf("abusekit_run_%d_%d", os.Getpid(), time.Now().UnixNano())

// TestMain creates runSchema once before any test in this package runs
// (best-effort: if the database isn't reachable, individual tests report
// that themselves via newTestStore/newThrowawayDatabaseURL) and drops it
// once after every test has finished, so a long-lived shared Postgres
// instance doesn't accumulate one abandoned schema per test run.
func TestMain(m *testing.M) {
	ctx := context.Background()
	haveSchema := createSchema(ctx, testDBURL(), runSchema) == nil

	// os.Exit does not run deferred functions, so the drop must happen
	// BEFORE calling it, not via a defer here.
	code := m.Run()
	if haveSchema {
		_ = dropSchema(context.Background(), testDBURL(), runSchema)
	}
	os.Exit(code)
}

func createSchema(ctx context.Context, dbURL, schema string) error {
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	_, err = conn.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+pgx.Identifier{schema}.Sanitize())
	return err
}

func dropSchema(ctx context.Context, dbURL, schema string) error {
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	_, err = conn.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	return err
}

// scopedPoolConfig returns a pgxpool.Config for dbURL whose every
// connection sets search_path to ONLY runSchema — deliberately with NO
// fallback to public. Every pool this test package opens against
// testDBURL() — newTestStore's own, and any verification pool a test
// opens to check what landed in the database — must go through this, not
// a bare pgxpool.New: a pool without it defaults to Postgres's own
// "$user", public search_path and would (depending on what's left over
// in `public` from past runs, before this repo had schema isolation, or
// from another concurrent run) either fail to find the tables at all or,
// worse, silently read/verify against the WRONG schema instead of loudly
// failing.
//
// The no-fallback part is load-bearing, not a simplification: a
// "runSchema, public" search_path would resolve an unqualified table
// reference against runSchema first, but SILENTLY FALL THROUGH to
// public for any table runSchema doesn't (yet) have — which is exactly
// ApplyMigrations' own tracker-existence check on
// schema_migrations_abusekit. A pre-existing public.schema_migrations_abusekit
// (there always is one, from before this schema-isolation existed, or
// from any test run that predates it) would then make a brand-new
// runSchema's tracker check see "001_core.sql already applied" via that
// fallback and skip creating any table in runSchema at all — the schema
// stays completely empty while every query silently keeps hitting
// public's tables, defeating isolation entirely without ever erroring.
// Postgres's built-in types/functions live in pg_catalog, which is
// always implicitly searched regardless of search_path, so dropping the
// "public" fallback costs nothing this schema needs.
func scopedPoolConfig(dbURL string) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		return nil, err
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET search_path TO "+pgx.Identifier{runSchema}.Sanitize())
		return err
	}
	return cfg, nil
}

// openScopedPool opens a pool against testDBURL(), scoped to runSchema
// (see scopedPoolConfig), for a test that needs to verify what a
// newTestStore-backed Store actually wrote — e.g. a column SubjectView
// doesn't expose. Fails the test on any connection error (this is a
// verification pool for a test that has presumably already confirmed the
// database is reachable via newTestStore).
func openScopedPool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	cfg, err := scopedPoolConfig(testDBURL())
	if err != nil {
		t.Fatalf("parse test db url: %v", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open verification pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// newTestStore opens (self-provisioning the database if it doesn't exist
// yet, mirroring e2a's testutil), migrates, and truncates a Store for one
// test, scoped to this process's runSchema (S11) so concurrent test runs
// against the same shared database never see each other's tables. Skips
// the test — or fails it under ABUSEKIT_REQUIRE_DB=1 (S11) — when the
// configured Postgres server itself is unreachable, so `go test ./...`
// stays green on a machine with no local Postgres by default, while CI
// can demand a real one; a database that IS reachable but fails to
// migrate or truncate is always a real failure, never a skip.
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

	poolCfg, err := scopedPoolConfig(dbURL)
	if err != nil {
		t.Fatalf("parse test db url: %v", err)
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		unavailable(t, "test database not available: %v", err)
		return nil
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "3D000" { // invalid_catalog_name: DB missing
			if cerr := createDatabase(ctx, dbURL); cerr != nil {
				t.Fatalf("failed to create test database: %v", cerr)
			}
			pool, err = pgxpool.NewWithConfig(ctx, poolCfg)
			if err != nil {
				t.Fatalf("reopen pool after creating database: %v", err)
			}
			if err := pool.Ping(ctx); err != nil {
				pool.Close()
				t.Fatalf("ping after creating database: %v", err)
			}
		} else {
			unavailable(t, "test database not available: %v", err)
			return nil
		}
	}
	// The schema itself may not exist yet if TestMain's own createSchema
	// couldn't reach the database at process start but it's reachable
	// now (e.g. a slow-starting CI service container) — ensure it here
	// too, cheaply (IF NOT EXISTS), before anything sets search_path to it.
	if _, err := pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+pgx.Identifier{runSchema}.Sanitize()); err != nil {
		pool.Close()
		t.Fatalf("create run schema: %v", err)
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
// Skips the test if the server itself is unreachable — or fails it under
// ABUSEKIT_REQUIRE_DB=1 (S11) — same as newTestStore.
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
		unavailable(t, "test database not available: %v", err)
		return ""
	}
	t.Cleanup(func() {
		_ = dropDatabase(context.Background(), target)
	})
	return target
}

// truncateAll resets every abusekit table between tests. All tables are
// reachable without FK CASCADE concerns since S1's schema has no foreign
// keys between them (see 001_core.sql's comment on current_verdict_id) —
// a plain multi-table TRUNCATE is enough. budget_usage (S7 fix round,
// migrations/007) is included alongside the original eight.
func truncateAll(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `
		TRUNCATE events, links, subjects, verdicts, rule_state, labels, corpus_examples, calibrations, budget_usage
	`)
	return err
}
