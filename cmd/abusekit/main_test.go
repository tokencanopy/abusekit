package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tokencanopy/abusekit/internal/worker"
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

// shippedConfig builds a serveConfig against this repo's own config/*.yaml
// files, including config/keys.yaml — whose secrets are all prefixed
// "dev-only-" specifically so a boot against them requires --dev (B2 fix
// round); shippedConfig always sets dev: true so callers get the old
// "just works against the shipped config" behavior unless a test
// specifically wants to exercise the refusal itself (see
// TestBoot_RefusesDevOnlySecretsWithoutDevFlag below).
func shippedConfig(t *testing.T) serveConfig {
	root := repoRoot(t)
	return serveConfig{
		rulesPath:   filepath.Join(root, "config", "rules.yaml"),
		vendorsPath: filepath.Join(root, "config", "vendors.yaml"),
		weightsPath: filepath.Join(root, "config", "local_weights.yaml"),
		brandsPath:  filepath.Join(root, "config", "brands.yaml"),
		webmailPath: filepath.Join(root, "config", "webmail.yaml"),
		keysPath:    filepath.Join(root, "config", "keys.yaml"),
		dev:         true,
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
	t.Setenv("ABUSEKIT_KEYS_CONFIG", "/tmp/keys.yaml") // no default (B2) -- must set something to reach the other defaults
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
	if c.brandsPath != "config/brands.yaml" {
		t.Errorf("brandsPath = %q, want the default config/brands.yaml", c.brandsPath)
	}
	if c.brandsExtraPath != "" {
		t.Errorf("brandsExtraPath = %q, want empty by default ([S2b]: no private brand list unless the operator sets one)", c.brandsExtraPath)
	}
	if c.webmailPath != "config/webmail.yaml" {
		t.Errorf("webmailPath = %q, want the default config/webmail.yaml ([S2b])", c.webmailPath)
	}
	if c.listenAddr != "127.0.0.1:8080" {
		t.Errorf("listenAddr = %q, want 127.0.0.1:8080 by default (B2 fix round: loopback, not every interface)", c.listenAddr)
	}
	if c.check {
		t.Errorf("expected check=false by default")
	}
	if c.dev {
		t.Errorf("expected dev=false by default (B2 fix round)")
	}
	if c.metricsListen != "127.0.0.1:9099" {
		t.Errorf("metricsListen = %q, want the default 127.0.0.1:9099 (R8 round 2)", c.metricsListen)
	}
}

// TestParseServeFlags_RequiresKeysPath is B2: no default keys path — a
// silently-defaulted config/keys.yaml is exactly the fail-open behavior
// this guards against.
func TestParseServeFlags_RequiresKeysPath(t *testing.T) {
	t.Setenv("ABUSEKIT_DATABASE_URL", "postgres://example/db")
	t.Setenv("ABUSEKIT_KEYS_CONFIG", "")
	if _, err := parseServeFlags(nil); err == nil {
		t.Fatalf("expected an error with no keys path configured")
	}
}

// TestBoot_RefusesDevOnlySecretsWithoutDevFlag is B2: config/keys.yaml's
// own secrets are all prefixed "dev-only-" specifically so this refusal
// has something real to catch — proving a caller can't silently boot
// against the shipped dev/test credentials without opting in via --dev.
func TestBoot_RefusesDevOnlySecretsWithoutDevFlag(t *testing.T) {
	c := shippedConfig(t)
	c.dev = false
	if _, _, _, err := boot(context.Background(), c); err == nil {
		t.Fatalf("expected boot to refuse dev-only- prefixed secrets without --dev")
	}
}

// TestBoot_RefusesShortSecrets is B2's unconditional minimum-length floor
// — even under --dev, a secret under minKeySecretBytes is refused.
func TestBoot_RefusesShortSecrets(t *testing.T) {
	c := shippedConfig(t)
	c.dev = true
	short := filepath.Join(t.TempDir(), "short-keys.yaml")
	if err := os.WriteFile(short, []byte("keys:\n  - id: k\n    secret: tooshort\n    tenant: e2a\n    scopes: [read]\n"), 0o644); err != nil {
		t.Fatalf("write short keys file: %v", err)
	}
	c.keysPath = short
	if _, _, _, err := boot(context.Background(), c); err == nil {
		t.Fatalf("expected boot to refuse a secret shorter than %d bytes even under --dev", minKeySecretBytes)
	}
}

func TestParseServeFlags_CheckFlag(t *testing.T) {
	t.Setenv("ABUSEKIT_DATABASE_URL", "postgres://example/db")
	t.Setenv("ABUSEKIT_KEYS_CONFIG", "/tmp/keys.yaml")
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
	if _, _, _, err := boot(context.Background(), c); err == nil {
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
	if _, _, _, err := boot(context.Background(), c); err == nil {
		t.Fatalf("expected boot to fail on a rules file referencing an unregistered scorer")
	}
}

// TestBoot_MergesBrandsExtra is [S2b] F5's "optional second brands file":
// boot merges an operator-private brands-extra file's entries alongside
// the shipped public config/brands.yaml, so a brand ONLY present in the
// private file is still matched.
func TestBoot_MergesBrandsExtra(t *testing.T) {
	c := shippedConfig(t)
	c.databaseURL = testDBURL(t)
	extra := filepath.Join(t.TempDir(), "brands-extra.yaml")
	if err := os.WriteFile(extra, []byte("brands:\n  - name: AcmeCorp Internal Tool\n"), 0o644); err != nil {
		t.Fatalf("write brands-extra file: %v", err)
	}
	c.brandsExtraPath = extra

	_, _, deps, err := boot(context.Background(), c)
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	if !deps.brands.Matches("AcmeCorp Internal Tool Alert") {
		t.Errorf("expected the merged brand set to match the brands-extra-only entry")
	}
	if !deps.brands.Matches("PayPal Support") {
		t.Errorf("expected the merged brand set to still match the public shipped list too")
	}
}

// TestBoot_RejectsMissingWebmailFile is [S2b]'s analogue of
// TestBoot_RejectsMissingRulesFile: a bad --webmail path fails boot before
// ever touching Postgres.
func TestBoot_RejectsMissingWebmailFile(t *testing.T) {
	c := shippedConfig(t)
	c.webmailPath = filepath.Join(t.TempDir(), "does-not-exist.yaml")
	if _, _, _, err := boot(context.Background(), c); err == nil {
		t.Fatalf("expected boot to fail with a missing webmail file")
	}
}

// TestRunServe_CheckSucceeds is S17's end-to-end path: connect, migrate,
// validate the shipped config, and return with no error and no blocking —
// including closing the store's pool on the way out (defer'd inside
// runServeWithContext; a leaked pool would still let this test pass but
// would show up as a lingering connection in a longer-running suite).
func TestRunServe_CheckSucceeds(t *testing.T) {
	c := shippedConfig(t)
	dbURL := testDBURL(t)
	args := []string{
		"--check",
		"--dev",
		"--database-url", dbURL,
		"--rules", c.rulesPath,
		"--vendors", c.vendorsPath,
		"--weights", c.weightsPath,
		"--brands", c.brandsPath,
		"--webmail", c.webmailPath,
		"--keys", c.keysPath,
	}
	if err := runServe(args); err != nil {
		t.Fatalf("runServe --check: %v", err)
	}
}

// TestRunServeWithContext_StartsAndStopsTheWorker is S9 (fix round):
// `serve` without --check now boots and runs internal/worker's scoring
// loop until ctx is done, rather than the old S1 placeholder's "not
// implemented yet" error. A real OS signal would be awkward to test with,
// so this drives runServeWithContext (the same body runServe wraps with
// signal.NotifyContext) with a context this test cancels itself, and
// proves the call returns promptly afterward — Worker.Stop's own
// "cancels and waits" contract is exercised directly in
// internal/worker's own test suite; this is the cmd-level wiring proof.
func TestRunServeWithContext_StartsAndStopsTheWorker(t *testing.T) {
	c := shippedConfig(t)
	c.databaseURL = testDBURL(t)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	done := make(chan error, 1)
	go func() { done <- runServeWithContext(ctx, c) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runServeWithContext: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("runServeWithContext did not return after its context was cancelled")
	}
}

// TestRunServeWithContext_ServesTheAPI is S3's cmd-level wiring proof: with
// a real (OS-assigned, via ":0") listen address, runServeWithContext
// actually answers /healthz over HTTP while running, and the listener is
// gone (connection refused) once its context is cancelled and the call has
// returned — proving startAPIServer's shutdown is wired into the SAME
// graceful-stop path as the worker's own Stop(), not left dangling.
func TestRunServeWithContext_ServesTheAPI(t *testing.T) {
	c := shippedConfig(t)
	c.databaseURL = testDBURL(t)
	c.listenAddr = "127.0.0.1:0"
	c.metricsListen = "" // avoid a second listener neither this test nor its port-discovery needs

	s, cfg, deps, err := boot(context.Background(), c)
	if err != nil {
		t.Fatalf("boot: %v", err)
	}

	w, err := worker.New(worker.Deps{Store: s, Config: cfg, Neighbors: deps.neighbors, Brands: deps.brands, Webmail: deps.webmail})
	if err != nil {
		s.Close()
		t.Fatalf("worker.New: %v", err)
	}
	apiSrv, apiAddr, err := startAPIServer(c.listenAddr, s, w, cfg, deps.keys, deps.neighbors, deps.brands, deps.webmail)
	if err != nil {
		s.Close()
		t.Fatalf("startAPIServer: %v", err)
	}
	defer s.Close()

	resp, err := http.Get("http://" + apiAddr + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := apiSrv.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	if _, err := http.Get("http://" + apiAddr + "/healthz"); err == nil {
		t.Fatalf("expected the listener to be gone after Shutdown")
	}
}

// TestBoot_ShippedConfigSucceeds is the closest thing this repo has to an
// integration test of the whole `serve --check` path: connect, migrate,
// register `local`, load the real config/*.yaml this repo ships. Skips
// when no local Postgres is reachable, same as internal/store's tests.
func TestBoot_ShippedConfigSucceeds(t *testing.T) {
	c := shippedConfig(t)
	c.databaseURL = testDBURL(t)

	s, cfg, deps, err := boot(context.Background(), c)
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	defer s.Close()

	if len(cfg.Rules) == 0 {
		t.Fatalf("expected at least one loaded rule")
	}
	if deps.neighbors == nil {
		t.Errorf("expected boot to construct a Neighbors implementation")
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
