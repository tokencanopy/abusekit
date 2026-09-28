package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tokencanopy/abusekit/internal/event"
	"github.com/tokencanopy/abusekit/internal/feature"
	"github.com/tokencanopy/abusekit/internal/model"
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
//
// EvaluateSubject itself only reports whether a fresh round committed (S1
// fix round: the caller always finishes with its own store.SubjectView
// read, exactly like GET /v1/subjects/{subject} does) — so this test reads
// the committed view back the same way internal/serve's handler will.
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
	evaluatedNow, err := w.EvaluateSubject(ctx, testTenant, "acct_example_fast_1", 3*time.Second)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("EvaluateSubject: %v", err)
	}
	if !evaluatedNow {
		t.Fatalf("expected evaluatedNow=true")
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
		t.Fatalf("expected the committed verdict to read back as high, got %q\nsignals: %+v", view.Tier, view.Signals)
	}
	if view.Degraded {
		t.Errorf("fast.jsonl evaluate: degraded = true, want false (the local rule should always answer)")
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

// TestEvaluateSubject_SyntheticSubjectIsNotScorable is S1: e2a's own
// prober accounts are synthetic — evaluate must report ErrNotScorable
// (never a generic/500-shaped error) so internal/serve can fall back to
// the stored view instead of failing the request.
func TestEvaluateSubject_SyntheticSubjectIsNotScorable(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	cfg := loadShippedConfig(t)
	now := time.Date(2031, time.January, 1, 0, 0, 0, 0, time.UTC)

	appendEvent(t, ctx, s, "mon-a", "subject.created", now, event.Links{}, map[string]any{"channel": "signup"})
	appendEvent(t, ctx, s, "mon-a", "subject.class", now.Add(time.Second), event.Links{}, map[string]any{"class": "synthetic"})

	w, err := New(Deps{Store: s, Config: cfg, Neighbors: feature.NoNeighbors, Brands: loadShippedBrands(t), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	evaluatedNow, err := w.EvaluateSubject(ctx, testTenant, "mon-a", 3*time.Second)
	if !errors.Is(err, store.ErrNotScorable) {
		t.Fatalf("expected store.ErrNotScorable, got %v", err)
	}
	if evaluatedNow {
		t.Fatalf("expected evaluatedNow=false")
	}
}

// TestEvaluateSubject_AlreadyClaimedSurfacesBusyWithRealRetryAfter proves a
// subject the worker's own Tick has already claimed cannot also be claimed
// by a concurrent evaluate call (design: evaluate shares "the same
// lease... rules" as the worker's own scoring pass), and that the
// returned *store.ErrBusy carries a REAL retry time (S1 fix round), not a
// fixed guess.
func TestEvaluateSubject_AlreadyClaimedSurfacesBusyWithRealRetryAfter(t *testing.T) {
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
	evaluatedNow, err := w.EvaluateSubject(ctx, testTenant, "acct_eval_claimed", 3*time.Second)
	var busy *store.ErrBusy
	if !errors.As(err, &busy) {
		t.Fatalf("expected *store.ErrBusy, got %v", err)
	}
	if evaluatedNow {
		t.Fatalf("expected evaluatedNow=false")
	}
	if wait := busy.RetryAt.Sub(now); wait < 90*time.Second {
		t.Fatalf("RetryAt implies only %v, want close to the ~2min worker claim lease", wait)
	}
}

// TestEvaluateSubject_SyncOnlyCarriesForwardVendorVerdict is B4: a non-local
// advise rule's LAST scored (high) result must still count toward the
// round's tier under evaluate, never be overwritten with "unscored".
func TestEvaluateSubject_SyncOnlyCarriesForwardVendorVerdict(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2031, time.January, 1, 0, 0, 0, 0, time.UTC)

	appendEvent(t, ctx, s, "acct_eval_carry", "subject.created", now, event.Links{}, map[string]any{"channel": "signup"})

	// A fake non-local advise rule pinned to a HIGH risk (benign
	// probability near 0) so the fixture's own local rule (which stays low
	// for a bare signup with nothing else) can never explain a `high` tier
	// on its own — only the carried-forward vendor rule can.
	vendor := fake.New()
	vendor.ScoreFunc = func(ctx context.Context, req model.ScoreRequest) (model.ScoreResult, error) {
		return model.ScoreResult{Probs: map[string]float64{"benign": 0.02, "abusive": 0.98}, Model: "vendor", Checkpoint: "v1"}, nil
	}
	cfg := newFakeRuleConfig(t, vendor)

	w, err := New(Deps{Store: s, Config: cfg, Neighbors: feature.NoNeighbors, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// First, a normal Tick scores the vendor rule for real (not syncOnly),
	// landing its high risk as the LATEST verdict.
	result, err := w.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if result.Scored != 1 || len(result.Errors) != 0 {
		t.Fatalf("Tick result = %+v, want exactly one subject scored with no errors", result)
	}
	before, err := s.SubjectView(ctx, testTenant, "acct_eval_carry", nil)
	if err != nil {
		t.Fatalf("SubjectView (before): %v", err)
	}
	if before.Tier != "high" {
		t.Fatalf("setup assumption broken: expected Tick to land tier=high via the vendor rule, got %q", before.Tier)
	}

	// A later, unrelated event makes the subject dirty again WITHOUT
	// changing anything the vendor rule would score differently.
	appendEvent(t, ctx, s, "acct_eval_carry", "resource.created", now.Add(time.Minute), event.Links{}, map[string]any{"kind": "agent", "name": "a"})

	evaluatedNow, err := w.EvaluateSubject(ctx, testTenant, "acct_eval_carry", 3*time.Second)
	if err != nil {
		t.Fatalf("EvaluateSubject: %v", err)
	}
	if !evaluatedNow {
		t.Fatalf("expected evaluatedNow=true")
	}

	after, err := s.SubjectView(ctx, testTenant, "acct_eval_carry", nil)
	if err != nil {
		t.Fatalf("SubjectView (after): %v", err)
	}
	if after.Tier != "high" {
		t.Fatalf("expected evaluate to KEEP tier=high by carrying the vendor rule's last verdict forward, got %q\nsignals: %+v", after.Tier, after.Signals)
	}
	for _, sig := range after.Signals {
		if sig.Rule == "fake_rule" {
			if sig.Status != "scored" || sig.ErrorCode == "sync_scorer_unsupported" {
				t.Fatalf("expected the vendor rule's signal to still read as scored (carried forward), got %+v", sig)
			}
		}
	}
}

// TestEvaluateSubject_SyncOnlyReportsUnsupportedWithNoPriorResult proves
// the fallback still applies when there's genuinely nothing to carry
// forward: a non-local rule NEVER scored before is unscored/
// sync_scorer_unsupported under evaluate, exactly as before B4.
func TestEvaluateSubject_SyncOnlyReportsUnsupportedWithNoPriorResult(t *testing.T) {
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

	evaluatedNow, err := w.EvaluateSubject(ctx, testTenant, "acct_eval_nonlocal", 3*time.Second)
	if err != nil {
		t.Fatalf("EvaluateSubject: %v", err)
	}
	if !evaluatedNow {
		t.Fatalf("expected evaluatedNow=true")
	}
	view, err := s.SubjectView(ctx, testTenant, "acct_eval_nonlocal", nil)
	if err != nil {
		t.Fatalf("SubjectView: %v", err)
	}
	if len(view.Signals) != 1 {
		t.Fatalf("expected exactly one signal, got %+v", view.Signals)
	}
	sig := view.Signals[0]
	if sig.Status != "unscored" || sig.ErrorCode != "sync_scorer_unsupported" {
		t.Fatalf("expected unscored/sync_scorer_unsupported with no prior result, got %+v", sig)
	}

	// The SAME rule DOES score normally via the ordinary Tick path (not
	// syncOnly) — proving the restriction is evaluate-specific.
	appendEvent(t, ctx, s, "acct_eval_nonlocal", "resource.created", now.Add(time.Minute), event.Links{}, map[string]any{"kind": "agent", "name": "a"})
	result, err := w.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if result.Scored != 1 || len(result.Errors) != 0 {
		t.Fatalf("Tick result = %+v, want exactly one subject scored with no errors", result)
	}
	after, err := s.SubjectView(ctx, testTenant, "acct_eval_nonlocal", nil)
	if err != nil {
		t.Fatalf("SubjectView: %v", err)
	}
	if len(after.Signals) != 1 || after.Signals[0].Status != "scored" {
		t.Fatalf("expected the fake rule to score normally via Tick, got %+v", after.Signals)
	}
}

// delayingStore wraps a real Store and sleeps for delay before every
// EventsForSubject call — a deterministic way to prove the ELAPSED WALL
// TIME during computeVerdict's DB-read phase can still exhaust
// EvaluateSubject's own scoreCtx (created before that phase runs) even
// though the DB reads themselves are no longer bounded by it (S1 fix
// round: "the caller deadline applies to scorer calls only, not DB
// reads") — without depending on real Postgres latency (normally far too
// fast, and never reliably slow, to exercise this deterministically).
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
// round's own latency surfaces context.DeadlineExceeded (S1: not a 500-
// shaped generic error) rather than blocking past it or committing a
// verdict, and that a deadline comfortably longer than that latency
// succeeds normally.
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
	evaluatedNow, err := w.EvaluateSubject(ctx, testTenant, "acct_eval_deadline_exceeded", 20*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded for a 20ms deadline against a 200ms-delayed store, got %v", err)
	}
	if evaluatedNow {
		t.Fatalf("expected evaluatedNow=false")
	}

	appendEvent(t, ctx, s, "acct_eval_deadline_ok", "subject.created", now, event.Links{}, map[string]any{"channel": "signup"})
	w2, err := New(Deps{Store: slow, Config: cfg, Neighbors: feature.NoNeighbors, Brands: loadShippedBrands(t), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	evaluatedNow2, err := w2.EvaluateSubject(ctx, testTenant, "acct_eval_deadline_ok", 3*time.Second)
	if err != nil {
		t.Fatalf("expected a 3s deadline against a 200ms-delayed store to succeed, got %v", err)
	}
	if !evaluatedNow2 {
		t.Fatalf("expected evaluatedNow=true")
	}
}

// TestEvaluateSubject_ReleasesClaimOnCancelledContext is B3's proven
// trigger: a cancelled/timed-out caller context must not leave the subject
// leased for the full ~2-minute claim lease — the NEXT evaluate call
// (with a fresh, uncancelled context) must succeed immediately rather than
// hitting *store.ErrBusy.
func TestEvaluateSubject_ReleasesClaimOnCancelledContext(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2031, time.January, 1, 0, 0, 0, 0, time.UTC)
	cfg := loadShippedConfig(t)

	appendEvent(t, context.Background(), s, "acct_eval_cancelled", "subject.created", now, event.Links{}, map[string]any{"channel": "signup"})

	slow := delayingStore{Store: s, delay: 50 * time.Millisecond}
	w, err := New(Deps{Store: slow, Config: cfg, Neighbors: feature.NoNeighbors, Brands: loadShippedBrands(t), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// An 800µs context: expires long before the 50ms-delayed EventsForSubject
	// call returns, so computeVerdict's DB read itself fails with
	// context.DeadlineExceeded (ctx, not scoreCtx, is what EventsForSubject
	// uses — this cancellation is real, not just scoreCtx's own budget).
	cancelledCtx, cancel := context.WithTimeout(context.Background(), 800*time.Microsecond)
	defer cancel()
	if _, err := w.EvaluateSubject(cancelledCtx, testTenant, "acct_eval_cancelled", 3*time.Second); err == nil {
		t.Fatalf("expected an error from an 800µs context against a 50ms-delayed store")
	}

	// The claim must already be released — a fresh call succeeds
	// immediately rather than hitting *store.ErrBusy.
	evaluatedNow, err := w.EvaluateSubject(context.Background(), testTenant, "acct_eval_cancelled", 3*time.Second)
	var busy *store.ErrBusy
	if errors.As(err, &busy) {
		t.Fatalf("expected the claim to have been released after the cancelled call, got *store.ErrBusy: %+v", busy)
	}
	if err != nil {
		t.Fatalf("expected the next evaluate to succeed, got %v", err)
	}
	if !evaluatedNow {
		t.Fatalf("expected evaluatedNow=true")
	}
}
