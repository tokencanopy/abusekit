package store_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tokencanopy/abusekit/internal/event"
	"github.com/tokencanopy/abusekit/internal/store"
)

const testTenant = "e2a"

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse time %q: %v", s, err)
	}
	return tm
}

func mkEvent(t *testing.T, id, subject, typ string, at time.Time, data map[string]any) event.Event {
	t.Helper()
	e := event.Event{ID: id, Subject: subject, Type: typ, At: at, Data: data}
	if err := e.Validate(event.ValidateOptions{Now: at}); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if err := e.Redact(); err != nil {
		t.Fatalf("Redact: %v", err)
	}
	return e
}

// TestApplyMigrations_FreshDatabase is the "migration applies on a fresh
// DB" acceptance criterion from plan.md's S1 row (S11: this must be a
// database this test creates and drops itself, not the shared per-run
// schema newTestStore's other callers already migrated earlier in the
// same test binary run — otherwise "fresh" is only true the first time
// any test in the package happens to run).
func TestApplyMigrations_FreshDatabase(t *testing.T) {
	dbURL := newThrowawayDatabaseURL(t)
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()

	s := store.New(pool)
	if err := s.ApplyMigrations(ctx); err != nil {
		t.Fatalf("ApplyMigrations on a genuinely fresh database: %v", err)
	}
	if err := s.ApplyMigrations(ctx); err != nil {
		t.Fatalf("re-applying migrations should be a no-op, got: %v", err)
	}
}

// TestApplyMigrations_ConcurrentOnFreshDatabase is B4: two instances
// racing to migrate the same brand-new database (the real startup
// scenario — a multi-instance deploy, or `go test ./...` running two
// packages' newTestStore concurrently) must all succeed. Proven: before
// wrapping the migration loop in pg_advisory_xact_lock, ~35/40 concurrent
// ApplyMigrations calls against a fresh database failed with a duplicate
// pg_type key (two sessions both trying to CREATE TABLE at once).
func TestApplyMigrations_ConcurrentOnFreshDatabase(t *testing.T) {
	dbURL := newThrowawayDatabaseURL(t)
	ctx := context.Background()

	const n = 40
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			pool, err := pgxpool.New(ctx, dbURL)
			if err != nil {
				errs <- fmt.Errorf("open pool: %w", err)
				return
			}
			defer pool.Close()
			errs <- store.New(pool).ApplyMigrations(ctx)
		}()
	}

	var failures int
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			failures++
			t.Logf("concurrent ApplyMigrations call %d failed: %v", i, err)
		}
	}
	if failures > 0 {
		t.Fatalf("%d/%d concurrent ApplyMigrations calls against a fresh database failed", failures, n)
	}

	// The schema must actually be usable afterward, not just "no error
	// returned" (e.g. a partially-applied migration that happened not to
	// error on this particular interleaving).
	verifyPool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("open verify pool: %v", err)
	}
	defer verifyPool.Close()
	if err := store.New(verifyPool).ApplyMigrations(ctx); err != nil {
		t.Fatalf("ApplyMigrations after the concurrent race: %v", err)
	}
}

