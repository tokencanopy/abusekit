package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tokencanopy/abusekit/internal/event"
	"github.com/tokencanopy/abusekit/internal/model"
	"github.com/tokencanopy/abusekit/internal/store"
)

// TestScoreSubject_RescheduleOnRuleBackoff is B4 fix round, proven: an
// errored rule on an otherwise-quiet subject was never retried (still
// "unknown" at +48h) because next_rescore_at only ever reflected feature-
// window decay, never a pending rule retry.
func TestScoreSubject_RescheduleOnRuleBackoff(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	appendEvent(t, ctx, s, "acct_reschedule_backoff", "subject.created", replayBase, event.Links{}, nil)

	scorer := scorerFunc{name: "fake_test_scorer", fn: func(context.Context, model.ScoreRequest) (model.ScoreResult, error) {
		return model.ScoreResult{}, errors.New("synthetic failure")
	}}
	cfg := newFakeRuleConfigWithScorer(t, scorer)

	now := replayBase.Add(time.Minute)
	w, err := New(Deps{Store: s, Config: cfg, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d := store.DirtySubject{Tenant: testTenant, Subject: "acct_reschedule_backoff", DirtySeq: 1, CurrentTier: "unknown"}
	if _, err := w.scoreSubject(ctx, d, now); err != nil {
		t.Fatalf("scoreSubject: %v", err)
	}

	// The rule just entered backoff with retry_at = now + 30s (first
	// failure). next_rescore_at must be scheduled at (or before) that
	// instant — feature-window decay alone (this subject is brand new,
	// no window is anywhere near expiring) would otherwise leave it with
	// nothing scheduled until some unrelated future event.
	wantRetryAt := now.Add(30 * time.Second)

	notYet, err := s.ClaimDirtySubjects(ctx, wantRetryAt.Add(-time.Second), 0)
	if err != nil {
		t.Fatalf("claim before retry_at: %v", err)
	}
	for _, c := range notYet {
		if c.Subject == "acct_reschedule_backoff" {
			t.Fatalf("claimed before its rule's retry_at was due")
		}
	}

	due, err := s.ClaimDirtySubjects(ctx, wantRetryAt, 0)
	if err != nil {
		t.Fatalf("claim at retry_at: %v", err)
	}
	found := false
	for _, c := range due {
		if c.Subject == "acct_reschedule_backoff" {
			found = true
		}
	}
	if !found {
		t.Fatalf("acct_reschedule_backoff was not rescheduled for its rule's retry_at (B4)")
	}
}

// TestScoreSubject_RescheduleOnCostCap is B4 fix round, proven: a
// cost-capped rule was never retried after midnight because next_rescore_at
// didn't account for the budget's own daily reset.
func TestScoreSubject_RescheduleOnCostCap(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	appendEvent(t, ctx, s, "acct_reschedule_costcap", "subject.created", replayBase, event.Links{}, nil)

	calls := 0
	scorer := scorerFunc{name: "fake_test_scorer", fn: func(context.Context, model.ScoreRequest) (model.ScoreResult, error) {
		calls++
		return model.ScoreResult{Probs: map[string]float64{"benign": 1, "abusive": 0}}, nil
	}}
	cfg := newFakeRuleConfigWithScorer(t, scorer)

	now := replayBase.Add(time.Minute)
	budgets := NewBudgets(1, 1, 1)
	budgets.Record("fake_test_scorer", testTenant, "acct_reschedule_costcap", now) // pre-exhaust the cap

	w, err := New(Deps{Store: s, Config: cfg, Budgets: budgets, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d := store.DirtySubject{Tenant: testTenant, Subject: "acct_reschedule_costcap", DirtySeq: 1, CurrentTier: "unknown"}
	if _, err := w.scoreSubject(ctx, d, now); err != nil {
		t.Fatalf("scoreSubject: %v", err)
	}
	if calls != 0 {
		t.Fatalf("scorer called %d times, want 0 (budget should have denied it)", calls)
	}

	wantReset := nextUTCMidnight(now)

	notYet, err := s.ClaimDirtySubjects(ctx, wantReset.Add(-time.Minute), 0)
	if err != nil {
		t.Fatalf("claim before reset: %v", err)
	}
	for _, c := range notYet {
		if c.Subject == "acct_reschedule_costcap" {
			t.Fatalf("claimed before the budget's daily reset")
		}
	}

	due, err := s.ClaimDirtySubjects(ctx, wantReset, 0)
	if err != nil {
		t.Fatalf("claim at reset: %v", err)
	}
	found := false
	for _, c := range due {
		if c.Subject == "acct_reschedule_costcap" {
			found = true
		}
	}
	if !found {
		t.Fatalf("acct_reschedule_costcap was not rescheduled for the budget's daily reset (B4)")
	}
}
