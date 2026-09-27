// Command abusekit is the single binary that runs abusekit (design §2:
// "One Go binary; Postgres").
//
// S1 wires up the store and config layers behind two subcommands; the
// HTTP surface (`serve`'s actual `/v1/*` handlers) is S3's job. Until
// then, `serve` only supports `--check` (S17): it boots exactly what a
// real server would need at startup — a validated rule config, then a
// migrated database — and exits 0. Without `--check` it returns an
// explicit "not implemented yet" error rather than blocking on a signal
// with nothing actually listening.
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
	"github.com/tokencanopy/abusekit/internal/feature"
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

// runServe implements the `serve` subcommand (S1 scope: config
// validation and a migrated database, nothing more — the real HTTP
// surface arrives in S3). S17: `serve` without --check is no longer a
// blocking placeholder loop that waits on SIGINT/SIGTERM for no reason;
// there is nothing to serve yet, so it's an explicit error instead.
func runServe(args []string) error {
	c, err := parseServeFlags(args)
	if err != nil {
		return err
	}
	if !c.check {
		return errors.New("abusekit: serve is not implemented yet without --check (the HTTP surface arrives in S3); pass --check to validate startup and exit")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	s, _, err := boot(ctx, c)
	if err != nil {
		return err
	}
	defer s.Close() // S17: close on every exit path, including this success one

	fmt.Println("abusekit: store migrated, config valid (S1 placeholder — HTTP surface arrives in S3)")
	return nil
}

// boot performs the startup sequence real `serve` (S3) will also need:
// build the scorer registry (local only in S1), load + validate the rule
// config against it, and only THEN connect to and migrate the store
// (S17) — a bad rules/vendors/weights file fails fast without ever
// touching Postgres, rather than migrating a database it's about to
// report as unusable anyway. Returns both so a future `serve` can keep
// using them; S1's caller only needs to know whether it succeeded.
func boot(ctx context.Context, c serveConfig) (*store.Store, *config.Config, error) {
	weights, err := local.LoadWeightsFile(c.weightsPath)
	if err != nil {
		return nil, nil, err
	}
	localScorer, err := local.New(weights)
	if err != nil {
		return nil, nil, err
	}
	registry := model.NewRegistry()
	if err := registry.Register(localScorer); err != nil {
		return nil, nil, err
	}

	vendorsData, err := os.ReadFile(c.vendorsPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read vendors config: %w", err)
	}
	vendors, err := config.LoadVendors(vendorsData)
	if err != nil {
		return nil, nil, err
	}

	rulesData, err := os.ReadFile(c.rulesPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read rules config: %w", err)
	}
	cfg, err := config.Load(rulesData, config.Dependencies{
		Registry: registry,
		Features: config.NewFeatureSet(feature.Names...),
		Vendors:  vendors,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("load rules config: %w", err)
	}

	pool, err := pgxpool.New(ctx, c.databaseURL)
	if err != nil {
		return nil, nil, fmt.Errorf("connect to database: %w", err)
	}
	s := store.New(pool)
	if err := s.ApplyMigrations(ctx); err != nil {
		pool.Close()
		return nil, nil, fmt.Errorf("apply migrations: %w", err)
	}

	return s, cfg, nil
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
