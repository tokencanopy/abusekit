// Command abusekit is the single binary that runs abusekit (design §2:
// "One Go binary; Postgres").
//
// S1 wired up the store and config layers behind two subcommands with
// `serve --check` only. S2's fix round (S9) wired up and ran the SCORING
// LOOP itself (internal/worker) under plain `serve` — it finds dirty
// subjects, scores them, and commits verdicts. R8 (round 2) added a
// loopback HTTP listener serving worker.Metrics via expvar. S3 adds the
// `/v1/*` HTTP surface itself (internal/serve): `serve` without --check
// now runs both the worker AND the API, blocking on SIGINT/SIGTERM.
// `serve --check` is unaffected: it still only validates config and
// migrates the database, then exits 0. S4 adds `eval`/`score`/`corpus`
// (eval_cmd.go/score_cmd.go/corpus_cmd.go): the evaluation harness CLI,
// which never touches Postgres except for `corpus export`.
package main

import (
	"context"
	"errors"
	"expvar"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tokencanopy/abusekit/internal/config"
	"github.com/tokencanopy/abusekit/internal/feature"
	"github.com/tokencanopy/abusekit/internal/serve"
	"github.com/tokencanopy/abusekit/internal/store"
	"github.com/tokencanopy/abusekit/internal/worker"
)

// exitError pairs an error with the process exit code main() should use
// for it (api-design: "exit codes 0 for pass, 1 for a floor violation, 2
// for bad input" — eval_cmd.go/score_cmd.go/corpus_cmd.go return one of
// these instead of a plain error whenever the distinction matters). A
// plain error (every pre-S4 code path) still exits 1, unchanged.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

// exitCode1 / exitCode2 build an *exitError for a floor violation / bad
// input, respectively (see the api-design contract above).
func exitCode1(err error) error { return &exitError{code: 1, err: err} }
func exitCode2(err error) error { return &exitError{code: 2, err: err} }

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "abusekit:", err)
		code := 1
		var ec *exitError
		if errors.As(err, &ec) {
			code = ec.code
		}
		os.Exit(code)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: abusekit <serve|migrate|eval|score|corpus> [flags]")
	}
	switch args[0] {
	case "serve":
		return runServe(args[1:])
	case "migrate":
		return runMigrate(args[1:])
	case "eval":
		return runEval(args[1:])
	case "score":
		return runScore(args[1:])
	case "corpus":
		return runCorpus(args[1:])
	default:
		return fmt.Errorf("unknown subcommand %q (want \"serve\", \"migrate\", \"eval\", \"score\", or \"corpus\")", args[0])
	}
}

type serveConfig struct {
	check           bool
	dev             bool
	databaseURL     string
	rulesPath       string
	vendorsPath     string
	weightsPath     string
	brandsPath      string
	brandsExtraPath string
	webmailPath     string
	keysPath        string
	metricsListen   string
	listenAddr      string
}

// minKeySecretBytes and devSecretPrefix are the fix round's B2 guards
// against booting with a fail-open default credential set: config/keys.yaml
// (this repo's own shipped dev/test file) names every secret with
// devSecretPrefix specifically so validateKeySecrets can recognize and
// refuse it outside --dev, and every one of those shipped secrets is
// already >= minKeySecretBytes so --dev alone is enough to run locally
// against the shipped file.
const (
	minKeySecretBytes = 32
	devSecretPrefix   = "dev-only-"
)

