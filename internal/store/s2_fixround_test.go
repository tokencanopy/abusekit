package store_test

// Tests for the abusekit PR #3 fix round's store-layer changes: B3
// (per-subject claim lease + failure backoff), S2 (propagation to
// neighbours), S4 (index evidence), S5 (claim priority ordering), and S11
// (LatestVerdicts not hiding a prior scored risk, rule_state pruning).

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tokencanopy/abusekit/internal/event"
	"github.com/tokencanopy/abusekit/internal/store"
)

func TestClaimDirtySubjects_LeaseExcludesAlreadyClaimedSubject(t *testing.T) {
	// B3, proven: a bare "select then commit before scoring" claim let two
	// instances issue 40 scorer calls for 20 subjects. A held lease must
	// stop a second claim call from reselecting the same subject.
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-09-27T12:00:00Z")

	appendAt(t, ctx, s, "acct_leased", "subject.created", now, nil)

	first, err := s.ClaimDirtySubjects(ctx, now.Add(time.Minute), 0)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if len(first) != 1 || first[0].Subject != "acct_leased" {
		t.Fatalf("first claim = %v, want exactly [acct_leased]", first)
	}

	// A second claim moments later, before the lease expires, must not
	// reselect it.
	second, err := s.ClaimDirtySubjects(ctx, now.Add(2*time.Minute), 0)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	for _, d := range second {
		if d.Subject == "acct_leased" {
			t.Fatalf("acct_leased was claimed twice while its lease was still held")
		}
	}

	// After the lease expires (DefaultClaimLease is 2 minutes) and with no
	// scoring round having released it, it becomes claimable again.
	third, err := s.ClaimDirtySubjects(ctx, now.Add(time.Minute).Add(store.DefaultClaimLease).Add(time.Second), 0)
	if err != nil {
		t.Fatalf("third claim: %v", err)
	}
	found := false
	for _, d := range third {
		if d.Subject == "acct_leased" {
			found = true
		}
	}
	if !found {
		t.Fatalf("acct_leased was not reclaimable after its lease expired")
	}
}

func TestExtendClaims_PushesLeaseForwardPastTheOriginalClaim(t *testing.T) {
	// R4 round 2: a batch whose per-subject processing time adds up to
	// more than the lease a single tick-start claim set (a slow scorer,
	// several subjects deep into a batch) must be able to extend an
	// already-claimed subject's lease from a freshly-taken "now", or a
	// second instance's ClaimDirtySubjects would see the ORIGINAL,
	// now-too-old claimed_until and reselect it out from under the first.
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-09-27T12:00:00Z")

	appendAt(t, ctx, s, "acct_extend", "subject.created", now, nil)

	claimed, err := s.ClaimDirtySubjects(ctx, now.Add(time.Minute), 0)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 1 || claimed[0].Subject != "acct_extend" {
		t.Fatalf("claim = %v, want exactly [acct_extend]", claimed)
	}

	// Without extending: moments before the ORIGINAL lease would expire,
	// the subject is still correctly excluded (sanity check the fixture's
	// own baseline before testing the extension itself).
	stillExcluded, err := s.ClaimDirtySubjects(ctx, now.Add(time.Minute).Add(store.DefaultClaimLease).Add(-time.Second), 0)
	if err != nil {
		t.Fatalf("claim before extension: %v", err)
	}
	for _, d := range stillExcluded {
		if d.Subject == "acct_extend" {
			t.Fatalf("acct_extend was reclaimable before its original lease even expired")
		}
	}

	// Extend from a LATER "now" (simulating this subject having actually
	// started processing later in the batch than the tick-level claim
	// time).
	extendFrom := now.Add(time.Minute).Add(store.DefaultClaimLease).Add(-time.Second)
	if err := s.ExtendClaims(ctx, []string{testTenant}, []string{"acct_extend"}, extendFrom); err != nil {
		t.Fatalf("ExtendClaims: %v", err)
	}

	// Just past what the ORIGINAL claim's lease would have allowed: still
	// excluded, because the extension pushed claimed_until out further.
	pastOriginalLease := now.Add(time.Minute).Add(store.DefaultClaimLease).Add(time.Second)
	afterOriginal, err := s.ClaimDirtySubjects(ctx, pastOriginalLease, 0)
	if err != nil {
		t.Fatalf("claim just past the original lease: %v", err)
	}
	for _, d := range afterOriginal {
		if d.Subject == "acct_extend" {
			t.Fatalf("acct_extend was reclaimed past its ORIGINAL lease — ExtendClaim did not take effect")
		}
	}

	// Past the EXTENDED lease (extendFrom + DefaultClaimLease): reclaimable
	// again.
	pastExtendedLease := extendFrom.Add(store.DefaultClaimLease).Add(time.Second)
	afterExtended, err := s.ClaimDirtySubjects(ctx, pastExtendedLease, 0)
	if err != nil {
		t.Fatalf("claim past the extended lease: %v", err)
	}
	found := false
	for _, d := range afterExtended {
		if d.Subject == "acct_extend" {
			found = true
		}
	}
	if !found {
		t.Fatalf("acct_extend was not reclaimable after its EXTENDED lease expired")
	}
}

