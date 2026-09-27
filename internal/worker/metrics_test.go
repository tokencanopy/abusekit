package worker

import (
	"context"
	"errors"
	"expvar"
	"testing"
	"time"

	"github.com/tokencanopy/abusekit/internal/event"
	"github.com/tokencanopy/abusekit/internal/model"
	"github.com/tokencanopy/abusekit/internal/store"
)

func TestMetrics_Publish(t *testing.T) {
	m := NewMetrics()
	m.SetQueueDepth(3)
	// A unique name per test run avoids colliding with any other test in
	// this package that also calls Publish — expvar's registry is a single
	// process-wide map and panics on a duplicate name.
	m.Publish("abusekit_test_metrics_publish")

	v := expvar.Get("abusekit_test_metrics_publish")
	if v == nil {
		t.Fatalf("expvar.Get did not find the published metrics")
	}
	if v.String() == "" {
		t.Errorf("published metrics stringified to empty")
	}
}

func TestMetrics_Counters(t *testing.T) {
	m := NewMetrics()
	m.SetQueueDepth(5)
	m.SetOldestDirtyAge(90 * time.Second)
	m.IncSubjectsFailed()
	m.IncSubjectsFailed()
	m.IncVerdictsByTier("high")
	m.IncVerdictsByTier("high")
	m.IncVerdictsByTier("low")
	m.IncAdapterCalls("local")
	m.IncAdapterCalls("local")
	m.IncAdapterErrors("local")
	m.ObserveAdapterLatency("local", 10*time.Millisecond)
	m.ObserveAdapterLatency("local", 30*time.Millisecond)
	m.IncBudgetDenials("jev")

	snap := m.Snapshot()
	if snap.QueueDepth != 5 {
		t.Errorf("QueueDepth = %d, want 5", snap.QueueDepth)
	}
	if snap.OldestDirtyAge != 90*time.Second {
		t.Errorf("OldestDirtyAge = %v, want 90s", snap.OldestDirtyAge)
	}
	if snap.SubjectsFailed != 2 {
		t.Errorf("SubjectsFailed = %d, want 2", snap.SubjectsFailed)
	}
	if snap.VerdictsByTier["high"] != 2 || snap.VerdictsByTier["low"] != 1 {
		t.Errorf("VerdictsByTier = %v, want {high:2, low:1}", snap.VerdictsByTier)
	}
	if snap.AdapterCalls["local"] != 2 {
		t.Errorf("AdapterCalls[local] = %d, want 2", snap.AdapterCalls["local"])
	}
	if snap.AdapterErrors["local"] != 1 {
		t.Errorf("AdapterErrors[local] = %d, want 1", snap.AdapterErrors["local"])
	}
	if snap.AdapterMeanLatency["local"] != 20*time.Millisecond {
		t.Errorf("AdapterMeanLatency[local] = %v, want 20ms", snap.AdapterMeanLatency["local"])
	}
	if snap.BudgetDenials["jev"] != 1 {
		t.Errorf("BudgetDenials[jev] = %d, want 1", snap.BudgetDenials["jev"])
	}
}

func TestMetrics_SnapshotIsIndependentCopy(t *testing.T) {
	m := NewMetrics()
	m.IncVerdictsByTier("high")
	snap := m.Snapshot()
	m.IncVerdictsByTier("high")
	if snap.VerdictsByTier["high"] != 1 {
		t.Errorf("snapshot mutated after being taken: %v", snap.VerdictsByTier)
	}
}

func TestWorker_RecordsMetricsDuringATick(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	cfg := loadShippedConfig(t)

	appendEvent(t, ctx, s, "acct_metrics", "subject.created", replayBase, event.Links{}, nil)

	now := replayBase.Add(time.Minute)
	metrics := NewMetrics()
	w, err := New(Deps{Store: s, Config: cfg, Metrics: metrics, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := w.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	snap := metrics.Snapshot()
	if snap.AdapterCalls["local"] != 1 {
		t.Errorf("AdapterCalls[local] = %d, want 1", snap.AdapterCalls["local"])
	}
	if total := snap.VerdictsByTier["low"] + snap.VerdictsByTier["medium"] + snap.VerdictsByTier["high"] + snap.VerdictsByTier["unknown"]; total != 1 {
		t.Errorf("expected exactly one verdict tier recorded, got %v", snap.VerdictsByTier)
	}
}

func TestWorker_RecordsBudgetDenialMetric(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	appendEvent(t, ctx, s, "acct_metrics_budget", "subject.created", replayBase, event.Links{}, nil)

	scorer := scorerFunc{name: "fake_test_scorer", fn: func(context.Context, model.ScoreRequest) (model.ScoreResult, error) {
		return model.ScoreResult{}, errors.New("should never be called")
	}}
	cfg := newFakeRuleConfigWithScorer(t, scorer)

	now := replayBase.Add(time.Minute)
	budgets := NewBudgets(1, 1, 1)
	budgets.Record("fake_test_scorer", testTenant, "acct_metrics_budget", now)
	metrics := NewMetrics()

	w, err := New(Deps{Store: s, Config: cfg, Budgets: budgets, Metrics: metrics, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d := store.DirtySubject{Tenant: testTenant, Subject: "acct_metrics_budget", DirtySeq: 1, CurrentTier: "unknown"}
	if _, err := w.scoreSubject(ctx, d, now); err != nil {
		t.Fatalf("scoreSubject: %v", err)
	}

	snap := metrics.Snapshot()
	if snap.BudgetDenials["fake_test_scorer"] != 1 {
		t.Errorf("BudgetDenials[fake_test_scorer] = %d, want 1", snap.BudgetDenials["fake_test_scorer"])
	}
}
