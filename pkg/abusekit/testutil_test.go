package abusekit_test

// Test harness for pkg/abusekit's contract tests: a real *serve.Server
// (internal/serve) backed by a throwaway Postgres store and the shipped
// rules/vendor/weights/brands config, served over httptest — "Contract
// tests run the client against an httptest server backed by the real
// handlers," not a client-side mock of the wire format. This harness is
// its own copy of internal/serve's (package-private, so unreachable from
// here) test setup rather than a shared exported helper, matching how
// internal/worker and internal/serve each keep their own copy too.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tokencanopy/abusekit/internal/config"
	"github.com/tokencanopy/abusekit/internal/feature"
	"github.com/tokencanopy/abusekit/internal/model"
	"github.com/tokencanopy/abusekit/internal/model/local"
	"github.com/tokencanopy/abusekit/internal/serve"
	"github.com/tokencanopy/abusekit/internal/store"
	"github.com/tokencanopy/abusekit/internal/worker"
)

const testTenant = "e2a"

const defaultTestDBURL = "postgres://e2a:e2a@localhost:5433/abusekit_test?sslmode=disable"

func testDBURL() string {
	if u := os.Getenv("ABUSEKIT_TEST_DATABASE_URL"); u != "" {
		return u
	}
	return defaultTestDBURL
}

func requireDB() bool { return os.Getenv("ABUSEKIT_REQUIRE_DB") == "1" }

func unavailable(t *testing.T, format string, args ...any) {
	t.Helper()
	if requireDB() {
		t.Fatalf(format, args...)
	}
	t.Skipf(format, args...)
}

var runSchema = fmt.Sprintf("abusekit_pkg_run_%d_%d", os.Getpid(), time.Now().UnixNano())

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
		if errors.As(err, &pgErr) && pgErr.Code == "3D000" {
			if cerr := createDatabase(ctx, dbURL); cerr != nil {
				t.Fatalf("create test database: %v", cerr)
			}
			pool, err = pgxpool.NewWithConfig(ctx, poolCfg)
			if err != nil {
				t.Fatalf("reopen pool: %v", err)
			}
			if err := pool.Ping(ctx); err != nil {
				pool.Close()
				t.Fatalf("ping after create: %v", err)
			}
		} else {
			unavailable(t, "test database not available: %v", err)
			return nil
		}
	}
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
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(func() {
		_ = truncateAll(context.Background(), pool)
		pool.Close()
	})
	return s
}

func truncateAll(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `
		TRUNCATE events, links, subjects, verdicts, rule_state, labels, corpus_examples, calibrations, budget_usage
	`)
	return err
}

// loadFixtureLines parses a JSONL replay fixture (eval/fixtures/*.jsonl)
// into raw wire objects, matching internal/serve's own test helper of the
// same name — reusing the real, already-calibrated fixtures rather than
// hand-rolled event sets that could silently diverge from what
// config/local_weights.yaml was tuned against.
func loadFixtureLines(t *testing.T, filename string) []map[string]any {
	t.Helper()
	path := filepath.Join(repoRoot(t), "eval", "fixtures", filename)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("parse fixture line %q: %v", line, err)
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		t.Fatalf("fixture %s parsed to zero lines", path)
	}
	return out
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

func loadShippedConfig(t *testing.T) *config.Config {
	t.Helper()
	root := repoRoot(t)

	weights, err := local.LoadWeightsFile(filepath.Join(root, "config", "local_weights.yaml"))
	if err != nil {
		t.Fatalf("load weights: %v", err)
	}
	scorer, err := local.New(weights)
	if err != nil {
		t.Fatalf("new local scorer: %v", err)
	}
	reg := model.NewRegistry()
	if err := reg.Register(scorer); err != nil {
		t.Fatalf("register local: %v", err)
	}

	vendorsData, err := os.ReadFile(filepath.Join(root, "config", "vendors.yaml"))
	if err != nil {
		t.Fatalf("read vendors.yaml: %v", err)
	}
	vendors, err := config.LoadVendors(vendorsData)
	if err != nil {
		t.Fatalf("load vendors: %v", err)
	}

	rulesData, err := os.ReadFile(filepath.Join(root, "config", "rules.yaml"))
	if err != nil {
		t.Fatalf("read rules.yaml: %v", err)
	}
	cfg, err := config.Load(rulesData, config.Dependencies{
		Registry: reg,
		Features: config.NewFeatureSet(feature.Names...),
		Vendors:  vendors,
	})
	if err != nil {
		t.Fatalf("load rules.yaml: %v", err)
	}
	return cfg
}

func loadShippedBrands(t *testing.T) feature.BrandSet {
	t.Helper()
	root := repoRoot(t)
	brands, err := feature.LoadBrandsFile(filepath.Join(root, "config", "brands.yaml"))
	if err != nil {
		t.Fatalf("load brands.yaml: %v", err)
	}
	return brands
}

// testKey is the one signed key every pkg/abusekit contract test uses:
// full scopes, so the client's own method surface (not internal/serve's
// authZ matrix, already covered by that package's own suite) is what's
// under test here.
func testKey() config.Key {
	return config.Key{
		ID: "test_client_key", Secret: "test-client-secret", Tenant: testTenant, Producer: "test-client-producer",
		Scopes: map[config.Scope]bool{config.ScopeEvents: true, config.ScopeRead: true, config.ScopeLabels: true},
	}
}

// testHarness wires a real *serve.Server behind httptest, for the client
// under test to talk to.
type testHarness struct {
	Store *store.Store
	TS    *httptest.Server
	Key   config.Key
}

func newTestHarness(t *testing.T, now time.Time) *testHarness {
	t.Helper()
	s := newTestStore(t)
	cfg := loadShippedConfig(t)
	brands := loadShippedBrands(t)
	nowFn := func() time.Time { return now }

	w, err := worker.New(worker.Deps{Store: s, Config: cfg, Neighbors: feature.NoNeighbors, Brands: brands, Now: nowFn})
	if err != nil {
		t.Fatalf("worker.New: %v", err)
	}

	key := testKey()
	srv, err := serve.New(serve.Deps{
		Store: s, Worker: w, Config: cfg, Keys: map[string]config.Key{key.ID: key},
		Neighbors: feature.NoNeighbors, Brands: brands, Now: nowFn,
	})
	if err != nil {
		t.Fatalf("serve.New: %v", err)
	}

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &testHarness{Store: s, TS: ts, Key: key}
}