func TestExtendClaims_ExtendsEveryPairInOneCall(t *testing.T) {
	// R4 round 2: internal/worker calls this for the WHOLE remaining tail
	// of a batch each iteration, not one subject at a time — must actually
	// extend every (tenant, subject) pair given, not just the first.
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-09-27T13:00:00Z")

	appendAt(t, ctx, s, "acct_extend_multi_1", "subject.created", now, nil)
	appendAt(t, ctx, s, "acct_extend_multi_2", "subject.created", now, nil)

	claimed, err := s.ClaimDirtySubjects(ctx, now.Add(time.Minute), 0)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	claimedSubjects := map[string]bool{}
	for _, d := range claimed {
		claimedSubjects[d.Subject] = true
	}
	if !claimedSubjects["acct_extend_multi_1"] || !claimedSubjects["acct_extend_multi_2"] {
		t.Fatalf("claim = %v, want both acct_extend_multi_1 and acct_extend_multi_2", claimed)
	}

	extendFrom := now.Add(time.Minute).Add(store.DefaultClaimLease).Add(-time.Second)
	if err := s.ExtendClaims(ctx,
		[]string{testTenant, testTenant},
		[]string{"acct_extend_multi_1", "acct_extend_multi_2"},
		extendFrom,
	); err != nil {
		t.Fatalf("ExtendClaims: %v", err)
	}

	pastOriginalLease := now.Add(time.Minute).Add(store.DefaultClaimLease).Add(time.Second)
	afterOriginal, err := s.ClaimDirtySubjects(ctx, pastOriginalLease, 0)
	if err != nil {
		t.Fatalf("claim just past the original lease: %v", err)
	}
	for _, d := range afterOriginal {
		if d.Subject == "acct_extend_multi_1" || d.Subject == "acct_extend_multi_2" {
			t.Fatalf("%s was reclaimed past its ORIGINAL lease — ExtendClaims did not extend every pair", d.Subject)
		}
	}
}

func TestExtendClaims_EmptyIsANoop(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.ExtendClaims(ctx, nil, nil, mustTime(t, "2031-09-27T13:00:00Z")); err != nil {
		t.Fatalf("ExtendClaims with no pairs: %v", err)
	}
}

