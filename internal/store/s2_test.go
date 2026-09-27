package store_test

// Tests for the S2 (features/worker) additions to internal/store:
// ClaimDirtySubjects, NeighborsByKinds, NeighborOutcomes, the rule_state
// backoff methods, LatestVerdicts, and UpsertVerdicts' new NextRescoreAt
// field. Kept in their own file rather than appended to store_test.go so
// the S1/S2 boundary in this package's test suite stays easy to see.

import (
	"context"
	"testing"
	"time"

	"github.com/tokencanopy/abusekit/internal/event"
	"github.com/tokencanopy/abusekit/internal/store"
)

func TestClaimDirtySubjects_ExcludesInternalAndSynthetic(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-09-27T12:00:00Z")

	appendAt(t, ctx, s, "acct_customer", "subject.created", now, nil)
	appendAt(t, ctx, s, "acct_internal", "subject.created", now, nil)
	appendAt(t, ctx, s, "acct_internal", "subject.class", now.Add(time.Second), map[string]any{"class": "internal"})
	appendAt(t, ctx, s, "acct_synthetic", "subject.created", now, nil)
	appendAt(t, ctx, s, "acct_synthetic", "subject.class", now.Add(time.Second), map[string]any{"class": "synthetic"})

	dirty, err := s.ClaimDirtySubjects(ctx, now.Add(time.Minute), 0)
	if err != nil {
		t.Fatalf("ClaimDirtySubjects: %v", err)
	}
	var subjects []string
	for _, d := range dirty {
		subjects = append(subjects, d.Subject)
	}
	if len(subjects) != 1 || subjects[0] != "acct_customer" {
		t.Fatalf("claimed subjects = %v, want exactly [acct_customer]", subjects)
	}
}

func TestClaimDirtySubjects_RespectsLimit(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-09-27T12:00:00Z")

	for i := 0; i < 5; i++ {
		appendAt(t, ctx, s, subjectName(i), "subject.created", now, nil)
	}

	dirty, err := s.ClaimDirtySubjects(ctx, now.Add(time.Minute), 2)
	if err != nil {
		t.Fatalf("ClaimDirtySubjects: %v", err)
	}
	if len(dirty) != 2 {
		t.Fatalf("claimed %d subjects, want exactly 2 (the limit)", len(dirty))
	}
}

func TestClaimDirtySubjects_NothingDirtyReturnsEmpty(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	dirty, err := s.ClaimDirtySubjects(ctx, mustTime(t, "2031-09-27T12:00:00Z"), 0)
	if err != nil {
		t.Fatalf("ClaimDirtySubjects: %v", err)
	}
	if len(dirty) != 0 {
		t.Fatalf("claimed %d subjects from an empty database, want 0", len(dirty))
	}
}

func TestClaimDirtySubjects_NextRescoreAtWithoutANewEvent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-09-27T12:00:00Z")

	e := mkEvent(t, "evt_1", "acct_rescore", "subject.created", now, nil)
	if _, err := s.AppendEvents(ctx, testTenant, "e2a-server", []event.Event{e}); err != nil {
		t.Fatalf("AppendEvents: %v", err)
	}

	// Score it now, scheduling a rescore 1 hour out — no new event will
	// arrive, this is purely a decayed-window schedule (design §4.8).
	rescoreAt := now.Add(time.Hour)
	if _, err := s.UpsertVerdicts(ctx, testTenant, "acct_rescore", 1, []store.VerdictRecord{{
		Rule: "r", Mode: "advise", Scorer: "local", Model: "local", Status: "scored", InputHash: "h",
	}}, store.SubjectSummary{Tier: "low", Score: 0, NextRescoreAt: rescoreAt}); err != nil {
		t.Fatalf("UpsertVerdicts: %v", err)
	}

	// Before the scheduled rescore: not dirty (scored_seq caught up to
	// dirty_seq, and next_rescore_at hasn't arrived).
	before, err := s.ClaimDirtySubjects(ctx, rescoreAt.Add(-time.Minute), 0)
	if err != nil {
		t.Fatalf("ClaimDirtySubjects (before): %v", err)
	}
	for _, d := range before {
		if d.Subject == "acct_rescore" {
			t.Fatalf("acct_rescore claimed before its scheduled rescore time")
		}
	}

	// At/after the scheduled rescore: dirty again, despite no new event.
	after, err := s.ClaimDirtySubjects(ctx, rescoreAt, 0)
	if err != nil {
		t.Fatalf("ClaimDirtySubjects (after): %v", err)
	}
	found := false
	for _, d := range after {
		if d.Subject == "acct_rescore" {
			found = true
		}
	}
	if !found {
		t.Fatalf("acct_rescore not claimed at its scheduled rescore time")
	}
}