// TestScopedPool_DoesNotInheritPublicSchemaTrackerState is S11's schema
// isolation, proven against the exact failure mode found while building
// it: a `search_path` of "runSchema, public" (WITH a public fallback)
// resolves ApplyMigrations' own unqualified `schema_migrations_abusekit`
// tracker check against public whenever runSchema doesn't have it yet —
// and a pre-existing public.schema_migrations_abusekit (there always is
// one on any Postgres server this repo's tests have run against before
// S11) makes that check see "001_core.sql already applied" and skip
// creating any table in runSchema at all, leaving it completely empty
// while every subsequent query keeps silently hitting public's tables.
// This test manufactures exactly that pre-existing state on a fresh
// throwaway database, then confirms a scoped pool with its own run
// schema still gets its OWN, independently populated tables.
func TestScopedPool_DoesNotInheritPublicSchemaTrackerState(t *testing.T) {
	dbURL := newThrowawayDatabaseURL(t)
	ctx := context.Background()

	// Simulate an older (pre-S11) or unscoped run that already applied
	// migrations directly to `public`.
	plainPool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("open plain pool: %v", err)
	}
	defer plainPool.Close()
	if err := store.New(plainPool).ApplyMigrations(ctx); err != nil {
		t.Fatalf("ApplyMigrations against public: %v", err)
	}

	// A scoped pool against the SAME database, with its own run schema,
	// must still create and use its own tables.
	scopedCfg, err := scopedPoolConfig(dbURL)
	if err != nil {
		t.Fatalf("scopedPoolConfig: %v", err)
	}
	scopedPool, err := pgxpool.NewWithConfig(ctx, scopedCfg)
	if err != nil {
		t.Fatalf("open scoped pool: %v", err)
	}
	defer scopedPool.Close()

	if _, err := scopedPool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+pgx.Identifier{runSchema}.Sanitize()); err != nil {
		t.Fatalf("create run schema: %v", err)
	}
	if err := store.New(scopedPool).ApplyMigrations(ctx); err != nil {
		t.Fatalf("ApplyMigrations in the scoped run schema: %v", err)
	}

	// Check information_schema directly, scoped to runSchema by name —
	// an ambient unqualified `SELECT ... FROM subjects` would succeed via
	// search_path fallback even when the table only exists in public,
	// which is exactly the bug this test exists to catch.
	var exists bool
	if err := scopedPool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_schema = $1 AND table_name = 'subjects')`,
		runSchema,
	).Scan(&exists); err != nil {
		t.Fatalf("query information_schema.tables: %v", err)
	}
	if !exists {
		t.Fatalf("expected the scoped run schema to have created its OWN subjects table, not silently reused public's via search_path fallback")
	}
}

func TestAppendEvents_AcceptsNewEvents(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-09-27T12:00:00Z")

	e1 := mkEvent(t, "evt_1", "acct_example_1", "subject.created", now, map[string]any{"channel": "api"})
	e2 := mkEvent(t, "evt_2", "acct_example_1", "resource.created", now.Add(time.Minute), map[string]any{"kind": "agent", "name": "Widget"})

	res, err := s.AppendEvents(ctx, testTenant, "e2a-server", []event.Event{e1, e2})
	if err != nil {
		t.Fatalf("AppendEvents: %v", err)
	}
	if len(res.Accepted) != 2 || len(res.Duplicates) != 0 || len(res.Rejected) != 0 {
		t.Fatalf("unexpected result: %+v", res)
	}

	stored, err := s.EventsForSubject(ctx, testTenant, "acct_example_1")
	if err != nil {
		t.Fatalf("EventsForSubject: %v", err)
	}
	if len(stored) != 2 {
		t.Fatalf("expected 2 stored events, got %d", len(stored))
	}
	if stored[0].Event.ID != "evt_1" || stored[1].Event.ID != "evt_2" {
		t.Fatalf("expected events ordered by `at`, got %s then %s", stored[0].Event.ID, stored[1].Event.ID)
	}
	if stored[1].Event.Data["kind"] != "agent" {
		t.Fatalf("expected data to round-trip, got %#v", stored[1].Event.Data)
	}
}

// TestAppendEvents_StampsRedactionVersion is N6: every stored event
// records the redaction schema version (event.RedactionSchemaVersion) it
// was redacted under, so a future schema change can identify which rows
// were redacted by an older rule set without re-deriving it from
// received_at timestamps.
func TestAppendEvents_StampsRedactionVersion(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-09-27T12:00:00Z")

	e := mkEvent(t, "evt_redaction_version", "acct_redaction_version", "subject.created", now, map[string]any{"channel": "api"})
	if _, err := s.AppendEvents(ctx, testTenant, "e2a-server", []event.Event{e}); err != nil {
		t.Fatalf("AppendEvents: %v", err)
	}

	pool := openScopedPool(t, ctx)
	var version int
	if err := pool.QueryRow(ctx,
		`SELECT redaction_version FROM events WHERE tenant = $1 AND id = $2`,
		testTenant, "evt_redaction_version",
	).Scan(&version); err != nil {
		t.Fatalf("query events.redaction_version: %v", err)
	}
	if version != event.RedactionSchemaVersion {
		t.Fatalf("expected redaction_version=%d, got %d", event.RedactionSchemaVersion, version)
	}
}

func TestAppendEvents_DuplicateVsConflict(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-09-27T12:00:00Z")

	e := mkEvent(t, "evt_dup", "acct_example_2", "subject.created", now, map[string]any{"channel": "api"})
	res, err := s.AppendEvents(ctx, testTenant, "e2a-server", []event.Event{e})
	if err != nil || len(res.Accepted) != 1 {
		t.Fatalf("first append: res=%+v err=%v", res, err)
	}

	// Exact replay: same id, identical body -> duplicate.
	replay := mkEvent(t, "evt_dup", "acct_example_2", "subject.created", now, map[string]any{"channel": "api"})
	res2, err := s.AppendEvents(ctx, testTenant, "e2a-server", []event.Event{replay})
	if err != nil {
		t.Fatalf("AppendEvents (replay): %v", err)
	}
	if len(res2.Duplicates) != 1 || res2.Duplicates[0] != "evt_dup" {
		t.Fatalf("expected a duplicate, got %+v", res2)
	}

	// Same id, different body -> conflict.
	changed := mkEvent(t, "evt_dup", "acct_example_2", "subject.created", now, map[string]any{"channel": "web"})
	res3, err := s.AppendEvents(ctx, testTenant, "e2a-server", []event.Event{changed})
	if err != nil {
		t.Fatalf("AppendEvents (conflict): %v", err)
	}
	if len(res3.Rejected) != 1 || res3.Rejected[0].Code != string(event.CodeConflict) {
		t.Fatalf("expected a conflict rejection, got %+v", res3)
	}
}

func TestAppendEvents_BumpsSubjectDirtySeq(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-09-27T12:00:00Z")

	for i := 0; i < 3; i++ {
		e := mkEvent(t, fmt.Sprintf("evt_%d", i), "acct_example_3", "resource.created", now.Add(time.Duration(i)*time.Second),
			map[string]any{"kind": "agent", "name": "a"})
		if _, err := s.AppendEvents(ctx, testTenant, "e2a-server", []event.Event{e}); err != nil {
			t.Fatalf("AppendEvents: %v", err)
		}
	}

	view, err := s.SubjectView(ctx, testTenant, "acct_example_3", nil)
	if err != nil {
		t.Fatalf("SubjectView: %v", err)
	}
	// Never scored: EventsSinceScore == dirty_seq - scored_seq(=0) == 3.
	if view.EventsSinceScore != 3 {
		t.Fatalf("expected dirty_seq to have bumped 3 times, got EventsSinceScore=%d", view.EventsSinceScore)
	}
	if view.Tier != "unknown" {
		t.Fatalf("expected an unscored subject to report tier=unknown, got %s", view.Tier)
	}
	if !view.Stale {
		t.Fatalf("expected an unscored subject with events to be stale")
	}
}

// TestAppendEvents_NeverReturnsAcceptedAlongsideError is B3: on an
// unexpected DB-level error partway through a batch, the whole call must
// report the error with an EMPTY result, never a partial Accepted list
// alongside a non-nil error. e2 deliberately bypasses Validate (which
// would normally reject a NUL byte per B1) to exercise AppendEvents' own
// INSERT failure path directly — a NUL byte in a text column is exactly
// the Postgres 22021/22P05 error the design references, and B1 makes it
// unreachable from real ingest traffic, but the store's own contract must
// still hold if it ever happens (a bug elsewhere, a future caller that
// forgets to Validate, ...): the transaction rolls back everything
// regardless, so a caller trusting a returned Accepted id here would
// believe an event was durably stored when it was not.
func TestAppendEvents_NeverReturnsAcceptedAlongsideError(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-01-01T00:00:00Z")

	e1 := mkEvent(t, "evt_ok", "acct_partial", "resource.created", now, map[string]any{"kind": "agent", "name": "a"})
	e2 := event.Event{
		ID: "evt_bad", Subject: "acct_partial\x00bad", Type: "resource.created", At: now.Add(time.Second),
		Data: map[string]any{"kind": "agent", "name": "b"},
	}

	res, err := s.AppendEvents(ctx, testTenant, "e2a-server", []event.Event{e1, e2})
	if err == nil {
		t.Fatalf("expected an error from the NUL byte in e2's subject")
	}
	if len(res.Accepted) != 0 || len(res.Duplicates) != 0 || len(res.Rejected) != 0 {
		t.Fatalf("expected an empty AppendResult alongside an error, got %+v", res)
	}
}

func TestAppendEvents_SubjectClassUpdatesSubjectRow(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-09-27T12:00:00Z")

	create := mkEvent(t, "evt_create", "mon-a", "subject.created", now, map[string]any{"channel": "api"})
	classify := mkEvent(t, "evt_class", "mon-a", "subject.class", now.Add(time.Second), map[string]any{"class": "synthetic"})

	if _, err := s.AppendEvents(ctx, testTenant, "e2a-server", []event.Event{create, classify}); err != nil {
		t.Fatalf("AppendEvents: %v", err)
	}

	view, err := s.SubjectView(ctx, testTenant, "mon-a", nil)
	if err != nil {
		t.Fatalf("SubjectView: %v", err)
	}
	if view.Class != "synthetic" {
		t.Fatalf("expected class=synthetic, got %s", view.Class)
	}
}

// TestAppendEvents_SubjectClassIsOrderedByAtNotDelivery is R11 (round 2):
// subjects.class must reflect the LATEST subject.class event by `at`, not
// whichever one was processed last. Proven scenario: a later
// classification (at=now+10s, "synthetic") is delivered and processed
// first, then an OLDER one (at=now+1s, "customer") arrives afterward
// (e.g. a retried/reordered delivery) — the older one must not overwrite
// the newer classification.
func TestAppendEvents_SubjectClassIsOrderedByAtNotDelivery(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-09-27T12:00:00Z")

	create := mkEvent(t, "evt_create_2", "mon-b", "subject.created", now, map[string]any{"channel": "api"})
	newer := mkEvent(t, "evt_class_newer", "mon-b", "subject.class", now.Add(10*time.Second), map[string]any{"class": "synthetic"})
	older := mkEvent(t, "evt_class_older", "mon-b", "subject.class", now.Add(1*time.Second), map[string]any{"class": "customer"})

	// Deliver the NEWER classification FIRST, then the OLDER one — the
	// older one arriving later (by delivery order) must not win.
	if _, err := s.AppendEvents(ctx, testTenant, "e2a-server", []event.Event{create, newer}); err != nil {
		t.Fatalf("AppendEvents (create+newer): %v", err)
	}
	if _, err := s.AppendEvents(ctx, testTenant, "e2a-server", []event.Event{older}); err != nil {
		t.Fatalf("AppendEvents (older, delivered second): %v", err)
	}

	view, err := s.SubjectView(ctx, testTenant, "mon-b", nil)
	if err != nil {
		t.Fatalf("SubjectView: %v", err)
	}
	if view.Class != "synthetic" {
		t.Fatalf("expected class to stay \"synthetic\" (the later-by-`at` classification), got %q", view.Class)
	}
}

func TestAppendEvents_UpsertsLinks(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-09-27T12:00:00Z")
	hash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" // 64 hex chars

	e1 := event.Event{ID: "e1", Subject: "acct_a", Type: "subject.created", At: now, Links: event.Links{EmailHash: hash}}
	if err := e1.Validate(event.ValidateOptions{Now: now}); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if err := e1.Redact(); err != nil {
		t.Fatalf("Redact: %v", err)
	}
	e2 := event.Event{ID: "e2", Subject: "acct_b", Type: "subject.created", At: now.Add(time.Minute), Links: event.Links{EmailHash: hash}}
	if err := e2.Validate(event.ValidateOptions{Now: now.Add(time.Minute)}); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if err := e2.Redact(); err != nil {
		t.Fatalf("Redact: %v", err)
	}

	if _, err := s.AppendEvents(ctx, testTenant, "e2a-server", []event.Event{e1, e2}); err != nil {
		t.Fatalf("AppendEvents: %v", err)
	}

	neighbors, truncated, err := s.Neighbors(ctx, testTenant, "acct_a", 0, 0)
	if err != nil {
		t.Fatalf("Neighbors: %v", err)
	}
	if truncated {
		t.Fatalf("did not expect truncation for 2 linked subjects")
	}
	if len(neighbors) != 1 || neighbors[0] != "acct_b" {
		t.Fatalf("expected acct_a's only neighbor to be acct_b, got %v", neighbors)
	}
}

func TestNeighbors_TruncatesAtCapPerKey(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-09-27T12:00:00Z")
	hash := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	var events []event.Event
	for i := 0; i < 5; i++ {
		e := event.Event{
			ID: fmt.Sprintf("e%d", i), Subject: fmt.Sprintf("acct_%d", i),
			Type: "subject.created", At: now.Add(time.Duration(i) * time.Second),
			Links: event.Links{EmailHash: hash},
		}
		if err := e.Validate(event.ValidateOptions{Now: e.At}); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if err := e.Redact(); err != nil {
			t.Fatalf("Redact: %v", err)
		}
		events = append(events, e)
	}
	if _, err := s.AppendEvents(ctx, testTenant, "e2a-server", events); err != nil {
		t.Fatalf("AppendEvents: %v", err)
	}

	// capPerKey=2 with 4 other subjects sharing the hash -> truncated.
	neighbors, truncated, err := s.Neighbors(ctx, testTenant, "acct_0", 2, 200)
	if err != nil {
		t.Fatalf("Neighbors: %v", err)
	}
	if !truncated {
		t.Fatalf("expected truncated=true when 4 neighbors exceed capPerKey=2")
	}
	if len(neighbors) != 2 {
		t.Fatalf("expected exactly capPerKey=2 neighbors, got %d: %v", len(neighbors), neighbors)
	}
}

func TestUpsertVerdictsAndSubjectView(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	// UpsertVerdicts stamps current_scored_at with the DB's own now(), so
	// the "not stale" / "stale again" assertions below need event
	// timestamps anchored to real wall-clock time, not a fixed fictional
	// instant — otherwise whether a fixed `at` falls before or after the
	// DB's now() depends on when the test happens to run.
	now := time.Now().UTC().Add(-2 * time.Hour)

	e := mkEvent(t, "evt_1", "acct_score_me", "resource.created", now, map[string]any{"kind": "agent", "name": "a"})
	if _, err := s.AppendEvents(ctx, testTenant, "e2a-server", []event.Event{e}); err != nil {
		t.Fatalf("AppendEvents: %v", err)
	}

	before, err := s.SubjectView(ctx, testTenant, "acct_score_me", nil)
	if err != nil {
		t.Fatalf("SubjectView (before): %v", err)
	}
	dirtySeqAtStart := before.EventsSinceScore // scored_seq is 0 so this equals dirty_seq

	risk := 0.85
	records := []store.VerdictRecord{
		{
			Rule: "new_account_velocity", Mode: "advise", Scorer: "local", Model: "local", Checkpoint: "v1",
			Probs: map[string]float64{"benign": 0.15, "abusive": 0.85}, Risk: &risk, Flagged: true,
			Reason: "high velocity", InputHash: "hash1", Status: "scored",
		},
	}
	ids, err := s.UpsertVerdicts(ctx, testTenant, "acct_score_me", dirtySeqAtStart, records, store.SubjectSummary{Tier: "high", Score: 0.85})
	if err != nil {
		t.Fatalf("UpsertVerdicts: %v", err)
	}
	if len(ids) != 1 || ids[0] == 0 {
		t.Fatalf("expected one non-zero verdict id, got %v", ids)
	}

	after, err := s.SubjectView(ctx, testTenant, "acct_score_me", nil)
	if err != nil {
		t.Fatalf("SubjectView (after): %v", err)
	}
	if after.Tier != "high" || after.Score != 0.85 {
		t.Fatalf("expected tier=high score=0.85, got tier=%s score=%v", after.Tier, after.Score)
	}
	if after.Stale {
		t.Fatalf("expected a freshly scored subject with no new events to not be stale")
	}
	if after.EventsSinceScore != 0 {
		t.Fatalf("expected EventsSinceScore=0 right after scoring, got %d", after.EventsSinceScore)
	}
	if len(after.Signals) != 1 || after.Signals[0].Rule != "new_account_velocity" {
		t.Fatalf("unexpected signals: %+v", after.Signals)
	}
	if after.Signals[0].Risk != 0.85 || !after.Signals[0].Flagged {
		t.Fatalf("unexpected signal contents: %+v", after.Signals[0])
	}

	// A new event after scoring makes the subject stale again. A short
	// sleep guarantees this event's `at` (real wall-clock time) lands
	// strictly after current_scored_at (the DB's now() from UpsertVerdicts
	// above), rather than racing it at sub-millisecond precision.
	time.Sleep(10 * time.Millisecond)
	e2 := mkEvent(t, "evt_2", "acct_score_me", "resource.created", time.Now().UTC(), map[string]any{"kind": "agent", "name": "b"})
	if _, err := s.AppendEvents(ctx, testTenant, "e2a-server", []event.Event{e2}); err != nil {
		t.Fatalf("AppendEvents: %v", err)
	}
	stale, err := s.SubjectView(ctx, testTenant, "acct_score_me", nil)
	if err != nil {
		t.Fatalf("SubjectView (stale): %v", err)
	}
	if !stale.Stale {
		t.Fatalf("expected the subject to become stale after a new event")
	}
	if stale.EventsSinceScore != 1 {
		t.Fatalf("expected EventsSinceScore=1, got %d", stale.EventsSinceScore)
	}
}

func TestSubjectView_DegradedWhenAdviseRuleUnscored(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-09-27T12:00:00Z")

	e := mkEvent(t, "evt_1", "acct_degraded", "resource.created", now, map[string]any{"kind": "agent", "name": "a"})
	if _, err := s.AppendEvents(ctx, testTenant, "e2a-server", []event.Event{e}); err != nil {
		t.Fatalf("AppendEvents: %v", err)
	}

	records := []store.VerdictRecord{
		{Rule: "advise_rule", Mode: "advise", Scorer: "jev", Status: "unscored", ErrorCode: "cost_cap", InputHash: "h"},
	}
	if _, err := s.UpsertVerdicts(ctx, testTenant, "acct_degraded", 1, records, store.SubjectSummary{Tier: "unknown", Score: 0}); err != nil {
		t.Fatalf("UpsertVerdicts: %v", err)
	}

	view, err := s.SubjectView(ctx, testTenant, "acct_degraded", nil)
	if err != nil {
		t.Fatalf("SubjectView: %v", err)
	}
	if !view.Degraded {
		t.Fatalf("expected degraded=true when an advise rule is unscored")
	}
}

func TestSubjectView_NotFoundForUnseenSubject(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_, err := s.SubjectView(ctx, testTenant, "never_seen_subject", nil)
	if err != store.ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestPutLabel(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	id, err := s.PutLabel(ctx, testTenant, store.Label{
		Subject: "acct_labelled", Label: "abusive", Source: "operator", Actor: "ops@example.test",
	})
	if err != nil {
		t.Fatalf("PutLabel: %v", err)
	}
	if id == 0 {
		t.Fatalf("expected a non-zero label id")
	}
}

// --- S4: Stale is sequence-based, not clock-based ---------------------

// TestSubjectView_StaleIsSequenceBasedNotClockBased is S4: Stale must
// come from dirty_seq > scored_seq alone. A fictional test timestamp in
// "the future" relative to the real wall clock (2031, per this repo's
// convention of only ever using fictional dates) makes the OLD
// last_event_at-vs-current_scored_at comparison wrong 100% of the time
// (not just flaky under clock skew): the event's `at` is unconditionally
// "after" Postgres's real now(), so the old code reported a freshly
// scored subject as stale.
func TestSubjectView_StaleIsSequenceBasedNotClockBased(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-01-01T00:00:00Z")

	e := mkEvent(t, "evt_1", "acct_seq_stale", "resource.created", now, map[string]any{"kind": "agent", "name": "a"})
	if _, err := s.AppendEvents(ctx, testTenant, "e2a-server", []event.Event{e}); err != nil {
		t.Fatalf("AppendEvents: %v", err)
	}

	before, err := s.SubjectView(ctx, testTenant, "acct_seq_stale", nil)
	if err != nil {
		t.Fatalf("SubjectView (before): %v", err)
	}
	dirtySeqAtStart := before.EventsSinceScore

	risk := 0.5
	records := []store.VerdictRecord{
		{Rule: "r", Mode: "advise", Scorer: "local", Model: "local", Probs: map[string]float64{"benign": 0.5, "abusive": 0.5},
			Risk: &risk, InputHash: "h", Status: "scored"},
	}
	if _, err := s.UpsertVerdicts(ctx, testTenant, "acct_seq_stale", dirtySeqAtStart, records, store.SubjectSummary{Tier: "medium", Score: risk}); err != nil {
		t.Fatalf("UpsertVerdicts: %v", err)
	}

	after, err := s.SubjectView(ctx, testTenant, "acct_seq_stale", nil)
	if err != nil {
		t.Fatalf("SubjectView (after): %v", err)
	}
	if after.Stale {
		t.Fatalf("expected a subject scored with no new events since to be Stale=false, even though its only event's `at` (2031, a fictional future date) is after Postgres's real now()")
	}
}

// --- S5: UpsertVerdicts must not let a stale round regress the summary --

// TestUpsertVerdicts_OutOfOrderRoundDoesNotRegressSummary is S5/R2
// (round 2 tightened this further). Proven (S5): a round for
// dirty_seq=3 recording tier=high, followed by a round for dirty_seq=1
// (an out-of-order/slow worker instance that read the subject's
// dirty_seq before the first round even started) recording tier=low,
// used to leave the subject at tier=low. Proven (R2, round 2): even
// after S5's fix, the stale round's own verdict ROWS still committed and
// became SubjectView's "latest signal per rule" (scored_at/id order
// favors the later-COMMITTING call, not the logically-newer one) — a
// caller could see current_tier="high" right beside a signal showing the
// stale round's risk=0.1/unflagged. UpsertVerdicts now rolls back the
// whole transaction (verdict inserts included) and returns the
// ErrStaleRound sentinel when a round turns out to be stale, so nothing
// about it ever becomes visible.
func TestUpsertVerdicts_OutOfOrderRoundDoesNotRegressSummary(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-01-01T00:00:00Z")

	for i := 0; i < 3; i++ {
		e := mkEvent(t, fmt.Sprintf("evt_%d", i), "acct_ooo_round", "resource.created", now.Add(time.Duration(i)*time.Second),
			map[string]any{"kind": "agent", "name": "a"})
		if _, err := s.AppendEvents(ctx, testTenant, "e2a-server", []event.Event{e}); err != nil {
			t.Fatalf("AppendEvents: %v", err)
		}
	}

	highRisk := 0.9
	highRecords := []store.VerdictRecord{
		{Rule: "r", Mode: "advise", Scorer: "local", Model: "local", Probs: map[string]float64{"benign": 0.1, "abusive": 0.9},
			Risk: &highRisk, Flagged: true, InputHash: "h_high", Status: "scored"},
	}
	if _, err := s.UpsertVerdicts(ctx, testTenant, "acct_ooo_round", 3, highRecords, store.SubjectSummary{Tier: "high", Score: highRisk}); err != nil {
		t.Fatalf("UpsertVerdicts (seq=3, high): %v", err)
	}
	before, err := s.SubjectView(ctx, testTenant, "acct_ooo_round", nil)
	if err != nil {
		t.Fatalf("SubjectView (before stale round): %v", err)
	}

	lowRisk := 0.1
	lowRecords := []store.VerdictRecord{
		{Rule: "r", Mode: "advise", Scorer: "local", Model: "local", Probs: map[string]float64{"benign": 0.9, "abusive": 0.1},
			Risk: &lowRisk, InputHash: "h_low", Status: "scored"},
	}
	_, err = s.UpsertVerdicts(ctx, testTenant, "acct_ooo_round", 1, lowRecords, store.SubjectSummary{Tier: "low", Score: lowRisk})
	if !errors.Is(err, store.ErrStaleRound) {
		t.Fatalf("expected ErrStaleRound from the out-of-order seq=1 round, got %v", err)
	}

	view, err := s.SubjectView(ctx, testTenant, "acct_ooo_round", nil)
	if err != nil {
		t.Fatalf("SubjectView: %v", err)
	}
	if view.Tier != "high" {
		t.Fatalf("expected the out-of-order seq=1 round to leave tier=high in place, got tier=%s", view.Tier)
	}
	if len(view.Signals) != 1 || view.Signals[0].Risk != highRisk || !view.Signals[0].Flagged {
		t.Fatalf("expected the stale round's verdict row to never become visible, got signals: %+v", view.Signals)
	}
	if view.ScoredAt == nil || before.ScoredAt == nil || !view.ScoredAt.Equal(*before.ScoredAt) {
		t.Fatalf("expected current_scored_at to be untouched by the rolled-back stale round: before=%v after=%v", before.ScoredAt, view.ScoredAt)
	}
}

// TestUpsertVerdicts_MissingSubjectIsAnError is S5: recording verdicts for
// a subject row that doesn't exist must be a reported error, not a
// silent no-op that leaves the caller believing the round was recorded.
func TestUpsertVerdicts_MissingSubjectIsAnError(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	risk := 0.5
	records := []store.VerdictRecord{
		{Rule: "r", Mode: "advise", Scorer: "local", Model: "local", Probs: map[string]float64{"benign": 0.5, "abusive": 0.5},
			Risk: &risk, InputHash: "h", Status: "scored"},
	}
	_, err := s.UpsertVerdicts(ctx, testTenant, "acct_never_created", 1, records, store.SubjectSummary{Tier: "medium", Score: risk})
	if err == nil {
		t.Fatalf("expected an error recording verdicts for a subject that was never created")
	}
}

// --- S7: first_seen uses LEAST(existing, new) --------------------------

func subjectFirstSeen(t *testing.T, ctx context.Context, tenant, subject string) time.Time {
	t.Helper()
	pool := openScopedPool(t, ctx)
	var firstSeen time.Time
	if err := pool.QueryRow(ctx, `SELECT first_seen_at FROM subjects WHERE tenant = $1 AND subject = $2`, tenant, subject).Scan(&firstSeen); err != nil {
		t.Fatalf("query subjects.first_seen_at: %v", err)
	}
	return firstSeen
}

func linkFirstSeen(t *testing.T, ctx context.Context, tenant, kind, hash, subject string) time.Time {
	t.Helper()
	pool := openScopedPool(t, ctx)
	var firstSeen time.Time
	if err := pool.QueryRow(ctx,
		`SELECT first_seen FROM links WHERE tenant = $1 AND kind = $2 AND hash = $3 AND subject = $4`,
		tenant, kind, hash, subject,
	).Scan(&firstSeen); err != nil {
		t.Fatalf("query links.first_seen: %v", err)
	}
	return firstSeen
}

// TestAppendEvents_SubjectFirstSeenUsesEarliestTimestamp is S7. Proven:
// out-of-order delivery (a later event's `at` arrives and is stored
// first, then an earlier one arrives) left subjects.first_seen_at at the
// LATER time instead of the true earliest.
func TestAppendEvents_SubjectFirstSeenUsesEarliestTimestamp(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	earlier := mustTime(t, "2031-06-01T00:00:00Z")
	later := earlier.Add(10 * time.Minute)

	eLater := mkEvent(t, "evt_later", "acct_ooo_subject", "resource.created", later, map[string]any{"kind": "agent", "name": "a"})
	if _, err := s.AppendEvents(ctx, testTenant, "e2a-server", []event.Event{eLater}); err != nil {
		t.Fatalf("AppendEvents (later, delivered first): %v", err)
	}
	eEarlier := mkEvent(t, "evt_earlier", "acct_ooo_subject", "resource.created", earlier, map[string]any{"kind": "agent", "name": "b"})
	if _, err := s.AppendEvents(ctx, testTenant, "e2a-server", []event.Event{eEarlier}); err != nil {
		t.Fatalf("AppendEvents (earlier, delivered second): %v", err)
	}

	got := subjectFirstSeen(t, ctx, testTenant, "acct_ooo_subject")
	if !got.Equal(earlier) {
		t.Fatalf("expected first_seen_at to be the earliest event's `at` (%s) despite out-of-order delivery, got %s", earlier, got)
	}
}

// TestAppendEvents_LinkFirstSeenUsesEarliestTimestamp is S7's links half.
func TestAppendEvents_LinkFirstSeenUsesEarliestTimestamp(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	earlier := mustTime(t, "2031-06-01T00:00:00Z")
	later := earlier.Add(10 * time.Minute)
	hash := "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

	eLater := event.Event{ID: "evt_link_later", Subject: "acct_ooo_link", Type: "subject.created", At: later, Links: event.Links{EmailHash: hash}}
	if err := eLater.Validate(event.ValidateOptions{Now: later}); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if err := eLater.Redact(); err != nil {
		t.Fatalf("Redact: %v", err)
	}
	eEarlier := event.Event{ID: "evt_link_earlier", Subject: "acct_ooo_link", Type: "subject.created", At: earlier, Links: event.Links{EmailHash: hash}}
	if err := eEarlier.Validate(event.ValidateOptions{Now: earlier}); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if err := eEarlier.Redact(); err != nil {
		t.Fatalf("Redact: %v", err)
	}

	if _, err := s.AppendEvents(ctx, testTenant, "e2a-server", []event.Event{eLater}); err != nil {
		t.Fatalf("AppendEvents (later, delivered first): %v", err)
	}
	if _, err := s.AppendEvents(ctx, testTenant, "e2a-server", []event.Event{eEarlier}); err != nil {
		t.Fatalf("AppendEvents (earlier, delivered second): %v", err)
	}

	got := linkFirstSeen(t, ctx, testTenant, "email_hash", hash, "acct_ooo_link")
	if !got.Equal(earlier) {
		t.Fatalf("expected links.first_seen to be the earliest `at` (%s) despite out-of-order delivery, got %s", earlier, got)
	}
}

// --- S13: Neighbors index, loop bound, and recency ordering ------------

func TestNeighbors_PrefersMostRecentlySeenWhenTruncated(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	base := mustTime(t, "2031-01-01T00:00:00Z")
	hash := "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"

	// Five neighbors of "acct_query", inserted in an order that puts the
	// ALPHABETICALLY FIRST subjects at the OLDEST last_seen — so "order by
	// subject" (alphabetical) and "order by last_seen DESC" (recency)
	// disagree about which 2 survive a capPerKey=2 truncation.
	names := []string{"n1", "n2", "n3", "n4", "n5"}
	var events []event.Event
	events = append(events, mustLinkEvent(t, "evt_q", "acct_query", base, hash))
	for i, name := range names {
		events = append(events, mustLinkEvent(t, "evt_"+name, name, base.Add(time.Duration(i+1)*time.Minute), hash))
	}
	if _, err := s.AppendEvents(ctx, testTenant, "e2a-server", events); err != nil {
		t.Fatalf("AppendEvents: %v", err)
	}

	neighbors, truncated, err := s.Neighbors(ctx, testTenant, "acct_query", 2, 200)
	if err != nil {
		t.Fatalf("Neighbors: %v", err)
	}
	if !truncated {
		t.Fatalf("expected truncated=true with 5 neighbors and capPerKey=2")
	}
	want := map[string]bool{"n5": true, "n4": true} // most recently seen
	for _, n := range neighbors {
		if !want[n] {
			t.Fatalf("expected the 2 MOST RECENT neighbors (n5, n4), got %v (alphabetical-order code would return n1, n2)", neighbors)
		}
	}
}

func mustLinkEvent(t *testing.T, id, subject string, at time.Time, hash string) event.Event {
	t.Helper()
	e := event.Event{ID: id, Subject: subject, Type: "subject.created", At: at, Links: event.Links{EmailHash: hash}}
	if err := e.Validate(event.ValidateOptions{Now: at}); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if err := e.Redact(); err != nil {
		t.Fatalf("Redact: %v", err)
	}
	return e
}

// TestNeighbors_KindHashLastSeenIndexExists is R4 (round 2): Neighbors'
// per-key query ORDER BY last_seen DESC had no supporting index, forcing
// a full sort of every row for a hot (kind, hash) key on every call.
func TestNeighbors_KindHashLastSeenIndexExists(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_ = s
	pool := openScopedPool(t, ctx)
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM pg_indexes WHERE schemaname = $1 AND tablename = 'links' AND indexdef LIKE '%last_seen%')`,
		runSchema,
	).Scan(&exists); err != nil {
		t.Fatalf("query pg_indexes: %v", err)
	}
	if !exists {
		t.Fatalf("expected an index covering links(tenant, kind, hash, last_seen) to support Neighbors' ORDER BY")
	}
}