func TestClaimDirtySubjects_ReleasedByUpsertVerdicts(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-09-27T12:00:00Z")

	e := mkEvent(t, "evt_1", "acct_release", "subject.created", now, nil)
	if _, err := s.AppendEvents(ctx, testTenant, "e2a-server", []event.Event{e}); err != nil {
		t.Fatalf("AppendEvents: %v", err)
	}
	claimed, err := s.ClaimDirtySubjects(ctx, now.Add(time.Minute), 0)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("expected exactly 1 claimed, got %d", len(claimed))
	}
	d := claimed[0]

	if _, err := s.UpsertVerdicts(ctx, testTenant, d.Subject, d.DirtySeq, []store.VerdictRecord{
		{Rule: "r", Mode: "advise", Scorer: "local", Model: "local", Status: "scored", InputHash: "h"},
	}, store.SubjectSummary{Tier: "low", Score: 0}); err != nil {
		t.Fatalf("UpsertVerdicts: %v", err)
	}

	// The lease must be released immediately by the successful commit, not
	// held for its full duration.
	reclaimed, err := s.ClaimDirtySubjects(ctx, now.Add(time.Minute+time.Second), 0)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	for _, rd := range reclaimed {
		if rd.Subject == "acct_release" {
			t.Fatalf("acct_release should not be dirty again (scored_seq caught up), got reclaimed: %+v", rd)
		}
	}
}

func TestRecordSubjectFailure_BacksOffAndClearsOnSuccess(t *testing.T) {
	// B3, proven: a subject whose scoring pass always errors was reclaimed
	// and retried every tick forever, starving the rest of the batch.
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-09-27T12:00:00Z")

	appendAt(t, ctx, s, "acct_poison", "subject.created", now, nil)

	claimed, err := s.ClaimDirtySubjects(ctx, now.Add(time.Minute), 0)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("expected 1 claimed, got %d", len(claimed))
	}

	retryAt := now.Add(time.Minute).Add(30 * time.Second)
	if err := s.RecordSubjectFailure(ctx, testTenant, "acct_poison", retryAt); err != nil {
		t.Fatalf("RecordSubjectFailure: %v", err)
	}

	// Immediately after: not reclaimable (backoff window), even though the
	// lease itself was released.
	tooSoon, err := s.ClaimDirtySubjects(ctx, now.Add(time.Minute).Add(time.Second), 0)
	if err != nil {
		t.Fatalf("claim (too soon): %v", err)
	}
	for _, d := range tooSoon {
		if d.Subject == "acct_poison" {
			t.Fatalf("acct_poison reclaimed before next_attempt_at")
		}
	}

	// After next_attempt_at: reclaimable, and fail_count is visible.
	after, err := s.ClaimDirtySubjects(ctx, retryAt, 0)
	if err != nil {
		t.Fatalf("claim (after backoff): %v", err)
	}
	var found *store.DirtySubject
	for i := range after {
		if after[i].Subject == "acct_poison" {
			found = &after[i]
		}
	}
	if found == nil {
		t.Fatalf("acct_poison not reclaimed after next_attempt_at")
	}
	if found.FailCount != 1 {
		t.Errorf("FailCount = %d, want 1", found.FailCount)
	}

	// A successful round clears fail_count/next_attempt_at.
	if _, err := s.UpsertVerdicts(ctx, testTenant, "acct_poison", found.DirtySeq, []store.VerdictRecord{
		{Rule: "r", Mode: "advise", Scorer: "local", Model: "local", Status: "scored", InputHash: "h"},
	}, store.SubjectSummary{Tier: "low", Score: 0}); err != nil {
		t.Fatalf("UpsertVerdicts: %v", err)
	}
	// Force it dirty again to inspect fail_count via a fresh claim.
	appendAt(t, ctx, s, "acct_poison", "resource.created", retryAt.Add(time.Second), map[string]any{"kind": "agent"})
	again, err := s.ClaimDirtySubjects(ctx, retryAt.Add(time.Minute), 0)
	if err != nil {
		t.Fatalf("claim (after recovery): %v", err)
	}
	for _, d := range again {
		if d.Subject == "acct_poison" && d.FailCount != 0 {
			t.Errorf("FailCount after a successful round = %d, want 0", d.FailCount)
		}
	}
}

