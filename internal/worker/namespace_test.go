package worker

import (
	"context"
	"github.com/tokencanopy/abusekit/internal/event"
	"github.com/tokencanopy/abusekit/internal/model/fake"
	"github.com/tokencanopy/abusekit/internal/store"
	"strings"
	"testing"
	"time"
)

func TestReasonVersionSurvivesUnchangedReuse(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	cfg := loadShippedConfig(t)
	now := replayBase.Add(time.Minute)
	const subject = "acct_reason_version"
	appendEvent(t, ctx, s, subject, "subject.created", replayBase, event.Links{}, nil)
	w, err := New(Deps{Store: s, Config: cfg, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	d := store.DirtySubject{Tenant: testTenant, Subject: subject, DirtySeq: 1, CurrentTier: "unknown"}
	if _, err = w.scoreSubject(ctx, d, now); err != nil {
		t.Fatal(err)
	}
	latest, err := s.LatestVerdicts(ctx, testTenant, subject)
	if err != nil {
		t.Fatal(err)
	}
	lv := latest["new_account_velocity"]
	if lv.ReasonVersion != 2 || !strings.Contains(lv.Reason, "core.resource_velocity_1h=") {
		t.Fatalf("new reason lost version: %+v", lv)
	}
	for _, version := range []int{2, 1} {
		reason := "historical template remains unchanged"
		_, err = s.UpsertVerdicts(ctx, testTenant, subject, 1, []store.VerdictRecord{{Rule: "new_account_velocity", Mode: lv.Mode, Scorer: lv.Scorer, Model: lv.Model, Checkpoint: lv.Checkpoint, Calibration: lv.Calibration, Probs: lv.Probs, Risk: lv.Risk, Flagged: lv.Flagged, Reason: reason, ReasonVersion: version, InputHash: lv.InputHash, Status: "scored"}}, store.SubjectSummary{Tier: "low"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = w.scoreSubject(ctx, d, now); err != nil {
			t.Fatal(err)
		}
		view, err := s.SubjectView(ctx, testTenant, subject, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(view.Signals) != 1 || view.Signals[0].Reason != reason || view.Signals[0].ReasonVersion != version {
			t.Fatalf("reuse rewrote reason/version %d: %+v", version, view.Signals)
		}
	}
}

func TestReasonVersionSurvivesSynchronousVendorReuse(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := replayBase.Add(time.Minute)
	const subject = "acct_vendor_reason"
	appendEvent(t, ctx, s, subject, "subject.created", replayBase, event.Links{}, nil)
	cfg := newFakeRuleConfig(t, fake.New())
	risk := 0.2
	_, err := s.UpsertVerdicts(ctx, testTenant, subject, 1, []store.VerdictRecord{{Rule: "fake_rule", Mode: "advise", Scorer: "fake_test_scorer", Model: "fake_test_scorer", Checkpoint: "v1", Probs: map[string]float64{"benign": 0.8, "abusive": 0.2}, Risk: &risk, Reason: "legacy reason", ReasonVersion: 1, InputHash: "old-input", Status: "scored"}}, store.SubjectSummary{Tier: "low"})
	if err != nil {
		t.Fatal(err)
	}
	w, err := New(Deps{Store: s, Config: cfg, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	_, records, _, _, err := w.computeVerdict(ctx, ctx, store.DirtySubject{Tenant: testTenant, Subject: subject, DirtySeq: 1}, now, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Reason != "legacy reason" || records[0].ReasonVersion != 1 || records[0].InputHash != "old-input" {
		t.Fatalf("synchronous reuse rewrote legacy evidence: %+v", records)
	}
}