// TestNeighbors_DeterministicAcrossRepeatedCalls is R4 (round 2)'s
// contract test: neither the per-key neighbor query (ORDER BY last_seen
// DESC alone, with several neighbors sharing an identical last_seen) nor
// the outer key-list query (SELECT DISTINCT kind, hash with no ORDER BY
// at all, with two different link keys) is guaranteed a deterministic
// order by the SQL standard — Postgres is free to return tied or
// unordered rows in any sequence, so which neighbors survive truncation
// could vary run to run (a different query plan, a parallel worker, a
// vacuum-reordered heap) even against unchanged data, even though a
// same-process repeated call in this test tends to observe a stable
// order either way (Postgres doesn't literally shuffle an unchanged
// table between two back-to-back queries on the same connection) —
// this asserts the now-explicit, index-backed ORDER BY clauses rather
// than relying on that incidental stability.
func TestNeighbors_DeterministicAcrossRepeatedCalls(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	base := mustTime(t, "2031-01-01T00:00:00Z")
	hashA := "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	hashB := "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"

	var events []event.Event
	events = append(events, mustLinkEvent(t, "evt_q_a", "acct_query_det", base, hashA))
	events = append(events, mustLinkEvent(t, "evt_q_b", "acct_query_det", base, hashB))
	// Three same-key, same-timestamp neighbors on hashA (capPerKey will be
	// 2, forcing a same-last_seen tiebreak) and two on hashB.
	for _, name := range []string{"na1", "na2", "na3"} {
		events = append(events, mustLinkEvent(t, "evt_"+name, name, base, hashA))
	}
	for _, name := range []string{"nb1", "nb2"} {
		events = append(events, mustLinkEvent(t, "evt_"+name, name, base, hashB))
	}
	if _, err := s.AppendEvents(ctx, testTenant, "e2a-server", events); err != nil {
		t.Fatalf("AppendEvents: %v", err)
	}

	first, firstTruncated, err := s.Neighbors(ctx, testTenant, "acct_query_det", 2, 3)
	if err != nil {
		t.Fatalf("Neighbors (1st): %v", err)
	}
	for i := 0; i < 5; i++ {
		got, gotTruncated, err := s.Neighbors(ctx, testTenant, "acct_query_det", 2, 3)
		if err != nil {
			t.Fatalf("Neighbors (repeat %d): %v", i, err)
		}
		if gotTruncated != firstTruncated || len(got) != len(first) {
			t.Fatalf("non-deterministic Neighbors shape: first=%v/%v, repeat %d=%v/%v", first, firstTruncated, i, got, gotTruncated)
		}
		for j := range got {
			if got[j] != first[j] {
				t.Fatalf("non-deterministic Neighbors order: first=%v, repeat %d=%v", first, i, got)
			}
		}
	}
}