func TestClaimDirtySubjects_NeverScoredRanksAboveAnyScoredSubject(t *testing.T) {
	// S5 fix round: current_score DESC NULLS FIRST — a never-scored
	// subject (current_score IS NULL) is the most urgent to score, not the
	// least.
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-09-27T12:00:00Z")

	// A subject that has already been scored at a high score...
	appendAt(t, ctx, s, "acct_scored_high", "subject.created", now, nil)
	scoredClaim, err := s.ClaimDirtySubjects(ctx, now.Add(time.Minute), 0)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.UpsertVerdicts(ctx, testTenant, "acct_scored_high", scoredClaim[0].DirtySeq, []store.VerdictRecord{
		{Rule: "r", Mode: "advise", Scorer: "local", Model: "local", Status: "scored", InputHash: "h"},
	}, store.SubjectSummary{Tier: "high", Score: 0.95}); err != nil {
		t.Fatalf("UpsertVerdicts: %v", err)
	}
	// ...made dirty again by a new event...
	appendAt(t, ctx, s, "acct_scored_high", "resource.created", now.Add(2*time.Minute), map[string]any{"kind": "agent"})

	// ...at the same instant a brand-new, never-scored subject also becomes
	// dirty.
	appendAt(t, ctx, s, "acct_never_scored", "subject.created", now.Add(2*time.Minute), nil)

	claimed, err := s.ClaimDirtySubjects(ctx, now.Add(3*time.Minute), 2)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 2 {
		t.Fatalf("expected both subjects claimed, got %d: %+v", len(claimed), claimed)
	}
	if claimed[0].Subject != "acct_never_scored" {
		t.Errorf("first-ranked subject = %q, want acct_never_scored (NULLS FIRST)", claimed[0].Subject)
	}
}

func TestClaimDirtySubjects_DirtyOutranksTimerOnlyRescore(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-09-27T12:00:00Z")

	// acct_timer: scored, then given a pending next_rescore_at with no new
	// event (dirty_seq == scored_seq).
	appendAt(t, ctx, s, "acct_timer", "subject.created", now, nil)
	timerClaim, err := s.ClaimDirtySubjects(ctx, now.Add(time.Minute), 0)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.UpsertVerdicts(ctx, testTenant, "acct_timer", timerClaim[0].DirtySeq, []store.VerdictRecord{
		{Rule: "r", Mode: "advise", Scorer: "local", Model: "local", Status: "scored", InputHash: "h"},
	}, store.SubjectSummary{Tier: "low", Score: 0, NextRescoreAt: now.Add(2 * time.Minute)}); err != nil {
		t.Fatalf("UpsertVerdicts: %v", err)
	}

	// acct_dirty: a genuinely new event at the SAME evaluation instant.
	appendAt(t, ctx, s, "acct_dirty", "subject.created", now.Add(2*time.Minute), nil)

	claimed, err := s.ClaimDirtySubjects(ctx, now.Add(2*time.Minute+time.Second), 1)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 1 || claimed[0].Subject != "acct_dirty" {
		t.Fatalf("with limit=1, expected the genuinely dirty subject first, got %+v", claimed)
	}
}

func TestPutLabel_AbusivePropagatesToNeighbors(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-09-27T12:00:00Z")

	emailHash := "2222222222222222222222222222222222222222222222222222222222222222"
	emailHash = emailHash[:64]
	e1 := event.Event{ID: "e1", Subject: "acct_a", Type: "subject.created", At: now, Links: event.Links{EmailHash: emailHash}}
	e2 := event.Event{ID: "e2", Subject: "acct_b", Type: "subject.created", At: now, Links: event.Links{EmailHash: emailHash}}
	for _, e := range []*event.Event{&e1, &e2} {
		if err := e.Validate(event.ValidateOptions{Now: e.At}); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if err := e.Redact(); err != nil {
			t.Fatalf("Redact: %v", err)
		}
	}
	if _, err := s.AppendEvents(ctx, testTenant, "e2a-server", []event.Event{e1, e2}); err != nil {
		t.Fatalf("AppendEvents: %v", err)
	}

	// Score acct_b so it's no longer dirty, proving the label — not a
	// leftover dirty_seq — is what makes it dirty again.
	claimed, err := s.ClaimDirtySubjects(ctx, now.Add(time.Minute), 0)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	for _, d := range claimed {
		if d.Subject == "acct_b" {
			if _, err := s.UpsertVerdicts(ctx, testTenant, "acct_b", d.DirtySeq, []store.VerdictRecord{
				{Rule: "r", Mode: "advise", Scorer: "local", Model: "local", Status: "scored", InputHash: "h"},
			}, store.SubjectSummary{Tier: "low", Score: 0}); err != nil {
				t.Fatalf("UpsertVerdicts: %v", err)
			}
		}
	}

	if _, err := s.PutLabel(ctx, testTenant, store.Label{Subject: "acct_a", Label: "abusive", Source: "operator", Actor: "ops@example.test"}); err != nil {
		t.Fatalf("PutLabel: %v", err)
	}

	// acct_b shares acct_a's email_hash, so labelling acct_a abusive must
	// have bumped acct_b's dirty_seq (S2 fix round).
	dirtyAgain, err := s.ClaimDirtySubjects(ctx, now.Add(2*time.Minute), 0)
	if err != nil {
		t.Fatalf("claim after label: %v", err)
	}
	found := false
	for _, d := range dirtyAgain {
		if d.Subject == "acct_b" {
			found = true
		}
	}
	if !found {
		t.Fatalf("acct_b was not made dirty by acct_a's abusive label")
	}
}