func TestNeighborsByKinds_RestrictsToGivenKinds(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-09-27T12:00:00Z")

	emailHash := "1111111111111111111111111111111111111111111111111111111111111111" // 64 hex chars
	asn := "AS64500"

	e1 := event.Event{ID: "e1", Subject: "acct_a", Type: "subject.created", At: now, Links: event.Links{EmailHash: emailHash, ASN: asn}}
	e2 := event.Event{ID: "e2", Subject: "acct_b", Type: "subject.created", At: now, Links: event.Links{EmailHash: emailHash}}
	e3 := event.Event{ID: "e3", Subject: "acct_c", Type: "subject.created", At: now, Links: event.Links{ASN: asn}}
	for _, e := range []*event.Event{&e1, &e2, &e3} {
		if err := e.Validate(event.ValidateOptions{Now: e.At}); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if err := e.Redact(); err != nil {
			t.Fatalf("Redact: %v", err)
		}
	}
	if _, err := s.AppendEvents(ctx, testTenant, "e2a-server", []event.Event{e1, e2, e3}); err != nil {
		t.Fatalf("AppendEvents: %v", err)
	}

	emailOnly, _, err := s.NeighborsByKinds(ctx, testTenant, "acct_a", []string{"email_hash"}, 0, 0)
	if err != nil {
		t.Fatalf("NeighborsByKinds: %v", err)
	}
	if len(emailOnly) != 1 || emailOnly[0] != "acct_b" {
		t.Fatalf("NeighborsByKinds(email_hash) = %v, want [acct_b]", emailOnly)
	}

	asnOnly, _, err := s.NeighborsByKinds(ctx, testTenant, "acct_a", []string{"asn"}, 0, 0)
	if err != nil {
		t.Fatalf("NeighborsByKinds: %v", err)
	}
	if len(asnOnly) != 1 || asnOnly[0] != "acct_c" {
		t.Fatalf("NeighborsByKinds(asn) = %v, want [acct_c]", asnOnly)
	}
}

func TestNeighborsByKinds_RejectsEmptyKinds(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, _, err := s.NeighborsByKinds(ctx, testTenant, "acct_a", nil, 0, 0); err == nil {
		t.Fatalf("expected an error for an empty kinds list")
	}
}

func TestNeighborOutcomes(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-09-27T12:00:00Z")

	appendAt(t, ctx, s, "acct_deleted", "subject.created", now, nil)
	appendAt(t, ctx, s, "acct_deleted", "subject.deleted", now.Add(time.Second), map[string]any{"mode": "trash"})
	appendAt(t, ctx, s, "acct_labelled", "subject.created", now, nil)
	appendAt(t, ctx, s, "acct_clean", "subject.created", now, nil)

	if _, err := s.PutLabel(ctx, testTenant, store.Label{Subject: "acct_labelled", Label: "abusive", Source: "operator", Actor: "ops@example.test"}); err != nil {
		t.Fatalf("PutLabel: %v", err)
	}

	deleted, labelled, err := s.NeighborOutcomes(ctx, testTenant, []string{"acct_deleted", "acct_labelled", "acct_clean"})
	if err != nil {
		t.Fatalf("NeighborOutcomes: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deletedCount = %d, want 1", deleted)
	}
	if labelled != 1 {
		t.Errorf("labelledAbusiveCount = %d, want 1", labelled)
	}
}

func TestNeighborOutcomes_EmptyInput(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	deleted, labelled, err := s.NeighborOutcomes(ctx, testTenant, nil)
	if err != nil {
		t.Fatalf("NeighborOutcomes: %v", err)
	}
	if deleted != 0 || labelled != 0 {
		t.Fatalf("NeighborOutcomes(nil) = (%d, %d), want (0, 0)", deleted, labelled)
	}
}

