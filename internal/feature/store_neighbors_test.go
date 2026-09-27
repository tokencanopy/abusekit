package feature_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tokencanopy/abusekit/internal/event"
	"github.com/tokencanopy/abusekit/internal/feature"
	"github.com/tokencanopy/abusekit/internal/store"
)

// This file's DB harness mirrors internal/worker's own (a throwaway
// database per test) rather than internal/store's shared-schema-per-binary
// one: internal/feature's DB-backed suite is small (one file, exercising
// StoreNeighbors specifically) and self-contained isolation is simpler
// than importing store's unexported test machinery.

const defaultFeatureTestDBURL = "postgres://e2a:e2a@localhost:5433/abusekit_test?sslmode=disable"

func featureTestDBURL() string {
	if u := os.Getenv("ABUSEKIT_TEST_DATABASE_URL"); u != "" {
		return u
	}
	return defaultFeatureTestDBURL
}

func requireDB() bool { return os.Getenv("ABUSEKIT_REQUIRE_DB") == "1" }

func unavailable(t *testing.T, format string, args ...any) {
	t.Helper()
	if requireDB() {
		t.Fatalf(format, args...)
	}
	t.Skipf(format, args...)
}

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping DB-backed test in -short mode")
	}
	ctx := context.Background()
	base := featureTestDBURL()

	probe, err := pgxpool.New(ctx, base)
	if err != nil {
		unavailable(t, "test database not available: %v", err)
		return nil
	}
	pingErr := probe.Ping(ctx)
	probe.Close()
	if pingErr != nil {
		var pgErr *pgconn.PgError
		if !errors.As(pingErr, &pgErr) || pgErr.Code != "3D000" {
			unavailable(t, "test database not available: %v", pingErr)
			return nil
		}
	}

	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse test db url: %v", err)
	}
	name := fmt.Sprintf("abusekit_feature_%d_%d", os.Getpid(), time.Now().UnixNano())
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

const testTenant = "e2a"

func mustAppend(t *testing.T, ctx context.Context, s *store.Store, subject, typ string, at time.Time, links event.Links, data map[string]any) {
	t.Helper()
	e := event.Event{ID: subject + "-" + typ + "-" + at.String(), Subject: subject, Type: typ, At: at, Links: links, Data: data}
	if err := e.Validate(event.ValidateOptions{Now: at}); err != nil {
		t.Fatalf("validate fixture event: %v", err)
	}
	if err := e.Redact(); err != nil {
		t.Fatalf("redact fixture event: %v", err)
	}
	if _, err := s.AppendEvents(ctx, testTenant, "test-producer", []event.Event{e}); err != nil {
		t.Fatalf("append fixture event: %v", err)
	}
}

// hexHash returns a deterministic, valid-format (64 lower-case hex chars)
// synthetic link hash from seed — not a real per-tenant HMAC (design
// §4.2), just something that satisfies event.Links' format validation for
// these tests without embedding any real identifier.
func hexHash(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])
}

func TestStoreNeighbors_ExcludesASNByDefault(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)

	asn := "AS64500"
	mustAppend(t, ctx, s, "acct_a", "subject.created", now, event.Links{ASN: asn}, nil)
	mustAppend(t, ctx, s, "acct_b", "subject.created", now, event.Links{ASN: asn}, nil)
	mustAppend(t, ctx, s, "acct_b", "subject.deleted", now.Add(time.Minute), event.Links{}, map[string]any{"mode": "trash"})

	n := feature.NewStoreNeighbors(s, feature.Config{IncludeASN: false})
	ev, err := n.Evidence(ctx, testTenant, "acct_a")
	if err != nil {
		t.Fatalf("Evidence: %v", err)
	}
	if ev.DeletedCount != 0 {
		t.Errorf("with IncludeASN=false: DeletedCount = %d, want 0 (acct_b is only linked via ASN)", ev.DeletedCount)
	}

	n2 := feature.NewStoreNeighbors(s, feature.Config{IncludeASN: true})
	ev2, err := n2.Evidence(ctx, testTenant, "acct_a")
	if err != nil {
		t.Fatalf("Evidence: %v", err)
	}
	if ev2.DeletedCount != 1 {
		t.Errorf("with IncludeASN=true: DeletedCount = %d, want 1 (acct_b shares the ASN and was deleted)", ev2.DeletedCount)
	}
}

func TestStoreNeighbors_LabelledAbusiveViaEmailHash(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)

	emailHash := hexHash("shared-email")
	mustAppend(t, ctx, s, "acct_a", "subject.created", now, event.Links{EmailHash: emailHash}, nil)
	mustAppend(t, ctx, s, "acct_b", "subject.created", now, event.Links{EmailHash: emailHash}, nil)

	if _, err := s.PutLabel(ctx, testTenant, store.Label{Subject: "acct_b", Label: "abusive", Source: "operator", Actor: "ops@example.test"}); err != nil {
		t.Fatalf("PutLabel: %v", err)
	}

	n := feature.NewStoreNeighbors(s, feature.Config{IncludeASN: false})
	ev, err := n.Evidence(ctx, testTenant, "acct_a")
	if err != nil {
		t.Fatalf("Evidence: %v", err)
	}
	if ev.LabelledAbusiveCount != 1 {
		t.Errorf("LabelledAbusiveCount = %d, want 1", ev.LabelledAbusiveCount)
	}
}

func TestStoreNeighbors_FingerprintSharedIsCardSpecific(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)

	card := hexHash("shared-card")
	mustAppend(t, ctx, s, "acct_a", "payment.attempt", now, event.Links{CardFingerprintHash: card}, map[string]any{"outcome": "declined", "funding": "credit"})
	mustAppend(t, ctx, s, "acct_b", "payment.attempt", now, event.Links{CardFingerprintHash: card}, map[string]any{"outcome": "declined", "funding": "credit"})

	// acct_c shares only an ASN with acct_a — this must NOT count toward
	// FingerprintShared, which is specific to card_fingerprint_hash.
	mustAppend(t, ctx, s, "acct_c", "subject.created", now, event.Links{ASN: "AS64500"}, nil)
	mustAppend(t, ctx, s, "acct_a", "subject.created", now, event.Links{ASN: "AS64500"}, nil)

	n := feature.NewStoreNeighbors(s, feature.Config{IncludeASN: true}) // even with ASN included, fingerprint check is unaffected
	ev, err := n.Evidence(ctx, testTenant, "acct_a")
	if err != nil {
		t.Fatalf("Evidence: %v", err)
	}
	if !ev.FingerprintShared {
		t.Errorf("FingerprintShared = false, want true (acct_b shares the card fingerprint)")
	}

	n2 := feature.NewStoreNeighbors(s, feature.Config{IncludeASN: false})
	evNoFingerprint, err := n2.Evidence(ctx, testTenant, "acct_c")
	if err != nil {
		t.Fatalf("Evidence: %v", err)
	}
	if evNoFingerprint.FingerprintShared {
		t.Errorf("FingerprintShared = true for acct_c, want false (only shares an ASN, no card fingerprint)")
	}
}

func TestStoreNeighbors_NoNeighbors(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	mustAppend(t, ctx, s, "acct_lonely", "subject.created", now, event.Links{}, nil)

	n := feature.NewStoreNeighbors(s, feature.Config{})
	ev, err := n.Evidence(ctx, testTenant, "acct_lonely")
	if err != nil {
		t.Fatalf("Evidence: %v", err)
	}
	if ev != (feature.NeighborEvidence{}) {
		t.Errorf("Evidence = %+v, want the zero value", ev)
	}
}