func TestAppendEvents_PermanentDeletionPropagatesToNeighbors(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-09-27T12:00:00Z")

	cardHash := "3333333333333333333333333333333333333333333333333333333333333333"
	cardHash = cardHash[:64]
	e1 := event.Event{ID: "e1", Subject: "acct_a", Type: "payment.attempt", At: now, Links: event.Links{CardFingerprintHash: cardHash}, Data: map[string]any{"outcome": "declined"}}
	e2 := event.Event{ID: "e2", Subject: "acct_b", Type: "payment.attempt", At: now, Links: event.Links{CardFingerprintHash: cardHash}, Data: map[string]any{"outcome": "declined"}}
	for _, e := range []*event.Event{&e1, &e2} {
		if err := e.Validate(event.ValidateOptions{Now: e.At}); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if err := e.Redact(); err != nil {
			t.Fatalf("Redact: %v", err)
		}
	}
	if _, err := s.AppendEvents(ctx, testTenant, "e2a-server", []event.Event{e1, e2}); err != nil {
		t.Fatalf("AppendEvents: %v", err)
	}

	// Score acct_b so it's no longer dirty from the initial events.
	claimed, err := s.ClaimDirtySubjects(ctx, now.Add(time.Minute), 0)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	for _, d := range claimed {
		if d.Subject == "acct_b" {
			if _, err := s.UpsertVerdicts(ctx, testTenant, "acct_b", d.DirtySeq, []store.VerdictRecord{
				{Rule: "r", Mode: "advise", Scorer: "local", Model: "local", Status: "scored", InputHash: "h"},
			}, store.SubjectSummary{Tier: "low", Score: 0}); err != nil {
				t.Fatalf("UpsertVerdicts: %v", err)
			}
		}
	}

	appendAt(t, ctx, s, "acct_a", "subject.deleted", now.Add(2*time.Minute), map[string]any{"mode": "permanent"})

	dirtyAgain, err := s.ClaimDirtySubjects(ctx, now.Add(3*time.Minute), 0)
	if err != nil {
		t.Fatalf("claim after deletion: %v", err)
	}
	found := false
	for _, d := range dirtyAgain {
		if d.Subject == "acct_b" {
			found = true
		}
	}
	if !found {
		t.Fatalf("acct_b was not made dirty by acct_a's permanent deletion")
	}
}

