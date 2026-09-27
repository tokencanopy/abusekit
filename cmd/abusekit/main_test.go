package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
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

func TestBoot_RejectsMissingRulesFile(t *testing.T) {
	c := shippedConfig(t)
	c.databaseURL = testDBURL(t)
	c.rulesPath = filepath.Join(t.TempDir(), "does-not-exist.yaml")
	if _, _, err := boot(context.Background(), c); err == nil {
		t.Fatalf("expected boot to fail with a missing rules file")
	}
}

func TestBoot_RejectsInvalidRules(t *testing.T) {
	c := shippedConfig(t)
	c.databaseURL = testDBURL(t)
	bad := filepath.Join(t.TempDir(), "bad-rules.yaml")
	if err := os.WriteFile(bad, []byte("tiers: {medium: 0.4, high: 0.8}\nrules:\n  - name: r\n    mode: advise\n    scorer: does_not_exist\n    inputs: [subject_age_h]\n    labels: [benign, abusive]\n    benign_label: benign\n    threshold: 0.5\n"), 0o644); err != nil {
		t.Fatalf("write bad rules file: %v", err)
	}
	c.rulesPath = bad
	if _, _, err := boot(context.Background(), c); err == nil {
		t.Fatalf("expected boot to fail on a rules file referencing an unregistered scorer")
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

// testDBURL skips the test under -short (boot() always applies
// migrations before it can even reach a bad rules/vendors file, so every
// test that calls this needs a real database, not just
// TestBoot_ShippedConfigSucceeds) or when the configured Postgres server
// (same ABUSEKIT_TEST_DATABASE_URL convention as internal/store's tests)
// is not reachable, and returns its URL otherwise.
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
		t.Skipf("test database not available: %v", err)
	}
	defer pool.Close()
	if err := pool.Ping(context.Background()); err != nil {
		t.Skipf("test database not available: %v", err)
	}
	return dbURL
}
