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
	// on its own — only the carried-forward vendor rule can. vendorCalls
	// (R3, round 2 fix round) counts genuine invocations, distinguishing a
	// real re-score from a carried-forward reuse of the last one.
	vendor := fake.New()
	var vendorCalls int
	vendor.ScoreFunc = func(ctx context.Context, req model.ScoreRequest) (model.ScoreResult, error) {
		vendorCalls++
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
	if vendorCalls != 1 {
		t.Fatalf("expected exactly 1 vendor call after the first Tick, got %d", vendorCalls)
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
	if vendorCalls != 1 {
		t.Fatalf("expected the vendor call count to stay at 1 through the carried-forward evaluate (no live call), got %d", vendorCalls)
	}
	// R3 (round 2 fix round): the carried-forward round must NOT have
	// advanced scored_seq past the vendor rule's un-refreshed state — the
	// subject stays stale/claimable for its own real round.
	if !after.Stale {
		t.Fatalf("expected the subject to remain stale (dirty_seq > scored_seq) after a syncOnly carry-forward round")
	}

	// fake_rule's only declared input is core.subject_age_h, which never changes
	// on its own while now is frozen — advancing the clock before the
	// second Tick is what makes core.Plan see genuinely different inputs
	// for the vendor rule (rather than replaying its own SkipInputUnchanged
	// optimization, an unrelated mechanism this test isn't about) so the
	// scorer is actually invoked again rather than reused a second time.
	now = now.Add(2 * time.Hour)

	// R3's own repro: the worker's own Tick must still claim the subject
	// and genuinely re-invoke the vendor scorer — 2 calls total across both
	// Ticks. Before R3, evaluate's carry-forward wrongly advanced
	// scored_seq up to dirty_seq, making ClaimDirtySubjects' dirty check
	// see the subject as already caught up and never re-visit the vendor
	// rule at all.
	result2, err := w.Tick(ctx)
	if err != nil {
		t.Fatalf("second Tick: %v", err)
	}
	if result2.Scored != 1 || len(result2.Errors) != 0 {
		t.Fatalf("second Tick result = %+v, want exactly one subject scored with no errors", result2)
	}
	if vendorCalls != 2 {
		t.Fatalf("expected the vendor to be called again by the second Tick (2 calls total), got %d", vendorCalls)
	}
}

// TestEvaluateSubject_SyncOnlyOmitsRuleWithNoPriorResult is R3 (round 2 fix
// round): when there's genuinely nothing to carry forward, the rule is
// OMITTED from the round entirely — no verdict row at all, not the
// previous unscored/sync_scorer_unsupported placeholder. Recording that
// placeholder made it look like evaluate had actually considered the rule
// and found it wanting for this exact dirty_seq, which then permanently
// starved it: because every configured rule here is non-local (no local
// rule to keep the round non-empty), UpsertVerdicts's own empty-records
// no-op means this evaluate call reports evaluatedNow=false — genuinely
// nothing was evaluated — and the claim is released normally.
func TestEvaluateSubject_SyncOnlyOmitsRuleWithNoPriorResult(t *testing.T) {
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
	if evaluatedNow {
		t.Fatalf("expected evaluatedNow=false: every configured rule was omitted, nothing committed")
	}
	view, err := s.SubjectView(ctx, testTenant, "acct_eval_nonlocal", nil)
	if err != nil {
		t.Fatalf("SubjectView: %v", err)
	}
	if len(view.Signals) != 0 {
		t.Fatalf("expected no signals at all (the rule was omitted, not recorded unscored), got %+v", view.Signals)
	}
	if !view.Stale {
		t.Fatalf("expected the subject to remain stale (dirty_seq > scored_seq) after an all-omitted round")
	}

	// The claim must have been released, not leaked by the all-omitted
	// round's evaluatedNow=false path: a normal ClaimSubjectForEvaluate
	// right afterward must not see *store.ErrBusy.
	claimed, err := s.ClaimSubjectForEvaluate(ctx, testTenant, "acct_eval_nonlocal", now)
	if err != nil {
		var busy *store.ErrBusy
		if errors.As(err, &busy) {
			t.Fatalf("expected the claim to have been released after an all-omitted round, got *store.ErrBusy: %+v", busy)
		}
		t.Fatalf("ClaimSubjectForEvaluate: %v", err)
	}
	if err := s.ReleaseClaim(ctx, testTenant, "acct_eval_nonlocal", claimed.ClaimedUntil); err != nil {
		t.Fatalf("ReleaseClaim: %v", err)
	}

	// The SAME rule DOES score normally via the ordinary Tick path (not
	// syncOnly) — proving the restriction is evaluate-specific, and that
	// the subject staying dirty (never advanced to d.DirtySeq) is exactly
	// what lets Tick pick it up.
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

// TestEvaluateSubject_ShortDeadlineDoesNotBackoffRule is R1 (round 2 fix
// round)'s proven trigger: the per-rule scorer-error handling used to call
// RecordRuleError (writing a real rule_state backoff row — a DB write that
// survives independently of computeVerdict's own return value) for ANY
// scorer-call error, including one caused by scoreCtx's own artificial
// deadline (evaluate's deadline_ms) expiring — even though EvaluateSubject
// separately abandons the WHOLE round in that exact case (never calling
// UpsertVerdicts). The backoff row outlived the abandoned round, so a
// single evaluate call with a deliberately tiny deadline_ms could push
// new_account_velocity — the only rule fast.jsonl's local scorer runs —
// into ~30s of backoff, silently degrading BOTH the next normal-deadline
// evaluate call AND the worker's own Tick to tier "unknown" for a window
// far larger than the offending call's own scope.
//
// Repro, as given by the reviewer: fast fixture evaluates high -> new
// event -> evaluate {"deadline_ms":1} -> a normal evaluate (and,
// separately, a worker Tick) must NOT come back unknown/degraded/backoff.
func TestEvaluateSubject_ShortDeadlineDoesNotBackoffRule(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	cfg := loadShippedConfig(t)
	const subject = "acct_short_deadline"
	const ruleName = "new_account_velocity"

	all := loadFixture(t, "../../eval/fixtures/fast.jsonl")
	var setup []event.Event
	for _, e := range all {
		if e.Type == "content.sent" && !isSelfSend(e) {
			break
		}
		setup = append(setup, e)
	}
	if len(setup) == 0 {
		t.Fatalf("fast.jsonl fixture shape assumption broken: no setup events")
	}
	for i := range setup {
		setup[i].Subject = subject
	}
	ingestFixture(t, ctx, s, setup)

	now := lastEventAt(setup).Add(time.Second)
	// A store that delays EventsForSubject well past scoreCtx's 1ms budget
	// — deterministically reproducing "scoreCtx has already expired by the
	// time the scorer loop runs" without depending on real scorer latency
	// (the local scorer is a pure in-process computation, normally far too
	// fast to ever observe an expired 1ms deadline on its own).
	slow := delayingStore{Store: s, delay: 50 * time.Millisecond}
	w, err := New(Deps{Store: slow, Config: cfg, Neighbors: feature.NoNeighbors, Brands: loadShippedBrands(t), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Baseline: reaches high exactly like TestEvaluateSubject_
	// FastFixtureReachesHighBeforeFirstSend, proving this fixture/config
	// pairing behaves as that test already established.
	evaluatedNow, err := w.EvaluateSubject(ctx, testTenant, subject, 3*time.Second)
	if err != nil {
		t.Fatalf("baseline EvaluateSubject: %v", err)
	}
	if !evaluatedNow {
		t.Fatalf("baseline: expected evaluatedNow=true")
	}
	baseline, err := s.SubjectView(ctx, testTenant, subject, nil)
	if err != nil {
		t.Fatalf("baseline SubjectView: %v", err)
	}
	if baseline.Tier != "high" || baseline.Degraded {
		t.Fatalf("baseline assumption broken: tier=%q degraded=%v, want high/false", baseline.Tier, baseline.Degraded)
	}

	// A new event makes the subject dirty again.
	appendEvent(t, ctx, s, subject, "resource.created", now.Add(time.Minute), event.Links{}, map[string]any{"kind": "agent", "name": "a-short-deadline"})

	// The poisoning attempt: deadline_ms:1, guaranteed to have expired by
	// the time the scorer loop runs (the 50ms-delayed EventsForSubject
	// call alone outlasts it 50x over).
	if _, err := w.EvaluateSubject(ctx, testTenant, subject, time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded from a 1ms deadline against a 50ms-delayed store, got %v", err)
	}

	// The core of the fix: no backoff row was left behind by the abandoned
	// round.
	backoff, err := s.GetRuleBackoff(ctx, testTenant, subject, ruleName)
	if err != nil {
		t.Fatalf("GetRuleBackoff: %v", err)
	}
	if backoff.InBackoff(now) {
		t.Fatalf("expected no backoff after a deadline-exceeded round, got %+v (still in backoff at %v)", backoff, now)
	}

	// A normal evaluate right afterward (same fixed `now` — the reviewer's
	// own point is that even a real 2s gap isn't enough to clear a ~30s
	// backoff, so holding `now` fixed is the harder, not easier, version of
	// that check) must succeed and land back at high, not unknown.
	evaluatedNow, err = w.EvaluateSubject(ctx, testTenant, subject, 3*time.Second)
	if err != nil {
		t.Fatalf("recovery EvaluateSubject: %v", err)
	}
	if !evaluatedNow {
		t.Fatalf("recovery: expected evaluatedNow=true")
	}
	after, err := s.SubjectView(ctx, testTenant, subject, nil)
	if err != nil {
		t.Fatalf("recovery SubjectView: %v", err)
	}
	if after.Tier == "unknown" || after.Degraded {
		t.Fatalf("recovery: tier=%q degraded=%v, want neither unknown nor degraded", after.Tier, after.Degraded)
	}
	found := false
	for _, sig := range after.Signals {
		if sig.Rule != ruleName {
			continue
		}
		found = true
		if sig.Status != "scored" || sig.ErrorCode != "" {
			t.Fatalf("recovery: expected %s scored with no error, got %+v", ruleName, sig)
		}
	}
	if !found {
		t.Fatalf("expected a %s signal, got %+v", ruleName, after.Signals)
	}

	// The worker's own Tick must not be blocked either: one more event,
	// then a Tick must claim and score the subject normally.
	appendEvent(t, ctx, s, subject, "resource.created", now.Add(2*time.Minute), event.Links{}, map[string]any{"kind": "agent", "name": "b-short-deadline"})
	result, err := w.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if result.Scored != 1 || len(result.Errors) != 0 {
		t.Fatalf("Tick result = %+v, want exactly one subject scored with no errors", result)
	}
	final, err := s.SubjectView(ctx, testTenant, subject, nil)
	if err != nil {
		t.Fatalf("final SubjectView: %v", err)
	}
	if final.Tier == "unknown" || final.Degraded {
		t.Fatalf("after Tick: tier=%q degraded=%v, want neither unknown nor degraded", final.Tier, final.Degraded)
	}
}

// ambiguousClaimStore wraps a real Store, letting the REAL
// ClaimSubjectForEvaluate call land normally, then swaps its result for a
// synthetic *store.ErrClaimAmbiguous — deterministically reproducing "the
// claim's commit actually succeeded, but the caller got back an error as
// if the outcome were ambiguous" without depending on hitting the real,
// narrow context-cancellation timing window against Postgres that
// TestEvaluateSubject_ShortDeadlineDuringClaimNeverLeaksLease below
// exercises directly (the reviewer's own literal repro, necessarily
// probabilistic against real DB timing).
type ambiguousClaimStore struct {
	Store
}

func (a ambiguousClaimStore) ClaimSubjectForEvaluate(ctx context.Context, tenant, subject string, now time.Time) (store.DirtySubject, error) {
	d, err := a.Store.ClaimSubjectForEvaluate(ctx, tenant, subject, now)
	if err != nil {
		return store.DirtySubject{}, err
	}
	// T3 (round 3): ClaimedUntil must be the REAL claim's own value — a
	// real ClaimSubjectForEvaluate populates ErrClaimAmbiguous.ClaimedUntil
	// from exactly what it just committed (evaluate.go), and
	// ReleaseClaim's compare-and-clear depends on that value matching what
	// is actually in the row. Leaving it as the zero value here would make
	// EvaluateSubject's best-effort release a guaranteed no-op — it would
	// never actually clear the real lease this wrapper just took.
	return store.DirtySubject{}, &store.ErrClaimAmbiguous{Err: errors.New("synthetic: simulated commit-outcome ambiguity"), ClaimedUntil: d.ClaimedUntil}
}

// TestEvaluateSubject_ReleasesAmbiguousClaimCommit is R2 (round 2 fix
// round)'s proven trigger, deterministic form: ClaimSubjectForEvaluate's
// own tx.Commit can fail with the caller's context expiring at the exact
// instant the COMMIT itself raced that expiry — an outcome the caller
// cannot distinguish from "never committed" (store.ErrClaimAmbiguous
// exists exactly for this). Before this fix, EvaluateSubject's early
// return on any ClaimSubjectForEvaluate error skipped the release-on-not-
// committed defer entirely (it isn't registered yet at that point), so a
// claim that actually landed leaked for the full ~2min lease with nothing
// to release it.
func TestEvaluateSubject_ReleasesAmbiguousClaimCommit(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2031, time.January, 1, 0, 0, 0, 0, time.UTC)
	cfg := loadShippedConfig(t)
	const subject = "acct_eval_ambiguous_commit"

	appendEvent(t, ctx, s, subject, "subject.created", now, event.Links{}, map[string]any{"channel": "signup"})

	w, err := New(Deps{Store: ambiguousClaimStore{Store: s}, Config: cfg, Neighbors: feature.NoNeighbors, Brands: loadShippedBrands(t), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = w.EvaluateSubject(ctx, testTenant, subject, 3*time.Second)
	var ambiguous *store.ErrClaimAmbiguous
	if !errors.As(err, &ambiguous) {
		t.Fatalf("expected *store.ErrClaimAmbiguous from the simulated ambiguous claim commit, got %v", err)
	}

	// The real underlying claim DID land (ambiguousClaimStore let the real
	// call through before swapping its result) — without R2's fix this
	// leaves the subject claimed for the full lease. A plain follow-up
	// call, through an UNWRAPPED store, must not see *store.ErrBusy.
	w2, err := New(Deps{Store: s, Config: cfg, Neighbors: feature.NoNeighbors, Brands: loadShippedBrands(t), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	evaluatedNow, err := w2.EvaluateSubject(ctx, testTenant, subject, 3*time.Second)
	var busy *store.ErrBusy
	if errors.As(err, &busy) {
		t.Fatalf("expected the ambiguous claim to have been released, got *store.ErrBusy: %+v", busy)
	}
	if err != nil {
		t.Fatalf("expected the follow-up evaluate to succeed, got %v", err)
	}
	if !evaluatedNow {
		t.Fatalf("expected evaluatedNow=true")
	}
}

// TestEvaluateSubject_ShortDeadlineDuringClaimNeverLeaksLease is R2's own
// literal repro against real Postgres timing: a context whose deadline can
// fire at ANY point during ClaimSubjectForEvaluate's own transaction —
// including exactly during its tx.Commit, the one window
// store.ErrClaimAmbiguous exists for — must never leave the subject
// claimed. Looping many short-deadline attempts (rather than one) is
// deliberate: on a fast local Postgres, any single attempt is far more
// likely to fail before starting, or to succeed comfortably within
// budget, than to land exactly inside the commit's own round trip — this
// is a best-effort, probabilistic way to exercise that narrow window at
// least once. The invariant asserted afterward (no leaked lease; the
// worker's own Tick can still claim and score) must hold regardless of
// how many attempts, if any, actually hit it.
func TestEvaluateSubject_ShortDeadlineDuringClaimNeverLeaksLease(t *testing.T) {
	s := newTestStore(t)
	cfg := loadShippedConfig(t)
	now := time.Date(2031, time.January, 1, 0, 0, 0, 0, time.UTC)
	const subject = "acct_eval_claim_race"

	appendEvent(t, context.Background(), s, subject, "subject.created", now, event.Links{}, map[string]any{"channel": "signup"})

	w, err := New(Deps{Store: s, Config: cfg, Neighbors: feature.NoNeighbors, Brands: loadShippedBrands(t), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for i := 0; i < 50; i++ {
		shortCtx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		_, _ = w.EvaluateSubject(shortCtx, testTenant, subject, 3*time.Second)
		cancel()
	}

	// The claim must not be leaked: a direct claim attempt with a normal
	// context must not see *store.ErrBusy.
	claimed, err := s.ClaimSubjectForEvaluate(context.Background(), testTenant, subject, now)
	if err != nil {
		var busy *store.ErrBusy
		if errors.As(err, &busy) {
			t.Fatalf("expected no leaked claim after 50 short-deadline attempts, got *store.ErrBusy: %+v", busy)
		}
		t.Fatalf("ClaimSubjectForEvaluate: %v", err)
	}
	if err := s.ReleaseClaim(context.Background(), testTenant, subject, claimed.ClaimedUntil); err != nil {
		t.Fatalf("ReleaseClaim: %v", err)
	}

	// The worker's own Tick must be able to claim it too: one more event,
	// then Tick must score it with no errors.
	appendEvent(t, context.Background(), s, subject, "resource.created", now.Add(time.Minute), event.Links{}, map[string]any{"kind": "agent", "name": "race"})
	result, err := w.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if result.Scored != 1 || len(result.Errors) != 0 {
		t.Fatalf("Tick result = %+v, want exactly one subject scored with no errors", result)
	}
}
