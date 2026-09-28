package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tokencanopy/abusekit/internal/config"
	"github.com/tokencanopy/abusekit/internal/event"
	"github.com/tokencanopy/abusekit/internal/feature"
	"github.com/tokencanopy/abusekit/internal/model"
	"github.com/tokencanopy/abusekit/internal/model/fake"
	"github.com/tokencanopy/abusekit/internal/store"
)

var replayBase = time.Date(2031, time.June, 1, 0, 0, 0, 0, time.UTC)

func TestNew_RequiresStoreAndConfig(t *testing.T) {
	if _, err := New(Deps{}); err == nil {
		t.Fatalf("expected an error with no Store or Config")
	}
	if _, err := New(Deps{Config: &config.Config{}}); err == nil {
		t.Fatalf("expected an error with no Store")
	}
	if _, err := New(Deps{Store: (*store.Store)(nil)}); err == nil {
		t.Fatalf("expected an error with no Config")
	}
}

func TestNew_AppliesDefaults(t *testing.T) {
	w, err := New(Deps{Store: (*store.Store)(nil), Config: &config.Config{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if w.deps.Neighbors == nil {
		t.Errorf("expected Neighbors to default to feature.NoNeighbors, got nil")
	}
	if w.deps.BatchSize != DefaultBatchSize {
		t.Errorf("BatchSize = %d, want default %d", w.deps.BatchSize, DefaultBatchSize)
	}
	if w.deps.Interval != DefaultInterval {
		t.Errorf("Interval = %v, want default %v", w.deps.Interval, DefaultInterval)
	}
	if w.deps.Calibration == nil {
		t.Errorf("expected Calibration to default to an empty (non-nil) CalibrationSet")
	}
}

func TestTick_ScoresSubjectWithLocalRule(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	cfg := loadShippedConfig(t)

	subject := "acct_worker_basic"
	appendEvent(t, ctx, s, subject, "subject.created", replayBase, event.Links{}, map[string]any{
		"channel": "signup", "email_domain_class": "corporate", "identity_kind": "individual",
	})

	now := replayBase.Add(time.Minute)
	w, err := New(Deps{Store: s, Config: cfg, Neighbors: feature.NoNeighbors, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	result, err := w.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if result.Claimed != 1 || result.Scored != 1 || result.Stale != 0 || len(result.Errors) != 0 {
		t.Fatalf("Tick result = %+v, want {Claimed:1 Scored:1}", result)
	}

	view, err := s.SubjectView(ctx, testTenant, subject, nil)
	if err != nil {
		t.Fatalf("SubjectView: %v", err)
	}
	if view.Tier == "unknown" {
		t.Errorf("subject view tier = %q, want a real tier (the rule should have scored)", view.Tier)
	}
	if view.Stale {
		t.Errorf("subject view should not be stale immediately after a Tick scored it")
	}
	if len(view.Signals) != 1 || view.Signals[0].Status != "scored" {
		t.Errorf("expected exactly one scored signal, got %+v", view.Signals)
	}

	// A second Tick with nothing new to score should claim nothing.
	result2, err := w.Tick(ctx)
	if err != nil {
		t.Fatalf("second Tick: %v", err)
	}
	if result2.Claimed != 0 {
		t.Errorf("second Tick claimed %d subjects, want 0 (nothing dirty)", result2.Claimed)
	}
}

func TestTick_ClassSkip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	cfg := loadShippedConfig(t)

	subject := "acct_worker_internal"
	appendEvent(t, ctx, s, subject, "subject.created", replayBase, event.Links{}, nil)
	appendEvent(t, ctx, s, subject, "subject.class", replayBase.Add(time.Second), event.Links{}, map[string]any{"class": "internal"})

	now := replayBase.Add(time.Minute)
	w, err := New(Deps{Store: s, Config: cfg, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	result, err := w.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if result.Claimed != 0 {
		t.Errorf("Tick claimed %d subjects, want 0 (class=internal must never be queued)", result.Claimed)
	}

	view, err := s.SubjectView(ctx, testTenant, subject, nil)
	if err != nil {
		t.Fatalf("SubjectView: %v", err)
	}
	if view.Tier != "unknown" {
		t.Errorf("internal subject's tier = %q, want unknown (it must never be scored)", view.Tier)
	}
}

func TestScoreSubject_HandlesStaleRound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	cfg := loadShippedConfig(t)

	subject := "acct_worker_stale"
	appendEvent(t, ctx, s, subject, "subject.created", replayBase, event.Links{}, nil)

	now := replayBase.Add(time.Minute)
	w, err := New(Deps{Store: s, Config: cfg, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// A normal round at dirty_seq=1 commits and advances scored_seq to 1.
	d := store.DirtySubject{Tenant: testTenant, Subject: subject, DirtySeq: 1, ScoredSeq: 0, CurrentTier: "unknown"}
	scored, err := w.scoreSubject(ctx, d, now)
	if err != nil {
		t.Fatalf("first scoreSubject: %v", err)
	}
	if !scored {
		t.Fatalf("first scoreSubject: got stale, want committed")
	}

	// A second round claiming to start from dirty_seq=0 — as if it had read
	// the subject before the first round advanced scored_seq to 1 — must
	// lose the compare-and-clear race: store.ErrStaleRound, reported as
	// (false, nil), never a Tick-visible error.
	stale := store.DirtySubject{Tenant: testTenant, Subject: subject, DirtySeq: 0, ScoredSeq: 0, CurrentTier: "unknown"}
	scored2, err := w.scoreSubject(ctx, stale, now.Add(time.Second))
	if err != nil {
		t.Fatalf("stale scoreSubject returned an error, want (false, nil): %v", err)
	}
	if scored2 {
		t.Fatalf("stale scoreSubject reported committed, want stale")
	}
}

func TestScoreSubject_RuleBackoffAndRecovery(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	subject := "acct_worker_backoff"
	appendEvent(t, ctx, s, subject, "subject.created", replayBase, event.Links{}, nil)

	calls := 0
	failing := true
	scorer := fake.New()
	scorer.ScoreFunc = func(_ context.Context, req model.ScoreRequest) (model.ScoreResult, error) {
		calls++
		if failing {
			return model.ScoreResult{}, errors.New("synthetic adapter failure")
		}
		return model.ScoreResult{
			Probs: map[string]float64{"benign": 0.9, "abusive": 0.1}, Model: "fake_test_scorer", Checkpoint: "v1",
		}, nil
	}
	cfg := newFakeRuleConfig(t, scorer)

	now := replayBase.Add(time.Minute)
	w, err := New(Deps{Store: s, Config: cfg, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d := store.DirtySubject{Tenant: testTenant, Subject: subject, DirtySeq: 1, CurrentTier: "unknown"}

	// Round 1: the scorer errors -> attempts=1, retry_at = now + 30s.
	if _, err := w.scoreSubject(ctx, d, now); err != nil {
		t.Fatalf("round 1: %v", err)
	}
	if calls != 1 {
		t.Fatalf("round 1: scorer called %d times, want 1", calls)
	}
	backoff, err := s.GetRuleBackoff(ctx, testTenant, subject, "fake_rule")
	if err != nil {
		t.Fatalf("GetRuleBackoff: %v", err)
	}
	if backoff.Attempts != 1 || backoff.RetryAt == nil {
		t.Fatalf("backoff after round 1 = %+v, want Attempts=1 with a RetryAt set", backoff)
	}
	wantRetryAt := now.Add(30 * time.Second)
	if !backoff.RetryAt.Equal(wantRetryAt) {
		t.Errorf("retry_at = %v, want %v (30s schedule)", *backoff.RetryAt, wantRetryAt)
	}

	// Round 2, still within the backoff window: the scorer must NOT be
	// called again.
	now2 := now.Add(10 * time.Second)
	if _, err := w.scoreSubject(ctx, d, now2); err != nil {
		t.Fatalf("round 2: %v", err)
	}
	if calls != 1 {
		t.Errorf("round 2 (still in backoff): scorer called %d times, want 1 (no new call)", calls)
	}

	// Round 3, past the 30s backoff: the scorer is called again, errors
	// again -> attempts=2, retry_at = now3 + 2m (the schedule's second
	// entry).
	now3 := now.Add(31 * time.Second)
	if _, err := w.scoreSubject(ctx, d, now3); err != nil {
		t.Fatalf("round 3: %v", err)
	}
	if calls != 2 {
		t.Fatalf("round 3: scorer called %d times, want 2", calls)
	}
	backoff2, err := s.GetRuleBackoff(ctx, testTenant, subject, "fake_rule")
	if err != nil {
		t.Fatalf("GetRuleBackoff: %v", err)
	}
	if backoff2.Attempts != 2 {
		t.Fatalf("attempts after round 3 = %d, want 2", backoff2.Attempts)
	}
	wantRetryAt2 := now3.Add(2 * time.Minute)
	if backoff2.RetryAt == nil || !backoff2.RetryAt.Equal(wantRetryAt2) {
		t.Errorf("retry_at after round 3 = %v, want %v (2m schedule)", backoff2.RetryAt, wantRetryAt2)
	}

	// Round 4, past the 2m backoff, scorer now recovers: attempts/retry_at
	// are cleared entirely.
	failing = false
	now4 := now3.Add(2*time.Minute + time.Second)
	if _, err := w.scoreSubject(ctx, d, now4); err != nil {
		t.Fatalf("round 4: %v", err)
	}
	if calls != 3 {
		t.Fatalf("round 4: scorer called %d times, want 3", calls)
	}
	backoff3, err := s.GetRuleBackoff(ctx, testTenant, subject, "fake_rule")
	if err != nil {
		t.Fatalf("GetRuleBackoff: %v", err)
	}
	if backoff3 != (store.RuleBackoff{}) {
		t.Errorf("backoff after recovery = %+v, want the zero value (cleared)", backoff3)
	}
}

func TestScoreSubject_BudgetDenialSetsCostCap(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	subject := "acct_worker_budget"
	appendEvent(t, ctx, s, subject, "subject.created", replayBase, event.Links{}, nil)

	calls := 0
	scorer := fake.New()
	scorer.ScoreFunc = func(context.Context, model.ScoreRequest) (model.ScoreResult, error) {
		calls++
		return model.ScoreResult{Probs: map[string]float64{"benign": 1, "abusive": 0}, Model: "fake_test_scorer", Checkpoint: "v1"}, nil
	}
	cfg := newFakeRuleConfig(t, scorer)

	now := replayBase.Add(time.Minute)
	budgets := NewBudgets(1, 1, 1)
	if err := budgets.Record(context.Background(), "fake_test_scorer", testTenant, subject, now); err != nil {
		t.Fatalf("Record: %v", err)
	}

	w, err := New(Deps{Store: s, Config: cfg, Budgets: budgets, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d := store.DirtySubject{Tenant: testTenant, Subject: subject, DirtySeq: 1, CurrentTier: "unknown"}

	if _, err := w.scoreSubject(ctx, d, now); err != nil {
		t.Fatalf("scoreSubject: %v", err)
	}
	if calls != 0 {
		t.Errorf("scorer called %d times, want 0 (budget should have denied it)", calls)
	}

	view, err := s.SubjectView(ctx, testTenant, subject, nil)
	if err != nil {
		t.Fatalf("SubjectView: %v", err)
	}
	if len(view.Signals) != 1 || view.Signals[0].Status != "unscored" || view.Signals[0].ErrorCode != "cost_cap" {
		t.Fatalf("signals = %+v, want one unscored signal with error_code cost_cap", view.Signals)
	}
}

func TestScoreSubject_LocalScorerBypassesBudget(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	cfg := loadShippedConfig(t)

	subject := "acct_worker_local_no_budget"
	appendEvent(t, ctx, s, subject, "subject.created", replayBase, event.Links{}, nil)

	now := replayBase.Add(time.Minute)
	budgets := NewBudgets(1, 1, 1)
	if err := budgets.Record(context.Background(), "local", testTenant, subject, now); err != nil {
		t.Fatalf("Record: %v", err)
	}
	w, err := New(Deps{Store: s, Config: cfg, Budgets: budgets, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	result, err := w.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if result.Scored != 1 {
		t.Fatalf("Tick result = %+v, want Scored=1 (local must bypass budgets entirely)", result)
	}
	view, err := s.SubjectView(ctx, testTenant, subject, nil)
	if err != nil {
		t.Fatalf("SubjectView: %v", err)
	}
	if len(view.Signals) != 1 || view.Signals[0].Status != "scored" {
		t.Fatalf("signals = %+v, want the local rule scored despite an exhausted budget", view.Signals)
	}
}

func TestStartStop(t *testing.T) {
	s := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := loadShippedConfig(t)

	w, err := New(Deps{Store: s, Config: cfg, Interval: 5 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	done := make(chan struct{})
	go func() {
		w.Start(ctx)
		close(done)
	}()

	// Give Start a few ticks to run against an empty queue (nothing to
	// score, but it must not panic or block), then stop it.
	time.Sleep(50 * time.Millisecond)
	w.Stop()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("Start did not return after Stop")
	}

	// Stop is safe to call again.
	w.Stop()
}
