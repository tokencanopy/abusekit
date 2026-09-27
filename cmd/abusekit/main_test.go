package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller failed")
	}
	// this file: <root>/cmd/abusekit/main_test.go
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

func shippedConfig(t *testing.T) serveConfig {
	root := repoRoot(t)
	return serveConfig{
		rulesPath:   filepath.Join(root, "config", "rules.yaml"),
		vendorsPath: filepath.Join(root, "config", "vendors.yaml"),
		weightsPath: filepath.Join(root, "config", "local_weights.yaml"),
	}
}

func TestParseServeFlags_RequiresDatabaseURL(t *testing.T) {
	t.Setenv("ABUSEKIT_DATABASE_URL", "")
	if _, err := parseServeFlags(nil); err == nil {
		t.Fatalf("expected an error with no database URL configured")
	}
}

func TestParseServeFlags_Defaults(t *testing.T) {
	t.Setenv("ABUSEKIT_DATABASE_URL", "postgres://example/db")
	c, err := parseServeFlags(nil)
	if err != nil {
		t.Fatalf("parseServeFlags: %v", err)
	}
	if c.databaseURL != "postgres://example/db" {
		t.Errorf("databaseURL = %q", c.databaseURL)
	}
	if c.rulesPath != "config/rules.yaml" || c.vendorsPath != "config/vendors.yaml" || c.weightsPath != "config/local_weights.yaml" {
		t.Errorf("unexpected default paths: %+v", c)
	}
	if c.check {
		t.Errorf("expected check=false by default")
	}
}

func TestParseServeFlags_CheckFlag(t *testing.T) {
	t.Setenv("ABUSEKIT_DATABASE_URL", "postgres://example/db")
	c, err := parseServeFlags([]string{"--check"})
	if err != nil {
		t.Fatalf("parseServeFlags: %v", err)
	}
	if !c.check {
		t.Errorf("expected --check to set check=true")
	}
}

// TestBoot_RejectsMissingRulesFile deliberately leaves c.databaseURL empty
// (S17: boot validates config BEFORE ever connecting to Postgres, so a
// missing rules file fails without needing a database at all — this test
// runs fine even under -short).
func TestBoot_RejectsMissingRulesFile(t *testing.T) {
	c := shippedConfig(t)
	c.rulesPath = filepath.Join(t.TempDir(), "does-not-exist.yaml")
	if _, _, err := boot(context.Background(), c); err == nil {
		t.Fatalf("expected boot to fail with a missing rules file")
	}
}

// TestBoot_RejectsInvalidRules: same S17 point as above — no database
// needed to reject a rules file referencing an unregistered scorer.
func TestBoot_RejectsInvalidRules(t *testing.T) {
	c := shippedConfig(t)
	bad := filepath.Join(t.TempDir(), "bad-rules.yaml")
	if err := os.WriteFile(bad, []byte("tiers: {medium: 0.4, high: 0.8}\nrules:\n  - name: r\n    mode: advise\n    scorer: does_not_exist\n    inputs: [subject_age_h]\n    labels: [benign, abusive]\n    benign_label: benign\n    threshold: 0.5\n"), 0o644); err != nil {
		t.Fatalf("write bad rules file: %v", err)
	}
	c.rulesPath = bad
	if _, _, err := boot(context.Background(), c); err == nil {
		t.Fatalf("expected boot to fail on a rules file referencing an unregistered scorer")
	}
}

// TestRunServe_RequiresCheckFlag is S17: `serve` without --check is no
// longer a blocking placeholder loop — it's an explicit "not implemented
// yet" error, before ever touching a database (the bogus --database-url
// here is never dialed).
func TestRunServe_RequiresCheckFlag(t *testing.T) {
	c := shippedConfig(t)
	args := []string{
		"--database-url", "postgres://unused/should-never-be-dialed",
		"--rules", c.rulesPath,
		"--vendors", c.vendorsPath,
		"--weights", c.weightsPath,
	}
	err := runServe(args)
	if err == nil {
		t.Fatalf("expected an error when --check is not passed")
	}
	if !strings.Contains(err.Error(), "--check") {
		t.Fatalf("expected the error to mention --check, got: %v", err)
	}
}

// TestRunServe_CheckSucceeds is S17's end-to-end path: connect, migrate,
// validate the shipped config, and return with no error and no blocking —
// including closing the store's pool on the way out (defer'd inside
// runServe; a leaked pool would still let this test pass but would show
// up as a lingering connection in a longer-running suite).
func TestRunServe_CheckSucceeds(t *testing.T) {
	c := shippedConfig(t)
	dbURL := testDBURL(t)
	args := []string{
		"--check",
		"--database-url", dbURL,
		"--rules", c.rulesPath,
		"--vendors", c.vendorsPath,
		"--weights", c.weightsPath,
	}
	if err := runServe(args); err != nil {
		t.Fatalf("runServe --check: %v", err)
	}
}

// TestBoot_ShippedConfigSucceeds is the closest thing S1 has to an
// integration test of the whole `serve --check` path: connect, migrate,
// register `local`, load the real config/*.yaml this repo ships. Skips
// when no local Postgres is reachable, same as internal/store's tests.
func TestBoot_ShippedConfigSucceeds(t *testing.T) {
	c := shippedConfig(t)
	c.databaseURL = testDBURL(t)

	s, cfg, err := boot(context.Background(), c)
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	defer s.Pool().Close()

	if len(cfg.Rules) == 0 {
		t.Fatalf("expected at least one loaded rule")
	}
}

// TestMigrate_RespectsCancelledContext is S17: `migrate` now threads a
// signal.NotifyContext-derived ctx through to ApplyMigrations (instead of
// a bare context.Background()) so SIGINT/SIGTERM actually abort a
// long-running migration rather than being ignored until it finishes.
// runMigrateWithContext is the same code runMigrate wraps with
// signal.NotifyContext; passing an already-cancelled context directly
// proves the wiring reaches pgx (which aborts a query started against a
// done context) rather than being silently dropped somewhere on the way.
func TestMigrate_RespectsCancelledContext(t *testing.T) {
	dbURL := testDBURL(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runMigrateWithContext(ctx, dbURL); err == nil {
		t.Fatalf("expected migrate to fail when its context is already cancelled")
	}
}

// testDBURL skips the test under -short — or fails it under
// ABUSEKIT_REQUIRE_DB=1 (S11, same convention as internal/store's tests)
// — when the configured Postgres server (ABUSEKIT_TEST_DATABASE_URL) is
// not reachable, and returns its URL otherwise.
func testDBURL(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping DB-backed test in -short mode")
	}
	dbURL := os.Getenv("ABUSEKIT_TEST_DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://e2a:e2a@localhost:5433/abusekit_test?sslmode=disable"
	}
	pool, err := pgxpool.New(context.Background(), dbURL)
	if err != nil {
		reportDBUnavailable(t, "test database not available: %v", err)
		return ""
	}
	defer pool.Close()
	if err := pool.Ping(context.Background()); err != nil {
		reportDBUnavailable(t, "test database not available: %v", err)
		return ""
	}
	return dbURL
}

// reportDBUnavailable is S11: CI sets ABUSEKIT_REQUIRE_DB=1 so this fails
// the test instead of skipping it when the configured Postgres server is
// unreachable — the same convention as internal/store's tests.
func reportDBUnavailable(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("ABUSEKIT_REQUIRE_DB") == "1" {
		t.Fatalf(format, args...)
	}
	t.Skipf(format, args...)
}