func parseServeFlags(args []string) (serveConfig, error) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	var c serveConfig
	fs.BoolVar(&c.check, "check", false, "boot store and config, then exit 0 without serving")
	fs.BoolVar(&c.dev, "dev", false, "allow key secrets prefixed \"dev-only-\" (config/keys.yaml ships these) -- refused otherwise (B2 fix round)")
	fs.StringVar(&c.databaseURL, "database-url", os.Getenv("ABUSEKIT_DATABASE_URL"), "Postgres connection string (env ABUSEKIT_DATABASE_URL)")
	fs.StringVar(&c.rulesPath, "rules", envOr("ABUSEKIT_RULES_CONFIG", "config/rules.yaml"), "path to rules.yaml")
	fs.StringVar(&c.vendorsPath, "vendors", envOr("ABUSEKIT_VENDORS_CONFIG", "config/vendors.yaml"), "path to vendors.yaml")
	fs.StringVar(&c.weightsPath, "weights", envOr("ABUSEKIT_LOCAL_WEIGHTS", "config/local_weights.yaml"), "path to the local scorer's weights YAML")
	fs.StringVar(&c.brandsPath, "brands", envOr("ABUSEKIT_BRANDS_CONFIG", "config/brands.yaml"), "path to brands.yaml")
	// S2b: brands-extra is OPTIONAL and has no default path at all (unlike
	// --brands) — an operator's private brand list lives outside this
	// public repo (AGENTS.md's data-boundary rule), so there is no
	// checked-in file a default could ever point at. Empty (the default)
	// means "no private brand list", not an error.
	fs.StringVar(&c.brandsExtraPath, "brands-extra", os.Getenv("ABUSEKIT_BRANDS_EXTRA_CONFIG"), "optional path to a private, brands.yaml-shaped extra brand list, merged with --brands (env ABUSEKIT_BRANDS_EXTRA_CONFIG; empty disables it — S2b)")
	fs.StringVar(&c.webmailPath, "webmail", envOr("ABUSEKIT_WEBMAIL_CONFIG", "config/webmail.yaml"), "path to webmail.yaml (S2b)")
	// B2 fix round: NO default keys path. A silently-defaulted
	// config/keys.yaml is exactly the fail-open behavior this guards
	// against -- every deployment must say explicitly where its keys live.
	fs.StringVar(&c.keysPath, "keys", os.Getenv("ABUSEKIT_KEYS_CONFIG"), "path to keys.yaml (design §4.3's per-producer/operator credentials) -- required, no default (env ABUSEKIT_KEYS_CONFIG)")
	fs.StringVar(&c.metricsListen, "metrics-listen", envOr("ABUSEKIT_METRICS_LISTEN", "127.0.0.1:9099"), "loopback address to serve worker.Metrics on via expvar's /debug/vars (env ABUSEKIT_METRICS_LISTEN; empty disables it — R8 round 2)")
	// B2 fix round: loopback by default, not every interface -- an
	// operator has to opt into a wider bind explicitly.
	fs.StringVar(&c.listenAddr, "listen", envOr("ABUSEKIT_LISTEN", "127.0.0.1:8080"), "address to serve the /v1/* HTTP API on (env ABUSEKIT_LISTEN; empty disables it — S3)")
	if err := fs.Parse(args); err != nil {
		return serveConfig{}, err
	}
	if c.databaseURL == "" {
		return serveConfig{}, errors.New("a database URL is required: pass --database-url or set ABUSEKIT_DATABASE_URL")
	}
	if c.keysPath == "" {
		return serveConfig{}, errors.New("a keys config is required: pass --keys or set ABUSEKIT_KEYS_CONFIG (no default — B2 fix round)")
	}
	return c, nil
}