func TestNeighbors_TenantSubjectIndexExists(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_ = s // ensure migrations have run in THIS run's schema
	pool := openScopedPool(t, ctx)
	// pg_indexes has a schemaname column and isn't itself affected by
	// search_path, so this must filter to our own run's schema
	// explicitly — otherwise a leftover schema from an earlier,
	// imperfectly-cleaned-up run would make this pass regardless of
	// whether THIS run's migrations actually created the index.
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM pg_indexes WHERE schemaname = $1 AND tablename = 'links' AND indexdef LIKE '%(tenant, subject)%')`,
		runSchema,
	).Scan(&exists); err != nil {
		t.Fatalf("query pg_indexes: %v", err)
	}
	if !exists {
		t.Fatalf("expected an index on links(tenant, subject) to support Neighbors' reverse lookup")
	}
}

// --- S14: SubjectView only reflects rules in the current config --------

func TestSubjectView_FiltersSignalsToCurrentConfigRules(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-01-01T00:00:00Z")

	e := mkEvent(t, "evt_1", "acct_filter_rules", "resource.created", now, map[string]any{"kind": "agent", "name": "a"})
	if _, err := s.AppendEvents(ctx, testTenant, "e2a-server", []event.Event{e}); err != nil {
		t.Fatalf("AppendEvents: %v", err)
	}

	riskCurrent, riskRetired := 0.9, 0.95
	records := []store.VerdictRecord{
		{Rule: "current_rule", Mode: "advise", Scorer: "local", Model: "local", Probs: map[string]float64{"benign": 0.1},
			Risk: &riskCurrent, Flagged: true, InputHash: "h1", Status: "scored"},
		{Rule: "retired_rule", Mode: "advise", Scorer: "local", Model: "local", Probs: map[string]float64{"benign": 0.05},
			Risk: &riskRetired, Flagged: true, InputHash: "h2", Status: "scored"},
	}
	if _, err := s.UpsertVerdicts(ctx, testTenant, "acct_filter_rules", 1, records, store.SubjectSummary{Tier: "high", Score: riskRetired}); err != nil {
		t.Fatalf("UpsertVerdicts: %v", err)
	}

	view, err := s.SubjectView(ctx, testTenant, "acct_filter_rules", []store.CurrentRule{{Name: "current_rule", Advise: true}})
	if err != nil {
		t.Fatalf("SubjectView: %v", err)
	}
	if len(view.Signals) != 1 || view.Signals[0].Rule != "current_rule" {
		t.Fatalf("expected only current_rule's signal once retired_rule is dropped from config, got %+v", view.Signals)
	}

	viewAll, err := s.SubjectView(ctx, testTenant, "acct_filter_rules", nil)
	if err != nil {
		t.Fatalf("SubjectView (nil filter): %v", err)
	}
	if len(viewAll.Signals) != 2 {
		t.Fatalf("expected a nil currentRules filter to return every rule's signal, got %+v", viewAll.Signals)
	}
}

func TestSubjectView_DegradedOnlyFromCurrentConfigRules(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-01-01T00:00:00Z")

	e := mkEvent(t, "evt_1", "acct_filter_degraded", "resource.created", now, map[string]any{"kind": "agent", "name": "a"})
	if _, err := s.AppendEvents(ctx, testTenant, "e2a-server", []event.Event{e}); err != nil {
		t.Fatalf("AppendEvents: %v", err)
	}

	records := []store.VerdictRecord{
		{Rule: "retired_rule", Mode: "advise", Scorer: "jev", Status: "unscored", ErrorCode: "cost_cap", InputHash: "h"},
	}
	if _, err := s.UpsertVerdicts(ctx, testTenant, "acct_filter_degraded", 1, records, store.SubjectSummary{Tier: "unknown", Score: 0}); err != nil {
		t.Fatalf("UpsertVerdicts: %v", err)
	}

	// some_other_current_rule is deliberately Advise: false (shadow) here —
	// this sub-case is specifically about a RETIRED rule's own unscored
	// signal not degrading once excluded by name; a shadow decoy entry
	// with no recorded verdict at all must not itself trigger T4's
	// separate "missing ADVISE rule" check (see
	// TestSubjectView_MissingAdviseRuleDegrades for that).
	excluded, err := s.SubjectView(ctx, testTenant, "acct_filter_degraded", []store.CurrentRule{{Name: "some_other_current_rule", Advise: false}})
	if err != nil {
		t.Fatalf("SubjectView (excluded): %v", err)
	}
	if excluded.Degraded {
		t.Fatalf("expected a retired rule's unscored signal, excluded by the current config filter, to not degrade")
	}

	included, err := s.SubjectView(ctx, testTenant, "acct_filter_degraded", []store.CurrentRule{{Name: "retired_rule", Advise: true}})
	if err != nil {
		t.Fatalf("SubjectView (included): %v", err)
	}
	if !included.Degraded {
		t.Fatalf("expected an unscored advise rule that IS in the current config filter to degrade")
	}
}

// TestSubjectView_MissingAdviseRuleDegrades is T4 (round 3): a currently
// configured advise rule with NO recorded verdict at all — not even as
// unscored — must degrade the view exactly like one that IS recorded but
// unscored (design §5: "absence of an advise signal must never look fully
// healthy"). Before this fix, an omitted rule (round 2's R3: a syncOnly
// evaluate round with nothing to carry forward for a non-local rule omits
// it entirely rather than recording it unscored) was invisible to
// SubjectView — it never appeared in the verdicts query at all, so its
// absence looked identical to "this subject has no such rule," silently
// reporting a fully-healthy, non-degraded view missing an advise opinion.
// The subject's current_tier — materialized from whatever WAS actually
// scored — is unaffected; only Degraded changes.
func TestSubjectView_MissingAdviseRuleDegrades(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-01-01T00:00:00Z")

	e := mkEvent(t, "evt_1", "acct_missing_advise", "resource.created", now, map[string]any{"kind": "agent", "name": "a"})
	if _, err := s.AppendEvents(ctx, testTenant, "e2a-server", []event.Event{e}); err != nil {
		t.Fatalf("AppendEvents: %v", err)
	}

	// scored_rule is the only rule that ever actually recorded a verdict —
	// missing_advise_rule is currently configured but has NOTHING recorded
	// for this subject at all.
	risk := 0.9
	records := []store.VerdictRecord{
		{Rule: "scored_rule", Mode: "advise", Scorer: "local", Model: "local", Probs: map[string]float64{"benign": 0.1},
			Risk: &risk, Flagged: true, InputHash: "h1", Status: "scored"},
	}
	if _, err := s.UpsertVerdicts(ctx, testTenant, "acct_missing_advise", 1, records, store.SubjectSummary{Tier: "high", Score: risk}); err != nil {
		t.Fatalf("UpsertVerdicts: %v", err)
	}

	view, err := s.SubjectView(ctx, testTenant, "acct_missing_advise", []store.CurrentRule{
		{Name: "scored_rule", Advise: true},
		{Name: "missing_advise_rule", Advise: true},
	})
	if err != nil {
		t.Fatalf("SubjectView: %v", err)
	}
	if !view.Degraded {
		t.Fatalf("expected a currently-configured advise rule with NO recorded verdict at all to degrade the view")
	}
	if view.Tier != "high" {
		t.Fatalf("expected the tier to still reflect what WAS scored (high), got %q", view.Tier)
	}
	if len(view.Signals) != 1 || view.Signals[0].Rule != "scored_rule" {
		t.Fatalf("expected exactly one signal (the rule that WAS scored) since missing_advise_rule has no row at all, got %+v", view.Signals)
	}

	// A missing SHADOW rule, by contrast, must not degrade — matching the
	// per-row unscored check's own advise-only scope.
	viewShadow, err := s.SubjectView(ctx, testTenant, "acct_missing_advise", []store.CurrentRule{
		{Name: "scored_rule", Advise: true},
		{Name: "missing_shadow_rule", Advise: false},
	})
	if err != nil {
		t.Fatalf("SubjectView (shadow): %v", err)
	}
	if viewShadow.Degraded {
		t.Fatalf("expected a missing SHADOW rule to NOT degrade the view")
	}
}

func TestSubjectView_DistinctOnTiebreaksByHighestID(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-01-01T00:00:00Z")

	e := mkEvent(t, "evt_1", "acct_tiebreak", "resource.created", now, map[string]any{"kind": "agent", "name": "a"})
	if _, err := s.AppendEvents(ctx, testTenant, "e2a-server", []event.Event{e}); err != nil {
		t.Fatalf("AppendEvents: %v", err)
	}

	// Two records for the SAME rule in the SAME UpsertVerdicts call share
	// an identical scored_at (Postgres's now() is stable within one
	// transaction), so DISTINCT ON (rule) ORDER BY rule, scored_at DESC
	// alone cannot deterministically pick between them — id DESC must
	// break the tie toward the later-inserted (higher id) row.
	riskA, riskB := 0.1, 0.9
	records := []store.VerdictRecord{
		{Rule: "r", Mode: "advise", Scorer: "local", Model: "local", Probs: map[string]float64{"benign": 0.9},
			Risk: &riskA, InputHash: "h1", Status: "scored"},
		{Rule: "r", Mode: "advise", Scorer: "local", Model: "local", Probs: map[string]float64{"benign": 0.1},
			Risk: &riskB, Flagged: true, InputHash: "h2", Status: "scored"},
	}
	if _, err := s.UpsertVerdicts(ctx, testTenant, "acct_tiebreak", 1, records, store.SubjectSummary{Tier: "high", Score: riskB}); err != nil {
		t.Fatalf("UpsertVerdicts: %v", err)
	}

	view, err := s.SubjectView(ctx, testTenant, "acct_tiebreak", nil)
	if err != nil {
		t.Fatalf("SubjectView: %v", err)
	}
	if len(view.Signals) != 1 {
		t.Fatalf("expected exactly one signal for rule %q (DISTINCT ON), got %d: %+v", "r", len(view.Signals), view.Signals)
	}
	if view.Signals[0].Risk != riskB {
		t.Fatalf("expected the scored_at tie to break toward the higher (later-inserted) verdict id, risk=%v, got risk=%v", riskB, view.Signals[0].Risk)
	}
}