func TestRuleBackoff_GetRecordClear(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	zero, err := s.GetRuleBackoff(ctx, testTenant, "acct_backoff", "r1")
	if err != nil {
		t.Fatalf("GetRuleBackoff (before any error): %v", err)
	}
	if zero != (store.RuleBackoff{}) {
		t.Fatalf("GetRuleBackoff (before any error) = %+v, want the zero value", zero)
	}

	retryAt := mustTime(t, "2031-09-27T12:00:30Z")
	if err := s.RecordRuleError(ctx, testTenant, "acct_backoff", "r1", retryAt, "boom"); err != nil {
		t.Fatalf("RecordRuleError: %v", err)
	}
	got, err := s.GetRuleBackoff(ctx, testTenant, "acct_backoff", "r1")
	if err != nil {
		t.Fatalf("GetRuleBackoff: %v", err)
	}
	if got.Attempts != 1 || got.RetryAt == nil || !got.RetryAt.Equal(retryAt) || got.LastError != "boom" {
		t.Fatalf("GetRuleBackoff = %+v, want Attempts=1 RetryAt=%v LastError=boom", got, retryAt)
	}

	retryAt2 := mustTime(t, "2031-09-27T12:02:30Z")
	if err := s.RecordRuleError(ctx, testTenant, "acct_backoff", "r1", retryAt2, "boom again"); err != nil {
		t.Fatalf("RecordRuleError (2nd): %v", err)
	}
	got2, err := s.GetRuleBackoff(ctx, testTenant, "acct_backoff", "r1")
	if err != nil {
		t.Fatalf("GetRuleBackoff (2nd): %v", err)
	}
	if got2.Attempts != 2 {
		t.Fatalf("Attempts after a 2nd error = %d, want 2", got2.Attempts)
	}

	if err := s.ClearRuleBackoff(ctx, testTenant, "acct_backoff", "r1"); err != nil {
		t.Fatalf("ClearRuleBackoff: %v", err)
	}
	cleared, err := s.GetRuleBackoff(ctx, testTenant, "acct_backoff", "r1")
	if err != nil {
		t.Fatalf("GetRuleBackoff (after clear): %v", err)
	}
	if cleared != (store.RuleBackoff{}) {
		t.Fatalf("GetRuleBackoff (after clear) = %+v, want the zero value", cleared)
	}

	// Clearing a row that doesn't exist is a no-op, not an error.
	if err := s.ClearRuleBackoff(ctx, testTenant, "acct_never_errored", "r1"); err != nil {
		t.Fatalf("ClearRuleBackoff (no row): %v", err)
	}
}

func TestLatestVerdicts(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-09-27T12:00:00Z")

	e := mkEvent(t, "evt_1", "acct_latest", "subject.created", now, nil)
	if _, err := s.AppendEvents(ctx, testTenant, "e2a-server", []event.Event{e}); err != nil {
		t.Fatalf("AppendEvents: %v", err)
	}

	risk := 0.6
	if _, err := s.UpsertVerdicts(ctx, testTenant, "acct_latest", 1, []store.VerdictRecord{
		{Rule: "scored_rule", Mode: "advise", Scorer: "local", Model: "local", Status: "scored", Risk: &risk, InputHash: "hash_a"},
		{Rule: "unscored_rule", Mode: "shadow", Scorer: "jev", Status: "unscored", ErrorCode: "cost_cap", InputHash: "hash_b"},
	}, store.SubjectSummary{Tier: "medium", Score: risk}); err != nil {
		t.Fatalf("UpsertVerdicts: %v", err)
	}

	latest, err := s.LatestVerdicts(ctx, testTenant, "acct_latest")
	if err != nil {
		t.Fatalf("LatestVerdicts: %v", err)
	}
	scored, ok := latest["scored_rule"]
	if !ok || scored.InputHash != "hash_a" || scored.Risk == nil || *scored.Risk != risk {
		t.Fatalf("latest[scored_rule] = %+v, want InputHash=hash_a Risk=%v", scored, risk)
	}
	unscored, ok := latest["unscored_rule"]
	if !ok || unscored.InputHash != "hash_b" || unscored.Risk != nil {
		t.Fatalf("latest[unscored_rule] = %+v, want InputHash=hash_b Risk=nil", unscored)
	}
	if _, ok := latest["never_scored_rule"]; ok {
		t.Fatalf("latest[never_scored_rule] should be absent, not present")
	}
}

func appendAt(t *testing.T, ctx context.Context, s *store.Store, subject, typ string, at time.Time, data map[string]any) {
	t.Helper()
	e := mkEvent(t, subject+"|"+typ+"|"+at.Format(time.RFC3339Nano), subject, typ, at, data)
	if _, err := s.AppendEvents(ctx, testTenant, "e2a-server", []event.Event{e}); err != nil {
		t.Fatalf("AppendEvents: %v", err)
	}
}

func subjectName(i int) string {
	return "acct_" + string(rune('a'+i))
}