// validateKeySecrets is the B2 fix round's boot-time guard against a
// fail-open credential set: every secret must be at least
// minKeySecretBytes long (regardless of --dev — a short secret is weak
// however it got there), and none may carry devSecretPrefix unless dev is
// true (config/keys.yaml, this repo's own shipped dev/test file, names
// every secret this way specifically so it can never boot silently
// outside --dev).
func validateKeySecrets(keys map[string]config.Key, dev bool) error {
	ids := make([]string, 0, len(keys))
	for id := range keys {
		ids = append(ids, id)
	}
	sort.Strings(ids) // deterministic error ordering, not map iteration
	for _, id := range ids {
		k := keys[id]
		if len(k.Secret) < minKeySecretBytes {
			return fmt.Errorf("key %q: secret is %d bytes, want >= %d", id, len(k.Secret), minKeySecretBytes)
		}
		if !dev && strings.HasPrefix(k.Secret, devSecretPrefix) {
			return fmt.Errorf("key %q: secret is prefixed %q (config/keys.yaml's own dev/test credentials) -- pass --dev to allow this, or point --keys at a real credential file", id, devSecretPrefix)
		}
	}
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// runServe implements the `serve` subcommand: parse flags, then derive a
// SIGINT/SIGTERM-cancelable context (S17) for runServeWithContext.
func runServe(args []string) error {
	c, err := parseServeFlags(args)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return runServeWithContext(ctx, c)
}

// runServeWithContext is runServe's body, factored out (matching
// runMigrate/runMigrateWithContext's existing pattern) so a test can drive
// it with a context it controls directly, instead of needing a real OS
// signal to ever make the non-check path return.
//
// `--check` (S17) only validates config and migrates the database, then
// exits 0 — unaffected by S9's worker wiring below. Without --check (S9
// fix round), it boots the same way and then runs internal/worker's
// scoring loop standing until ctx is done; Stop() (which cancels and
// WAITS — see Worker.Stop's doc comment) runs before this function
// returns, so a caller can rely on the worker having actually stopped
// touching the store by the time it does. The `/v1/*` HTTP surface itself
// is still S3's job — nothing here listens on a port.
func requireProductionKeySpace(environment, keySpace string) error {
	if environment == "production" && keySpace != "ns-v1" {
		return fmt.Errorf("production requires feature key space ns-v1")
	}
	return nil
}

func runServeWithContext(ctx context.Context, c serveConfig) error {
	if err := requireProductionKeySpace(os.Getenv("ABUSEKIT_ENV"), feature.KeySpace); err != nil {
		return err
	}

	s, cfg, deps, err := boot(ctx, c)
	if err != nil {
		return err
	}
	defer s.Close() // S17: close on every exit path, including this success one

	if c.check {
		fmt.Println("abusekit: store migrated, config valid")
		return nil
	}

	w, err := worker.New(worker.Deps{
		Store:     s,
		Config:    cfg,
		Neighbors: deps.neighbors,
		Brands:    deps.brands,
		Webmail:   deps.webmail,
		Metrics:   deps.metrics,
		Budgets:   deps.budgets,
		Logger:    slog.Default(),
	})
	if err != nil {
		return fmt.Errorf("construct worker: %w", err)
	}

	// R8 round 2: publish worker.Metrics and serve it over a loopback HTTP
	// listener (expvar's own /debug/vars) so S8's counters are observable
	// from outside the process, not just via Snapshot() in a test. An
	// idempotent-publish guard (rather than an unconditional Publish,
	// which panics on a second call with the same name) is defensive
	// against this func running more than once in the same process — a
	// real `serve` invocation never does, but a test binary calling
	// runServeWithContext more than once otherwise would panic on the
	// process-wide expvar registry.
	const metricsVarName = "abusekit"
	if expvar.Get(metricsVarName) == nil {
		deps.metrics.Publish(metricsVarName)
	}
	metricsSrv, metricsAddr, err := startMetricsServer(c.metricsListen)
	if err != nil {
		return fmt.Errorf("start metrics server: %w", err)
	}
	if metricsSrv != nil {
		fmt.Println("abusekit: metrics available at http://" + metricsAddr + "/debug/vars")
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = metricsSrv.Shutdown(shutdownCtx)
		}()
	}

	// S3: the /v1/* HTTP API. Started BEFORE the worker begins ticking
	// (so evaluate/events never race a not-yet-ready dependency) and shut
	// down BEFORE the worker stops (below) — draining in-flight HTTP
	// requests first means an evaluate call that's already claimed a
	// subject gets to finish its round through a still-running worker
	// rather than racing its own shutdown.
	apiSrv, apiAddr, err := startAPIServer(c.listenAddr, s, w, cfg, deps.keys, deps.neighbors, deps.brands, deps.webmail)
	if err != nil {
		return fmt.Errorf("start api server: %w", err)
	}
	if apiSrv != nil {
		fmt.Println("abusekit: API listening on http://" + apiAddr)
	}

	fmt.Println("abusekit: worker running; press Ctrl-C to stop")
	w.Start(ctx)
	// Start returns once ctx is done. Shut the API server down first (it
	// stops accepting new requests and waits for in-flight ones), then
	// Stop the worker — safe (and a no-op beyond releasing state) to call
	// here for symmetry with a caller that constructs a Worker and drives
	// Start/Stop itself.
	if apiSrv != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = apiSrv.Shutdown(shutdownCtx)
		cancel()
	}
	w.Stop()
	fmt.Println("abusekit: worker stopped")
	return nil
}

