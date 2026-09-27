package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

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
// DB" acceptance criterion from plan.md's S1 row: newTestStore already
// calls ApplyMigrations as part of setup, so a passing test here means a
// completely empty database reaches a working schema, and running it
// again (idempotent) doesn't error either.
func TestApplyMigrations_FreshDatabase(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.ApplyMigrations(ctx); err != nil {
		t.Fatalf("re-applying migrations should be a no-op, got: %v", err)
	}
}

func TestAppendEvents_AcceptsNewEvents(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2026-09-27T12:00:00Z")

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

func TestAppendEvents_DuplicateVsConflict(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2026-09-27T12:00:00Z")

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
	now := mustTime(t, "2026-09-27T12:00:00Z")

	for i := 0; i < 3; i++ {
		e := mkEvent(t, fmt.Sprintf("evt_%d", i), "acct_example_3", "resource.created", now.Add(time.Duration(i)*time.Second),
			map[string]any{"kind": "agent", "name": "a"})
		if _, err := s.AppendEvents(ctx, testTenant, "e2a-server", []event.Event{e}); err != nil {
			t.Fatalf("AppendEvents: %v", err)
		}
	}

	view, err := s.SubjectView(ctx, testTenant, "acct_example_3")
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
	now := mustTime(t, "2026-09-27T12:00:00Z")

	create := mkEvent(t, "evt_create", "mon-a", "subject.created", now, map[string]any{"channel": "api"})
	classify := mkEvent(t, "evt_class", "mon-a", "subject.class", now.Add(time.Second), map[string]any{"class": "synthetic"})

	if _, err := s.AppendEvents(ctx, testTenant, "e2a-server", []event.Event{create, classify}); err != nil {
		t.Fatalf("AppendEvents: %v", err)
	}

	view, err := s.SubjectView(ctx, testTenant, "mon-a")
	if err != nil {
		t.Fatalf("SubjectView: %v", err)
	}
	if view.Class != "synthetic" {
		t.Fatalf("expected class=synthetic, got %s", view.Class)
	}
}

func TestAppendEvents_UpsertsLinks(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2026-09-27T12:00:00Z")
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
	now := mustTime(t, "2026-09-27T12:00:00Z")
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

	before, err := s.SubjectView(ctx, testTenant, "acct_score_me")
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

	after, err := s.SubjectView(ctx, testTenant, "acct_score_me")
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
	stale, err := s.SubjectView(ctx, testTenant, "acct_score_me")
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
	now := mustTime(t, "2026-09-27T12:00:00Z")

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

	view, err := s.SubjectView(ctx, testTenant, "acct_degraded")
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
	_, err := s.SubjectView(ctx, testTenant, "never_seen_subject")
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