func TestLatestVerdicts_DoesNotHideAPriorScoredRisk(t *testing.T) {
	// S11, proven: a later unscored (backoff/cost_cap/staged) round hid an
	// earlier round's perfectly good risk from stage:{min_local_risk:...}
	// gating.
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-09-27T12:00:00Z")

	e := mkEvent(t, "evt_1", "acct_hidden_risk", "subject.created", now, nil)
	if _, err := s.AppendEvents(ctx, testTenant, "e2a-server", []event.Event{e}); err != nil {
		t.Fatalf("AppendEvents: %v", err)
	}

	risk := 0.7
	if _, err := s.UpsertVerdicts(ctx, testTenant, "acct_hidden_risk", 1, []store.VerdictRecord{
		{Rule: "r", Mode: "advise", Scorer: "local", Model: "local", Checkpoint: "v1", Status: "scored",
			Risk: &risk, InputHash: "h1", Probs: map[string]float64{"benign": 0.3, "abusive": 0.7}},
	}, store.SubjectSummary{Tier: "medium", Score: risk}); err != nil {
		t.Fatalf("UpsertVerdicts (round 1, scored): %v", err)
	}

	// Round 2: the SAME rule goes unscored (e.g. backoff), a later row than
	// round 1.
	if _, err := s.UpsertVerdicts(ctx, testTenant, "acct_hidden_risk", 1, []store.VerdictRecord{
		{Rule: "r", Mode: "advise", Scorer: "local", Status: "unscored", ErrorCode: "backoff", InputHash: "h2"},
	}, store.SubjectSummary{Tier: "unknown", Score: 0}); err != nil {
		t.Fatalf("UpsertVerdicts (round 2, unscored): %v", err)
	}

	latest, err := s.LatestVerdicts(ctx, testTenant, "acct_hidden_risk")
	if err != nil {
		t.Fatalf("LatestVerdicts: %v", err)
	}
	lv, ok := latest["r"]
	if !ok {
		t.Fatalf("rule %q missing from LatestVerdicts", "r")
	}
	if lv.InputHash != "h2" || lv.Status != "unscored" || lv.ErrorCode != "backoff" {
		t.Errorf("latest row fields = %+v, want the literal latest (unscored) row", lv)
	}
	if lv.Risk == nil || *lv.Risk != risk {
		t.Errorf("Risk = %v, want %v carried forward from the last SCORED round", lv.Risk, risk)
	}
	if lv.Model != "local" || lv.Checkpoint != "v1" {
		t.Errorf("Model/Checkpoint = %q/%q, want the last scored round's local/v1", lv.Model, lv.Checkpoint)
	}
}

func TestPruneRuleState(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.RecordRuleError(ctx, testTenant, "acct_prune", "old_rule", mustTime(t, "2031-09-27T12:00:00Z"), "boom"); err != nil {
		t.Fatalf("RecordRuleError: %v", err)
	}
	if err := s.RecordRuleError(ctx, testTenant, "acct_prune", "current_rule", mustTime(t, "2031-09-27T12:00:00Z"), "boom"); err != nil {
		t.Fatalf("RecordRuleError: %v", err)
	}

	if err := s.PruneRuleState(ctx, testTenant, "acct_prune", []string{"current_rule"}); err != nil {
		t.Fatalf("PruneRuleState: %v", err)
	}

	old, err := s.GetRuleBackoff(ctx, testTenant, "acct_prune", "old_rule")
	if err != nil {
		t.Fatalf("GetRuleBackoff(old_rule): %v", err)
	}
	if old != (store.RuleBackoff{}) {
		t.Errorf("old_rule's backoff row should have been pruned, got %+v", old)
	}
	current, err := s.GetRuleBackoff(ctx, testTenant, "acct_prune", "current_rule")
	if err != nil {
		t.Fatalf("GetRuleBackoff(current_rule): %v", err)
	}
	if current.Attempts != 1 {
		t.Errorf("current_rule's backoff row should survive pruning, got %+v", current)
	}
}