// apiServerTimeouts bound the HTTP API's connection lifecycle (task's
// "timeouts on the server"): generous enough for a synchronous evaluate
// call at its full {deadline_ms<=3000} ceiling with room to spare, tight
// enough that a stalled client can't hold a connection open indefinitely.
const (
	apiReadHeaderTimeout = 5 * time.Second
	apiReadTimeout       = 10 * time.Second
	apiWriteTimeout      = 15 * time.Second
	apiIdleTimeout       = 60 * time.Second
)

// startAPIServer builds internal/serve's Server and starts it on addr.
// addr == "" disables it entirely (matching startMetricsServer's own
// convention), returning (nil, "", nil) — a test constructing a
// serveConfig by hand (leaving listenAddr at its zero value) gets no
// listener at all, never a port collision.
func startAPIServer(addr string, s *store.Store, w *worker.Worker, cfg *config.Config, keys map[string]config.Key, neighbors feature.Neighbors, brands feature.BrandSet, webmail feature.WebmailSet) (*http.Server, string, error) {
	if addr == "" {
		return nil, "", nil
	}
	srv, err := serve.New(serve.Deps{
		Store: s, Worker: w, Config: cfg, Keys: keys,
		Neighbors: neighbors, Brands: brands, Webmail: webmail, Logger: slog.Default(),
	})
	if err != nil {
		return nil, "", fmt.Errorf("construct http server: %w", err)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, "", fmt.Errorf("listen on %s: %w", addr, err)
	}
	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: apiReadHeaderTimeout,
		ReadTimeout:       apiReadTimeout,
		WriteTimeout:      apiWriteTimeout,
		IdleTimeout:       apiIdleTimeout,
	}
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Default().Error("abusekit: api server failed", "error", err)
		}
	}()
	return httpSrv, ln.Addr().String(), nil
}

// startMetricsServer starts a loopback HTTP server on addr serving
// http.DefaultServeMux (which expvar registers /debug/vars on
// automatically, as soon as anything imports the expvar package —
// internal/worker's metrics.go already does), returning it (for the
// caller to Shutdown) and the listener's actual bound address. addr == ""
// disables it entirely, returning (nil, "", nil) — R8 round 2's default
// is "127.0.0.1:9099" (parseServeFlags), but any test constructing a
// serveConfig by hand (leaving metricsListen at its zero value) gets no
// listener at all, never a port collision.
//
// These endpoints are unauthenticated by design, matching the OSS
// server's own E2A_METRICS_LISTEN_ADDR convention (AGENTS.md: "never
// route them through Caddy/HAProxy") — loopback-only is the whole
// safeguard, not an auth check.
func startMetricsServer(addr string) (*http.Server, string, error) {
	if addr == "" {
		return nil, "", nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, "", fmt.Errorf("listen on %s: %w", addr, err)
	}
	srv := &http.Server{Handler: http.DefaultServeMux}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Default().Error("abusekit: metrics server failed", "error", err)
		}
	}()
	return srv, ln.Addr().String(), nil
}

// Placeholder daily budget defaults (S7 fix round; design §8 open question
// 3, "$5/day per vendor adapter, 20 calls/subject/day, 2,000/producer/day —
// confirm"): v0 has no vendor adapter registered at all, so these are
// currently dead code on every real request (internal/worker never checks
// a budget for a rule scored by "local"). defaultPerTenantDailyBudget
// reuses design's "2,000/producer/day" figure under internal/worker.
// Budgets' documented producer-to-tenant substitution; there is no
// per-adapter dollar-to-call conversion available yet, so
// defaultPerAdapterDailyBudget is a round, deliberately generous
// placeholder pending real per-adapter cost data.
const (
	defaultPerAdapterDailyBudget = 5000
	defaultPerTenantDailyBudget  = 2000
)

// bootDeps are the pieces of boot's startup sequence that only `serve`
// (not `serve --check` or `migrate`) needs to go on and construct a
// worker.Worker from.
type bootDeps struct {
	neighbors feature.Neighbors
	brands    feature.BrandSet
	webmail   feature.WebmailSet
	metrics   *worker.Metrics
	budgets   *worker.Budgets
	// keys is design §4.3's per-producer/operator credential set (S3),
	// loaded alongside every other config file below — a bad keys.yaml
	// fails boot the same way a bad rules/vendors/weights/brands file
	// does, before ever touching Postgres.
	keys map[string]config.Key
}

