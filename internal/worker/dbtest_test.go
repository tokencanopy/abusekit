package worker

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tokencanopy/abusekit/internal/store"
)

// This is internal/worker's own small DB-test harness. Unlike
// internal/store's (one schema shared across a whole test BINARY, S11),
// this creates a whole separate throwaway database PER TEST: internal/
// worker's suite is small enough that the overhead doesn't matter, and a
// separate database per test means internal/worker's Tick/ClaimDirtySubjects
// tests — which genuinely care about dirty_seq/scored_seq/current_tier
// bookkeeping — never have to worry about state left over from a sibling
// test in the same run. Duplicating this rather than reusing store's own
// unexported test machinery is deliberate: importing test-only helpers
// across package boundaries isn't possible in Go, and internal/store's
// already-reviewed harness (S1's two review rounds specifically covered its
// concurrency/isolation properties) is left untouched.

const defaultWorkerTestDBURL = "postgres://e2a:e2a@localhost:5433/abusekit_test?sslmode=disable"

func workerTestDBURL() string {
	if u := os.Getenv("ABUSEKIT_TEST_DATABASE_URL"); u != "" {
		return u
	}
	return defaultWorkerTestDBURL
}

func requireDB() bool { return os.Getenv("ABUSEKIT_REQUIRE_DB") == "1" }

func unavailable(t *testing.T, format string, args ...any) {
	t.Helper()
	if requireDB() {
		t.Fatalf(format, args...)
	}
	t.Skipf(format, args...)
}

// newTestStore opens a throwaway, migrated database dedicated to one test.
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping DB-backed test in -short mode")
	}
	ctx := context.Background()
	base := workerTestDBURL()

	probe, err := pgxpool.New(ctx, base)
	if err != nil {
		unavailable(t, "test database not available: %v", err)
		return nil
	}
	pingErr := probe.Ping(ctx)
	probe.Close()
	if pingErr != nil {
		var pgErr *pgconn.PgError
		if !errors.As(pingErr, &pgErr) || pgErr.Code != "3D000" { // invalid_catalog_name: base db missing, server reachable
			unavailable(t, "test database not available: %v", pingErr)
			return nil
		}
	}

	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse test db url: %v", err)
	}
	name := fmt.Sprintf("abusekit_worker_%d_%d", os.Getpid(), time.Now().UnixNano())
	fresh := *u
	fresh.Path = "/" + name
	dbURL := fresh.String()

	if err := createDatabase(ctx, base, name); err != nil {
		t.Fatalf("create throwaway database %s: %v", name, err)
	}
	t.Cleanup(func() { _ = dropDatabase(context.Background(), base, name) })

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("open pool for %s: %v", name, err)
	}
	t.Cleanup(pool.Close)

	s := store.New(pool)
	if err := s.ApplyMigrations(ctx); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return s
}

func createDatabase(ctx context.Context, baseURL, name string) error {
	target, err := url.Parse(baseURL)
	if err != nil {
		return err
	}
	admin := *target
	admin.Path = "/postgres"
	conn, err := pgx.Connect(ctx, admin.String())
	if err != nil {
		return fmt.Errorf("connect admin db: %w", err)
	}
	defer conn.Close(ctx)
	_, err = conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize())
	return err
}

func dropDatabase(ctx context.Context, baseURL, name string) error {
	target, err := url.Parse(baseURL)
	if err != nil {
		return err
	}
	admin := *target
	admin.Path = "/postgres"
	conn, err := pgx.Connect(ctx, admin.String())
	if err != nil {
		return fmt.Errorf("connect admin db: %w", err)
	}
	defer conn.Close(ctx)
	_, err = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	return err
}
