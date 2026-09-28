package worker

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/tokencanopy/abusekit/internal/event"
	"github.com/tokencanopy/abusekit/internal/model"
	"github.com/tokencanopy/abusekit/internal/model/fake"
	"github.com/tokencanopy/abusekit/internal/store"
)

// TestScoreSubject_LatencyMeasuredPerCallNotPerSubject is R8 round 2.
// Proven: scoreSubject's per-call loop took `start := now` OUTSIDE the
// loop (the subject's own entry-time parameter), reused for EVERY rule's
// latency measurement — a subject scored by two rules had its SECOND
// rule's recorded latency inflated by however long the FIRST rule's call
// took, since `w.deps.now().Sub(start)` measured from the subject's
// start, not the individual call's. start must be taken fresh,
// immediately before each scorer.Score call.
func TestScoreSubject_LatencyMeasuredPerCallNotPerSubject(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	const subject = "acct_example_r2_latency"
	appendEvent(t, ctx, s, subject, "subject.created", replayBase, event.Links{}, map[string]any{})

	scorerA := fake.New()
	scorerA.ScoreFunc = func(context.Context, model.ScoreRequest) (model.ScoreResult, error) {
		time.Sleep(150 * time.Millisecond)
		return model.ScoreResult{Probs: map[string]float64{"benign": 1, "abusive": 0}, Model: "fake", Checkpoint: "v1"}, nil
	}
	scorerB := fake.New()
	scorerB.ScoreFunc = func(context.Context, model.ScoreRequest) (model.ScoreResult, error) {
		return model.ScoreResult{Probs: map[string]float64{"benign": 1, "abusive": 0}, Model: "fake", Checkpoint: "v1"}, nil
	}
	cfg := newTwoFakeRuleConfig(t, scorerA, scorerB)

	metrics := NewMetrics()
	realNow := func() time.Time { return time.Now().UTC() }
	w, err := New(Deps{Store: s, Config: cfg, Metrics: metrics, Now: realNow, Logger: discardLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	dirty, err := s.ClaimDirtySubjects(ctx, realNow(), 10)
	if err != nil {
		t.Fatalf("ClaimDirtySubjects: %v", err)
	}
	if len(dirty) != 1 {
		t.Fatalf("ClaimDirtySubjects = %+v, want exactly one subject", dirty)
	}

	if _, err := w.scoreSubject(ctx, dirty[0], realNow()); err != nil {
		t.Fatalf("scoreSubject: %v", err)
	}

	snap := metrics.Snapshot()
	if snap.AdapterMeanLatency["fake_scorer_a"] < 100*time.Millisecond {
		t.Errorf("AdapterMeanLatency[fake_scorer_a] = %v, want >= ~150ms (it actually slept that long)", snap.AdapterMeanLatency["fake_scorer_a"])
	}
	if snap.AdapterMeanLatency["fake_scorer_b"] >= 100*time.Millisecond {
		t.Errorf("AdapterMeanLatency[fake_scorer_b] = %v, want well under 100ms — it answers instantly; a measurement this large means it was inflated by fake_scorer_a's own delay", snap.AdapterMeanLatency["fake_scorer_b"])
	}
}

// TestScoreSubject_BudgetDenialLogsAtWarn is R8 round 2: a budget denial
// silently stops a rule from scoring for a whole tenant/adapter
// combination — operationally significant enough to be visible in logs
// (at warn), with the tenant and adapter identified, not just an
// incrementing Metrics counter.
func TestScoreSubject_BudgetDenialLogsAtWarn(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	const subject = "acct_example_r2_budget_log"
	appendEvent(t, ctx, s, subject, "subject.created", replayBase, event.Links{}, nil)

	scorer := fake.New()
	scorer.ScoreFunc = func(context.Context, model.ScoreRequest) (model.ScoreResult, error) {
		return model.ScoreResult{Probs: map[string]float64{"benign": 1, "abusive": 0}, Model: "fake_test_scorer", Checkpoint: "v1"}, nil
	}
	cfg := newFakeRuleConfig(t, scorer)

	now := replayBase.Add(time.Minute)
	budgets := NewBudgets(1, 1, 1)
	if err := budgets.Record(context.Background(), "fake_test_scorer", testTenant, subject, now); err != nil {
		t.Fatalf("Record: %v", err)
	}

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	w, err := New(Deps{Store: s, Config: cfg, Budgets: budgets, Now: func() time.Time { return now }, Logger: logger})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d := store.DirtySubject{Tenant: testTenant, Subject: subject, DirtySeq: 1, CurrentTier: "unknown"}

	if _, err := w.scoreSubject(ctx, d, now); err != nil {
		t.Fatalf("scoreSubject: %v", err)
	}

	logged := logBuf.String()
	if !strings.Contains(logged, "level=WARN") {
		t.Errorf("expected a WARN-level log line, got: %s", logged)
	}
	if !strings.Contains(logged, "tenant="+testTenant) {
		t.Errorf("expected the log line to identify tenant=%s, got: %s", testTenant, logged)
	}
	if !strings.Contains(logged, "adapter=fake_test_scorer") {
		t.Errorf("expected the log line to identify adapter=fake_test_scorer, got: %s", logged)
	}
}