// boot performs the startup sequence every `serve` invocation needs:
// build the scorer registry (local only in S1/S2), load + validate the
// rule config against it, load the brand list (S3 fix round), and only
// THEN connect to and migrate the store (S17) — a bad rules/vendors/
// weights/brands file fails fast without ever touching Postgres, rather
// than migrating a database it's about to report as unusable anyway.
func boot(ctx context.Context, c serveConfig) (*store.Store, *config.Config, bootDeps, error) {
	cfg, _, brands, err := loadRuleConfig(ruleConfigPaths{
		rulesPath:   c.rulesPath,
		vendorsPath: c.vendorsPath,
		weightsPath: c.weightsPath,
		brandsPath:  c.brandsPath,
	})
	if err != nil {
		return nil, nil, bootDeps{}, err
	}
	// S2b: brands-extra is optional (see parseServeFlags' own comment) —
	// an empty path merges in nothing, MergeBrandSets(brands, BrandSet{})
	// behaves identically to brands alone.
	if c.brandsExtraPath != "" {
		extra, err := feature.LoadBrandsFile(c.brandsExtraPath)
		if err != nil {
			return nil, nil, bootDeps{}, fmt.Errorf("load brands-extra config: %w", err)
		}
		brands = feature.MergeBrandSets(brands, extra)
	}

	webmail, err := feature.LoadWebmailFile(c.webmailPath)
	if err != nil {
		return nil, nil, bootDeps{}, fmt.Errorf("load webmail config: %w", err)
	}

	keysData, err := os.ReadFile(c.keysPath)
	if err != nil {
		return nil, nil, bootDeps{}, fmt.Errorf("read keys config: %w", err)
	}
	keys, err := config.LoadKeys(keysData)
	if err != nil {
		return nil, nil, bootDeps{}, fmt.Errorf("load keys config: %w", err)
	}
	if err := validateKeySecrets(keys, c.dev); err != nil {
		return nil, nil, bootDeps{}, fmt.Errorf("validate keys config: %w", err)
	}

	pool, err := pgxpool.New(ctx, c.databaseURL)
	if err != nil {
		return nil, nil, bootDeps{}, fmt.Errorf("connect to database: %w", err)
	}
	s := store.New(pool)
	if err := s.ApplyMigrations(ctx); err != nil {
		pool.Close()
		return nil, nil, bootDeps{}, fmt.Errorf("apply migrations: %w", err)
	}

	metrics := worker.NewMetrics()
	deps := bootDeps{
		neighbors: feature.NewStoreNeighbors(s, feature.Config{}), // default link-kind policy (S1 fix round) until a tenant-specific override exists
		brands:    brands,
		webmail:   webmail,
		budgets:   worker.NewPersistedBudgets(s, defaultPerAdapterDailyBudget, worker.DefaultPerSubjectDailyBudget, defaultPerTenantDailyBudget),
		metrics:   metrics,
		keys:      keys,
	}
	return s, cfg, deps, nil
}

// runMigrate handles SIGINT/SIGTERM (S17): a long-running migration
// against a large database can now be interrupted cleanly instead of
// running to completion regardless.
func runMigrate(args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	databaseURL := fs.String("database-url", os.Getenv("ABUSEKIT_DATABASE_URL"), "Postgres connection string (env ABUSEKIT_DATABASE_URL)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *databaseURL == "" {
		return errors.New("a database URL is required: pass --database-url or set ABUSEKIT_DATABASE_URL")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return runMigrateWithContext(ctx, *databaseURL)
}

// runMigrateWithContext is runMigrate's body, factored out so a test can
// drive it with an already-cancelled context and prove SIGINT/SIGTERM
// actually reaches pgx rather than being silently dropped somewhere on
// the way.
func runMigrateWithContext(ctx context.Context, databaseURL string) error {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer pool.Close()

	if err := store.New(pool).ApplyMigrations(ctx); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	fmt.Println("abusekit: migrations applied")
	return nil
}
