package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tokencanopy/abusekit/internal/event"
	"github.com/tokencanopy/abusekit/internal/feature"
	"github.com/tokencanopy/abusekit/internal/model/fake"
	"github.com/tokencanopy/abusekit/internal/store"
)

// TestEvaluateSubject_FastFixtureReachesHighBeforeFirstSend is design §1
// success criterion 2(b) / plan.md's S3 "Done when": eval/fixtures/
// fast.jsonl compresses burst.jsonl's exact shape (fraud-declined
// attempts, a prepaid success, a quick upgrade, a resource burst,
// self-send rehearsal) so its first EXTERNAL content.sent lands 30s after
// signup — "agents and first send inside one minute". This replays only
// the setup events (everything before that first external send) through
// the SYNCHRONOUS POST .../evaluate path (never Tick), proving the
// operator scenario reaches `high` on the call a product makes before its
// first external send, and records the call's own latency as the "fast"
// criterion's evidence against design's <=3000ms deadline ceiling.
func TestEvaluateSubject_FastFixtureReachesHighBeforeFirstSend(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	cfg := loadShippedConfig(t)

	all := loadFixture(t, "../../eval/fixtures/fast.jsonl")
	var setup []event.Event
	for _, e := range all {
		if e.Type == "content.sent" && !isSelfSend(e) {
			break
		}
		setup = append(setup, e)
	}
	if len(setup) == 0 || len(setup) == len(all) {
		t.Fatalf("fast.jsonl fixture shape assumption broken: got %d setup events of %d total", len(setup), len(all))
	}
	firstExternalSendAt := firstExternalSendTime(t, all)
	signupAt := all[0].At
	if window := firstExternalSendAt.Sub(signupAt); window > time.Minute {
		t.Fatalf("fast.jsonl fixture shape assumption broken: first external send lands %v after signup, want <= 1 minute", window)
	}
	ingestFixture(t, ctx, s, setup)

	now := lastEventAt(setup).Add(time.Second)
	if !now.Before(firstExternalSendAt) {
		t.Fatalf("fixture timing assumption broken: evaluate instant (%v) is not before the first external send (%v)", now, firstExternalSendAt)
	}
	w, err := New(Deps{Store: s, Config: cfg, Neighbors: feature.NoNeighbors, Brands: loadShippedBrands(t), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	start := time.Now()
	verdict, err := w.EvaluateSubject(ctx, testTenant, "acct_example_fast_1", 3*time.Second)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("EvaluateSubject: %v", err)
	}
	if verdict.Tier != "high" {
		t.Errorf("fast.jsonl evaluate (before first external send): tier = %q (score %v), want high\nsignals: %+v", verdict.Tier, verdict.Score, verdict.Signals)
	}
	if verdict.Degraded {
		t.Errorf("fast.jsonl evaluate: degraded = true, want false (the local rule should always answer)")
	}
	// "the design's latency target": the local scorer is a pure in-process
	// computation with no network call, so a synchronous evaluate call
	// against it should complete in low milliseconds, nowhere near the
	// {deadline_ms <= 3000} ceiling design allows.
	if elapsed > time.Second {
		t.Errorf("evaluate took %v, want comfortably under the 3s deadline (local scorer only)", elapsed)
	}
	t.Logf("fast.jsonl evaluate latency: %v (design's deadline ceiling: 3s)", elapsed)

	view, err := s.SubjectView(ctx, testTenant, "acct_example_fast_1", nil)
	if err != nil {
		t.Fatalf("SubjectView: %v", err)
	}
	if view.Tier != "high" {
		t.Fatalf("expected the committed verdict to also read back as high, got %q", view.Tier)
	}
}

func isSelfSend(e event.Event) bool {
	own, _ := e.Data["recipient_is_own_identity"].(bool)
	return own
}

func firstExternalSendTime(t *testing.T, events []event.Event) time.Time {
	t.Helper()
	for _, e := range events {
		if e.Type == "content.sent" && !isSelfSend(e) {
			return e.At
		}
	}
	t.Fatalf("fixture has no external content.sent event")
	return time.Time{}
}

// TestEvaluateSubject_NotFoundForUnseenSubject proves EvaluateSubject
// surfaces store.ErrNotFound for a subject that has never been ingested
// (design §4.4: 404 only for a subject never seen).
func TestEvaluateSubject_NotFoundForUnseenSubject(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	cfg := loadShippedConfig(t)
	now := time.Date(2031, time.January, 1, 0, 0, 0, 0, time.UTC)
	w, err := New(Deps{Store: s, Config: cfg, Neighbors: feature.NoNeighbors, Brands: loadShippedBrands(t), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = w.EvaluateSubject(ctx, testTenant, "never_seen_eval", 3*time.Second)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected store.ErrNotFound, got %v", err)
	}
}

// TestEvaluateSubject_AlreadyClaimedSurfacesError proves a subject the
// worker's own Tick has already claimed cannot also be claimed by a
// concurrent evaluate call (design: evaluate shares "the same lease...
// rules" as the worker's own scoring pass).
func TestEvaluateSubject_AlreadyClaimedSurfacesError(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	cfg := loadShippedConfig(t)
	now := time.Date(2031, time.January, 1, 0, 0, 0, 0, time.UTC)

	appendEvent(t, ctx, s, "acct_eval_claimed", "subject.created", now, event.Links{}, map[string]any{"channel": "signup"})

	if _, err := s.ClaimDirtySubjects(ctx, now, 0); err != nil {
		t.Fatalf("ClaimDirtySubjects: %v", err)
	}

	w, err := New(Deps{Store: s, Config: cfg, Neighbors: feature.NoNeighbors, Brands: loadShippedBrands(t), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = w.EvaluateSubject(ctx, testTenant, "acct_eval_claimed", 3*time.Second)
	if !errors.Is(err, store.ErrAlreadyClaimed) {
		t.Fatalf("expected store.ErrAlreadyClaimed, got %v", err)
	}
}

// TestEvaluateSubject_SyncOnlySkipsNonLocalScorer proves the syncOnly
// restriction (design: "the local scorer always can; vendor scorers only
// if their p99 fits" — v0 has no measured p99 for any vendor scorer): a
// rule scored by anything other than "local" is reported unscored with
// "sync_scorer_unsupported" from EvaluateSubject, even though the SAME
// rule scores normally from Tick.
func TestEvaluateSubject_SyncOnlySkipsNonLocalScorer(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2031, time.January, 1, 0, 0, 0, 0, time.UTC)

	appendEvent(t, ctx, s, "acct_eval_nonlocal", "subject.created", now, event.Links{}, map[string]any{"channel": "signup"})

	scorer := fake.New()
	cfg := newFakeRuleConfig(t, scorer)

	w, err := New(Deps{Store: s, Config: cfg, Neighbors: feature.NoNeighbors, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	verdict, err := w.EvaluateSubject(ctx, testTenant, "acct_eval_nonlocal", 3*time.Second)
	if err != nil {
		t.Fatalf("EvaluateSubject: %v", err)
	}
	if len(verdict.Signals) != 1 {
		t.Fatalf("expected exactly one signal, got %+v", verdict.Signals)
	}
	sig := verdict.Signals[0]
	if sig.Status != "unscored" || sig.ErrorCode != "sync_scorer_unsupported" {
		t.Fatalf("expected unscored/sync_scorer_unsupported for a non-local rule under evaluate, got %+v", sig)
	}

	// The SAME rule DOES score normally via the ordinary Tick path (not
	// syncOnly) — proving the restriction is evaluate-specific, not a
	// config or scorer problem. EvaluateSubject's own successful commit
	// above already consumed the dirty_seq bump from the setup event, so a
	// second event is needed to make the subject dirty again before Tick
	// has anything to claim.
	appendEvent(t, ctx, s, "acct_eval_nonlocal", "resource.created", now.Add(time.Minute), event.Links{}, map[string]any{"kind": "agent", "name": "a"})
	result, err := w.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if result.Scored != 1 || len(result.Errors) != 0 {
		t.Fatalf("Tick result = %+v, want exactly one subject scored with no errors", result)
	}
	view, err := s.SubjectView(ctx, testTenant, "acct_eval_nonlocal", nil)
	if err != nil {
		t.Fatalf("SubjectView: %v", err)
	}
	if len(view.Signals) != 1 || view.Signals[0].Status != "scored" {
		t.Fatalf("expected the fake rule to score normally via Tick, got %+v", view.Signals)
	}
}

// delayingStore wraps a real Store and sleeps for delay before every
// EventsForSubject call — a deterministic way to prove EvaluateSubject's
// deadline bounds the WHOLE synchronous call (design: "scores the subject
// now using rules whose scorer can answer within the deadline"), not just
// an individual scorer.Score invocation, without depending on real
// Postgres latency (which is normally far too fast, and never reliably
// slow, to exercise a deadline test deterministically).
type delayingStore struct {
	Store
	delay time.Duration
}

func (d delayingStore) EventsForSubject(ctx context.Context, tenant, subject string) ([]store.StoredEvent, error) {
	select {
	case <-time.After(d.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return d.Store.EventsForSubject(ctx, tenant, subject)
}

// TestEvaluateSubject_RespectsDeadline proves a deadline shorter than the
// round's own latency surfaces an error rather than blocking past it, and
// that a deadline comfortably longer than that latency succeeds.
func TestEvaluateSubject_RespectsDeadline(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2031, time.January, 1, 0, 0, 0, 0, time.UTC)
	cfg := loadShippedConfig(t)

	appendEvent(t, ctx, s, "acct_eval_deadline_exceeded", "subject.created", now, event.Links{}, map[string]any{"channel": "signup"})
	slow := delayingStore{Store: s, delay: 200 * time.Millisecond}
	w, err := New(Deps{Store: slow, Config: cfg, Neighbors: feature.NoNeighbors, Brands: loadShippedBrands(t), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := w.EvaluateSubject(ctx, testTenant, "acct_eval_deadline_exceeded", 20*time.Millisecond); err == nil {
		t.Fatalf("expected a 20ms deadline against a 200ms-delayed store to fail, got no error")
	}

	appendEvent(t, ctx, s, "acct_eval_deadline_ok", "subject.created", now, event.Links{}, map[string]any{"channel": "signup"})
	w2, err := New(Deps{Store: slow, Config: cfg, Neighbors: feature.NoNeighbors, Brands: loadShippedBrands(t), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	verdict, err := w2.EvaluateSubject(ctx, testTenant, "acct_eval_deadline_ok", 3*time.Second)
	if err != nil {
		t.Fatalf("expected a 3s deadline against a 200ms-delayed store to succeed, got %v", err)
	}
	if len(verdict.Signals) != 1 {
		t.Fatalf("expected exactly one signal, got %+v", verdict.Signals)
	}
}
