package worker

import (
	"context"
	"testing"
	"time"

	"github.com/tokencanopy/abusekit/internal/event"
	"github.com/tokencanopy/abusekit/internal/model"
	"github.com/tokencanopy/abusekit/internal/store"
)

// TestScoreSubject_SkipInputUnchangedAvoidsRecallingScorer is S6 fix
// round, proven wasteful otherwise: 60 sends in quick succession produced
// 59 redundant full recomputations of a rule whose input hadn't actually
// changed between rounds.
func TestScoreSubject_SkipInputUnchangedAvoidsRecallingScorer(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	appendEvent(t, ctx, s, "acct_skip_unchanged", "subject.created", replayBase, event.Links{}, nil)

	calls := 0
	scorer := scorerFunc{name: "fake_test_scorer", fn: func(context.Context, model.ScoreRequest) (model.ScoreResult, error) {
		calls++
		return model.ScoreResult{Probs: map[string]float64{"benign": 0.8, "abusive": 0.2}, Model: "fake_test_scorer", Checkpoint: "v1"}, nil
	}}
	cfg := newFakeRuleConfigWithScorer(t, scorer)

	now := replayBase.Add(time.Minute)
	w, err := New(Deps{Store: s, Config: cfg, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Round 1: dirty_seq=1, input is whatever subject_age_h computes to at
	// `now` — the scorer is called once.
	d := store.DirtySubject{Tenant: testTenant, Subject: "acct_skip_unchanged", DirtySeq: 1, CurrentTier: "unknown"}
	if _, err := w.scoreSubject(ctx, d, now); err != nil {
		t.Fatalf("round 1: %v", err)
	}
	if calls != 1 {
		t.Fatalf("round 1: scorer called %d times, want 1", calls)
	}

	// Round 2: SAME now (so subject_age_h, the rule's only input, is
	// byte-identical) and the SAME dirty_seq (simulating a rescore-at tick
	// with no new event) — Plan should mark this input_unchanged, so the
	// scorer must NOT be called again.
	if _, err := w.scoreSubject(ctx, d, now); err != nil {
		t.Fatalf("round 2: %v", err)
	}
	if calls != 1 {
		t.Errorf("round 2 (unchanged input): scorer called %d times, want still 1 (S6: input_unchanged should skip the vendor call)", calls)
	}

	// The reused round must still produce the SAME risk (from the SAME
	// Probs, deterministically re-derived through Combine) as round 1.
	view, err := s.SubjectView(ctx, testTenant, "acct_skip_unchanged", nil)
	if err != nil {
		t.Fatalf("SubjectView: %v", err)
	}
	if len(view.Signals) != 1 || view.Signals[0].Status != "scored" {
		t.Fatalf("signals = %+v, want one scored signal", view.Signals)
	}
	wantRisk := 0.2
	if diff := view.Signals[0].Risk - wantRisk; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("reused risk = %v, want %v (identical to round 1's)", view.Signals[0].Risk, wantRisk)
	}

	// Round 3: a genuinely different `now` (input changes: subject_age_h
	// grows) must call the scorer again.
	now2 := now.Add(time.Hour)
	d2 := store.DirtySubject{Tenant: testTenant, Subject: "acct_skip_unchanged", DirtySeq: 1, CurrentTier: "unknown"}
	if _, err := w.scoreSubject(ctx, d2, now2); err != nil {
		t.Fatalf("round 3: %v", err)
	}
	if calls != 2 {
		t.Errorf("round 3 (changed input): scorer called %d times, want 2", calls)
	}
}

func TestScoreSubject_PrunesRemovedRuleState(t *testing.T) {
	// S11 fix round: a rule renamed or removed from config leaves behind an
	// orphaned rule_state row unless pruned.
	s := newTestStore(t)
	ctx := context.Background()
	cfg := loadShippedConfig(t)

	appendEvent(t, ctx, s, "acct_prune_worker", "subject.created", replayBase, event.Links{}, nil)
	if err := s.RecordRuleError(ctx, testTenant, "acct_prune_worker", "old_removed_rule", replayBase, "boom"); err != nil {
		t.Fatalf("RecordRuleError: %v", err)
	}

	now := replayBase.Add(time.Minute)
	w, err := New(Deps{Store: s, Config: cfg, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := w.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	backoff, err := s.GetRuleBackoff(ctx, testTenant, "acct_prune_worker", "old_removed_rule")
	if err != nil {
		t.Fatalf("GetRuleBackoff: %v", err)
	}
	if backoff != (store.RuleBackoff{}) {
		t.Errorf("old_removed_rule's rule_state should have been pruned by the Tick, got %+v", backoff)
	}
}
