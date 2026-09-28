package serve_test

// Test harness for internal/serve's contract tests: a real, throwaway
// Postgres-backed store, worker, and config — the same "drive it through
// the real seam, not a mock" convention internal/worker's own test suite
// uses (repoRoot/loadShippedConfig/loadShippedBrands there; this package
// needs its own copy since those are unexported in internal/worker).

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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

var runSchema = fmt.Sprintf("abusekit_serve_run_%d_%d", os.Getpid(), time.Now().UnixNano())

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
// into raw wire objects — exactly the shape POST /v1/events expects for
// each item, so a contract test can replay a real, already-calibrated
// fixture over HTTP instead of hand-rolling an event set that might
// silently diverge from what config/local_weights.yaml was actually tuned
// against (internal/worker's own replay tests load the same files this
// way, just straight into event.Event rather than a raw map).
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

// fixtureEventAt parses one fixture line's "at" field.
func fixtureEventAt(t *testing.T, line map[string]any) time.Time {
	t.Helper()
	s, _ := line["at"].(string)
	at, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse fixture event `at` %q: %v", s, err)
	}
	return at
}

// fixtureIsExternalSend reports whether line is a content.sent event with
// recipient_is_own_identity false.
func fixtureIsExternalSend(line map[string]any) bool {
	if line["type"] != "content.sent" {
		return false
	}
	data, _ := line["data"].(map[string]any)
	own, _ := data["recipient_is_own_identity"].(bool)
	return !own
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

// testKeys is a small, fixed key set for contract tests: one events-scoped
// producer key, one all-scopes operator key (labels/read), a second
// tenant's read-scoped key (for cross-tenant-denial tests), a second
// tenant's LABELS-scoped key (S6: cross-tenant denial on the labels
// endpoint specifically, which a read-only key can't exercise since it'd
// 403 on scope before ever reaching the tenant check), and a backfill key.
type testKeys struct {
	Producer     config.Key // tenant e2a, scope events
	Operator     config.Key // tenant e2a, scope read+labels
	Other        config.Key // tenant other-tenant, scope read
	OtherLabels  config.Key // tenant other-tenant, scope labels
	Backfill     config.Key // tenant e2a, scope events+backfill
	BackfillRead config.Key // tenant e2a, scope events+read+backfill (R5, round 2 fix round: proves the widened backfill timestamp window is events-only, not scope-wide)
}

func fixedTestKeys() testKeys {
	return testKeys{
		Producer:     config.Key{ID: "test_producer", Secret: "test-producer-secret", Tenant: testTenant, Producer: "test-producer", Scopes: map[config.Scope]bool{config.ScopeEvents: true}},
		Operator:     config.Key{ID: "test_operator", Secret: "test-operator-secret", Tenant: testTenant, Scopes: map[config.Scope]bool{config.ScopeRead: true, config.ScopeLabels: true}},
		Other:        config.Key{ID: "test_other_tenant", Secret: "test-other-secret", Tenant: "other-tenant", Scopes: map[config.Scope]bool{config.ScopeRead: true}},
		OtherLabels:  config.Key{ID: "test_other_tenant_labels", Secret: "test-other-labels-secret", Tenant: "other-tenant", Scopes: map[config.Scope]bool{config.ScopeLabels: true}},
		Backfill:     config.Key{ID: "test_backfill", Secret: "test-backfill-secret", Tenant: testTenant, Producer: "test-backfiller", Scopes: map[config.Scope]bool{config.ScopeEvents: true, config.ScopeBackfill: true}},
		BackfillRead: config.Key{ID: "test_backfill_read", Secret: "test-backfill-read-secret", Tenant: testTenant, Producer: "test-backfill-read", Scopes: map[config.Scope]bool{config.ScopeEvents: true, config.ScopeRead: true, config.ScopeBackfill: true}},
	}
}

func (k testKeys) asMap() map[string]config.Key {
	return map[string]config.Key{
		k.Producer.ID:     k.Producer,
		k.Operator.ID:     k.Operator,
		k.Other.ID:        k.Other,
		k.OtherLabels.ID:  k.OtherLabels,
		k.Backfill.ID:     k.Backfill,
		k.BackfillRead.ID: k.BackfillRead,
	}
}

// testServer wires a real *serve.Server (and the *httptest.Server serving
// it) against a fresh throwaway store + worker + the shipped rules/vendor/
// weights/brands config, with a controllable clock. Every contract test
// drives the seam through real HTTP requests to ts.URL — codebase-design's
// "tests cross the HTTP seam, not internals".
type testServer struct {
	Store  *store.Store
	Worker *worker.Worker
	Server *serve.Server
	TS     *httptest.Server
	Keys   testKeys
	Now    func() time.Time
}

func newTestServer(t *testing.T, now time.Time) *testServer {
	t.Helper()
	s := newTestStore(t)
	cfg := loadShippedConfig(t)
	brands := loadShippedBrands(t)
	nowFn := func() time.Time { return now }

	w, err := worker.New(worker.Deps{Store: s, Config: cfg, Neighbors: feature.NoNeighbors, Brands: brands, Now: nowFn})
	if err != nil {
		t.Fatalf("worker.New: %v", err)
	}

	keys := fixedTestKeys()
	srv, err := serve.New(serve.Deps{
		Store: s, Worker: w, Config: cfg, Keys: keys.asMap(),
		Neighbors: feature.NoNeighbors, Brands: brands, Now: nowFn,
	})
	if err != nil {
		t.Fatalf("serve.New: %v", err)
	}

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	t.Cleanup(srv.Close)

	return &testServer{Store: s, Worker: w, Server: srv, TS: ts, Keys: keys, Now: nowFn}
}

// httpDo is a tiny convenience wrapper so tests don't repeat transport
// plumbing; contract tests otherwise build requests by hand (headers,
// signing) to exercise the auth seam directly rather than going through
// pkg/abusekit's client, which has its own separate test suite.
func httpDo(t *testing.T, req *http.Request) *http.Response {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("http.Do: %v", err)
	}
	return resp
}