func TestClaimDirtySubjects_UsesIndexesNotSequentialScans(t *testing.T) {
	// S4: EXPLAIN evidence that the claim query's two arms hit their
	// partial indexes rather than sequentially scanning subjects. This
	// needs a table large enough that Postgres's planner actually prefers
	// an index over a sequential scan — on a handful of rows a seq scan is
	// the CORRECT choice regardless of what indexes exist, so a tiny table
	// would prove nothing either way.
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-09-27T12:00:00Z")

	pool := openScopedPool(t, ctx)
	const bulkSubjects = 20000
	if _, err := pool.Exec(ctx, `
		INSERT INTO subjects (tenant, subject, dirty_seq, scored_seq, first_seen_at, last_event_at)
		SELECT $1, 'acct_bulk_' || g, 0, 0, now(), now() FROM generate_series(1, $2) AS g
	`, testTenant, bulkSubjects); err != nil {
		t.Fatalf("bulk insert clean subjects: %v", err)
	}
	if _, err := pool.Exec(ctx, `ANALYZE subjects`); err != nil {
		t.Fatalf("ANALYZE: %v", err)
	}
	// A small, selective set of genuinely dirty / rescore-due subjects
	// among the large clean majority above.
	appendAt(t, ctx, s, "acct_explain_dirty", "subject.created", now, nil)
	if _, err := pool.Exec(ctx, `
		UPDATE subjects SET next_rescore_at = now() - interval '1 minute'
		WHERE tenant = $1 AND subject = 'acct_bulk_1'
	`, testTenant); err != nil {
		t.Fatalf("set next_rescore_at: %v", err)
	}

	// R10 round 2: run the EXACT query text ClaimDirtySubjects itself uses
	// (store.DirtyArmQuery / store.RescoreArmQuery), parameterized exactly
	// as production does — not a hand-copied literal (with now()/interval
	// substituted for $1/$2) that could silently drift out of sync with
	// the real query if it ever changes.
	explainNow := now.Add(time.Minute)
	newSubjectCutoff := explainNow.Add(-24 * time.Hour)
	for _, q := range []string{store.DirtyArmQuery, store.RescoreArmQuery} {
		rows, err := pool.Query(ctx, "EXPLAIN "+q, explainNow, newSubjectCutoff, 200)
		if err != nil {
			t.Fatalf("EXPLAIN: %v", err)
		}
		var plan strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				rows.Close()
				t.Fatalf("scan EXPLAIN line: %v", err)
			}
			plan.WriteString(line)
			plan.WriteString("\n")
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate EXPLAIN: %v", err)
		}
		t.Logf("plan:\n%s", plan.String())
		if strings.Contains(plan.String(), "Seq Scan on subjects") {
			t.Errorf("expected an index scan, got a sequential scan of subjects:\n%s", plan.String())
		}
	}
}

func TestBudgetUsage_IncrementAndGet(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	zero, err := s.GetBudgetUsage(ctx, "2031-01-01", "adapter", "jev")
	if err != nil {
		t.Fatalf("GetBudgetUsage (before any increment): %v", err)
	}
	if zero != 0 {
		t.Fatalf("GetBudgetUsage (before any increment) = %d, want 0", zero)
	}

	for i := 1; i <= 3; i++ {
		got, err := s.IncrementBudgetUsage(ctx, "2031-01-01", "adapter", "jev")
		if err != nil {
			t.Fatalf("IncrementBudgetUsage (call %d): %v", i, err)
		}
		if got != i {
			t.Fatalf("IncrementBudgetUsage (call %d) = %d, want %d", i, got, i)
		}
	}

	got, err := s.GetBudgetUsage(ctx, "2031-01-01", "adapter", "jev")
	if err != nil {
		t.Fatalf("GetBudgetUsage: %v", err)
	}
	if got != 3 {
		t.Fatalf("GetBudgetUsage = %d, want 3", got)
	}

	// A different day/dim/key is an independent counter.
	otherDay, err := s.GetBudgetUsage(ctx, "2031-01-02", "adapter", "jev")
	if err != nil {
		t.Fatalf("GetBudgetUsage (other day): %v", err)
	}
	if otherDay != 0 {
		t.Fatalf("GetBudgetUsage (other day) = %d, want 0", otherDay)
	}
	otherDim, err := s.GetBudgetUsage(ctx, "2031-01-01", "subject", "jev")
	if err != nil {
		t.Fatalf("GetBudgetUsage (other dim): %v", err)
	}
	if otherDim != 0 {
		t.Fatalf("GetBudgetUsage (other dim) = %d, want 0", otherDim)
	}
}
