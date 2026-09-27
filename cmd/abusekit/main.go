// Command abusekit is the single binary that runs abusekit (design §2:
// "One Go binary; Postgres").
//
// S1 wires up the store and config layers behind two subcommands; the
// HTTP surface (`serve`'s actual `/v1/*` handlers) is S3's job. Until
// then, `serve` boots exactly what a real server would need at startup —
// a migrated database and a validated rule config — and either exits 0
// immediately (`--check`, for CI and deploy smoke tests) or blocks until
// an OS signal, so the binary already has the shape (long-running,
// signal-terminated) the eventual HTTP server will have.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tokencanopy/abusekit/internal/config"
	"github.com/tokencanopy/abusekit/internal/model"
	"github.com/tokencanopy/abusekit/internal/model/local"
	"github.com/tokencanopy/abusekit/internal/store"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "abusekit:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: abusekit <serve|migrate> [flags]")
	}
	switch args[0] {
	case "serve":
		return runServe(args[1:])
	case "migrate":
		return runMigrate(args[1:])
	default:
		return fmt.Errorf("unknown subcommand %q (want \"serve\" or \"migrate\")", args[0])
	}
}

// v0FeatureNames is design §4.5's new_account_velocity input list — the
// only features S1 knows the NAMES of; internal/feature (S2) is what
// actually computes them. Registering just the names here is what lets
// internal/config's loader validate config/rules.yaml's `inputs` today
// without a real feature implementation existing yet (task S1 scope:
// "a Registry of feature names is passed in (S2 fills it, S1 registers
// the names only)").
var v0FeatureNames = []string{
	"subject_age_h", "resource_velocity_1h", "resource_total", "key_velocity_1h", "key_total",
	"upgrade_delay_min", "declines_before_first_success", "first_funding_prepaid",
	"name_brand_match", "name_has_at", "first_day_distinct_domains", "self_send_before_external",
	"linked_deleted_n", "linked_labelled_abusive_n", "fingerprint_seen_on_other_subjects",
	"burst_ratio_24h_vs_lifetime",
}

type serveConfig struct {
	check       bool
	databaseURL string
	rulesPath   string
	vendorsPath string
	weightsPath string
}

func parseServeFlags(args []string) (serveConfig, error) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	var c serveConfig
	fs.BoolVar(&c.check, "check", false, "boot store and config, then exit 0 without serving")
	fs.StringVar(&c.databaseURL, "database-url", os.Getenv("ABUSEKIT_DATABASE_URL"), "Postgres connection string (env ABUSEKIT_DATABASE_URL)")
	fs.StringVar(&c.rulesPath, "rules", envOr("ABUSEKIT_RULES_CONFIG", "config/rules.yaml"), "path to rules.yaml")
	fs.StringVar(&c.vendorsPath, "vendors", envOr("ABUSEKIT_VENDORS_CONFIG", "config/vendors.yaml"), "path to vendors.yaml")
	fs.StringVar(&c.weightsPath, "weights", envOr("ABUSEKIT_LOCAL_WEIGHTS", "config/local_weights.yaml"), "path to the local scorer's weights YAML")
	if err := fs.Parse(args); err != nil {
		return serveConfig{}, err
	}
	if c.databaseURL == "" {
		return serveConfig{}, errors.New("a database URL is required: pass --database-url or set ABUSEKIT_DATABASE_URL")
	}
	return c, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// runServe implements the `serve` subcommand's S1 placeholder behavior:
// connect, migrate, build the scorer registry, load and validate config,
// then either exit 0 (--check) or block until SIGINT/SIGTERM.
func runServe(args []string) error {
	c, err := parseServeFlags(args)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if _, _, err := boot(ctx, c); err != nil {
		return err
	}

	fmt.Println("abusekit: store migrated, config valid (S1 placeholder — HTTP surface arrives in S3)")
	if c.check {
		return nil
	}

	fmt.Println("abusekit: waiting for SIGINT/SIGTERM")
	<-ctx.Done()
	fmt.Println("abusekit: shutting down")
	return nil
}

// boot performs the startup sequence real `serve` (S3) will also need:
// connect + migrate the store, build the scorer registry (local only in
// S1), and load + validate the rule config against it. Returns both so a
// future `serve` can keep using them; S1's caller only needs to know
// whether it succeeded.
func boot(ctx context.Context, c serveConfig) (*store.Store, *config.Config, error) {
	pool, err := pgxpool.New(ctx, c.databaseURL)
	if err != nil {
		return nil, nil, fmt.Errorf("connect to database: %w", err)
	}
	s := store.New(pool)
	if err := s.ApplyMigrations(ctx); err != nil {
		pool.Close()
		return nil, nil, fmt.Errorf("apply migrations: %w", err)
	}

	weights, err := local.LoadWeightsFile(c.weightsPath)
	if err != nil {
		pool.Close()
		return nil, nil, err
	}
	localScorer, err := local.New(weights)
	if err != nil {
		pool.Close()
		return nil, nil, err
	}
	registry := model.NewRegistry()
	if err := registry.Register(localScorer); err != nil {
		pool.Close()
		return nil, nil, err
	}

	vendorsData, err := os.ReadFile(c.vendorsPath)
	if err != nil {
		pool.Close()
		return nil, nil, fmt.Errorf("read vendors config: %w", err)
	}
	vendors, err := config.LoadVendors(vendorsData)
	if err != nil {
		pool.Close()
		return nil, nil, err
	}

	rulesData, err := os.ReadFile(c.rulesPath)
	if err != nil {
		pool.Close()
		return nil, nil, fmt.Errorf("read rules config: %w", err)
	}
	cfg, err := config.Load(rulesData, config.Dependencies{
		Registry: registry,
		Features: config.NewFeatureSet(v0FeatureNames...),
		Vendors:  vendors,
	})
	if err != nil {
		pool.Close()
		return nil, nil, fmt.Errorf("load rules config: %w", err)
	}

	return s, cfg, nil
}

func runMigrate(args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	databaseURL := fs.String("database-url", os.Getenv("ABUSEKIT_DATABASE_URL"), "Postgres connection string (env ABUSEKIT_DATABASE_URL)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *databaseURL == "" {
		return errors.New("a database URL is required: pass --database-url or set ABUSEKIT_DATABASE_URL")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, *databaseURL)
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