// signBody replicates internal/serve's own (unexported) HMAC signing
// scheme (design §4.3, B1 fix round's nonce) independently, deliberately
// NOT by calling into the package under test — a contract test proves the
// WIRE contract, so it signs the same way an external producer would,
// from this package's own implementation of the documented scheme.
func signBody(secret, method, requestURI, timestamp, keyID, nonce string, body []byte) string {
	bodyHash := sha256.Sum256(body)
	canonical := method + "\n" + requestURI + "\n" + timestamp + "\n" + keyID + "\n" + nonce + "\n" + hex.EncodeToString(bodyHash[:])
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(canonical))
	return hex.EncodeToString(mac.Sum(nil))
}

func mustNonce(t *testing.T) string {
	t.Helper()
	n, err := serve.GenerateNonce()
	if err != nil {
		t.Fatalf("GenerateNonce: %v", err)
	}
	return n
}

// signedRequest builds an http.Request against ts, signed as key would
// sign it at instant `at` (design §4.3's whole scheme, "including GET" —
// a nil body signs the empty string, matching every non-events endpoint),
// with a FRESH random nonce (B1 fix round) — the common case for a test
// that just wants an independently-valid request. A test that needs to
// control or reuse a nonce (replay tests) uses signedRequestWithNonce
// directly.
func signedRequest(t *testing.T, ts *httptest.Server, method, path string, body []byte, key config.Key, at time.Time) *http.Request {
	t.Helper()
	return signedRequestWithNonce(t, ts, method, path, body, key, at, mustNonce(t))
}

// signedRequestWithNonce is signedRequest with an explicit nonce — used by
// replay-protection tests that need TWO requests sharing the same nonce
// (a genuine replay) versus two independently fresh ones (two legitimate
// calls that happen to land in the same wall-clock second).
func signedRequestWithNonce(t *testing.T, ts *httptest.Server, method, path string, body []byte, key config.Key, at time.Time, nonce string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("http.NewRequest: %v", err)
	}
	timestamp := at.UTC().Format(time.RFC3339)
	sig := signBody(key.Secret, method, req.URL.RequestURI(), timestamp, key.ID, nonce, body)
	req.Header.Set(serve.HeaderKey, key.ID)
	req.Header.Set(serve.HeaderTimestamp, timestamp)
	req.Header.Set(serve.HeaderSignature, sig)
	req.Header.Set(serve.HeaderNonce, nonce)
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}
