package store_test

// Tests for the S3 (HTTP surface) additions to internal/store:
// ClaimSubjectForEvaluate and InsertCorpusExample. Kept in their own file,
// matching s2_test.go's own "one file per slice" convention for this
// package's test suite.
//
// ListSubjects/EraseSubject/HasAbusiveLabel and their tests were pulled
// out in the S3 fix round (X1: scope split) — see feat/s3b-list-erasure
// and docs/design/notes/erasure-findings.md on that branch.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tokencanopy/abusekit/internal/store"
)

func TestClaimSubjectForEvaluate_ClaimsAndReturnsState(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-09-27T12:00:00Z")

	appendAt(t, ctx, s, "acct_eval_1", "subject.created", now, nil)

	d, err := s.ClaimSubjectForEvaluate(ctx, testTenant, "acct_eval_1", now)
	if err != nil {
		t.Fatalf("ClaimSubjectForEvaluate: %v", err)
	}
	if d.Subject != "acct_eval_1" || d.DirtySeq != 1 || d.ScoredSeq != 0 {
		t.Fatalf("unexpected claim state: %+v", d)
	}
}

func TestClaimSubjectForEvaluate_NotFoundForUnseenSubject(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-09-27T12:00:00Z")

	_, err := s.ClaimSubjectForEvaluate(ctx, testTenant, "never_seen", now)
	if err != store.ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// TestClaimSubjectForEvaluate_RejectsInternalAndSynthetic is S1: an
// internal/synthetic subject is ErrNotScorable specifically (not a generic
// error) — internal/serve maps this to a 200 + stored view, never a 500.
func TestClaimSubjectForEvaluate_RejectsInternalAndSynthetic(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-09-27T12:00:00Z")

	appendAt(t, ctx, s, "acct_internal_eval", "subject.created", now, nil)
	appendAt(t, ctx, s, "acct_internal_eval", "subject.class", now.Add(time.Second), map[string]any{"class": "internal"})

	_, err := s.ClaimSubjectForEvaluate(ctx, testTenant, "acct_internal_eval", now)
	if !errors.Is(err, store.ErrNotScorable) {
		t.Fatalf("expected ErrNotScorable claiming an internal-class subject for evaluate, got %v", err)
	}
}

// TestClaimSubjectForEvaluate_AlreadyClaimedByWorker proves evaluate and
// the worker's own ClaimDirtySubjects can never both be mid-scoring-round
// for the same subject at once — design's "same lease... rules" for
// evaluate. S1 fix round: the returned *ErrBusy carries a REAL RetryAt
// derived from the worker's own claim lease, not a fixed guess.
func TestClaimSubjectForEvaluate_AlreadyClaimedByWorker(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-09-27T12:00:00Z")

	appendAt(t, ctx, s, "acct_eval_race", "subject.created", now, nil)

	dirty, err := s.ClaimDirtySubjects(ctx, now, 0)
	if err != nil {
		t.Fatalf("ClaimDirtySubjects: %v", err)
	}
	if len(dirty) != 1 {
		t.Fatalf("expected the worker to claim exactly one subject, got %d", len(dirty))
	}

	_, err = s.ClaimSubjectForEvaluate(ctx, testTenant, "acct_eval_race", now)
	var busy *store.ErrBusy
	if !errors.As(err, &busy) {
		t.Fatalf("expected *store.ErrBusy while the worker holds the claim, got %v", err)
	}
	// The worker's default claim lease is 2 minutes (store.DefaultClaimLease)
	// — RetryAt must reflect roughly that, not a short fixed guess like 1s.
	if wait := busy.RetryAt.Sub(now); wait < 90*time.Second || wait > store.DefaultClaimLease+time.Second {
		t.Fatalf("RetryAt = %v after now (%v), want close to the %v claim lease", wait, now, store.DefaultClaimLease)
	}
}

// TestClaimSubjectForEvaluate_BusyFromFailureBackoff proves the SAME
// *ErrBusy path also fires (with a RetryAt derived from next_attempt_at)
// when a subject is in whole-pass failure backoff, not just when it's
// claimed.
func TestClaimSubjectForEvaluate_BusyFromFailureBackoff(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-09-27T12:00:00Z")

	appendAt(t, ctx, s, "acct_eval_backoff", "subject.created", now, nil)
	retryAt := now.Add(5 * time.Minute)
	if err := s.RecordSubjectFailure(ctx, testTenant, "acct_eval_backoff", retryAt); err != nil {
		t.Fatalf("RecordSubjectFailure: %v", err)
	}

	_, err := s.ClaimSubjectForEvaluate(ctx, testTenant, "acct_eval_backoff", now)
	var busy *store.ErrBusy
	if !errors.As(err, &busy) {
		t.Fatalf("expected *store.ErrBusy during failure backoff, got %v", err)
	}
	if !busy.RetryAt.Equal(retryAt) {
		t.Fatalf("RetryAt = %v, want exactly the recorded next_attempt_at %v", busy.RetryAt, retryAt)
	}
}

func TestClaimSubjectForEvaluate_ReclaimableAfterRelease(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-09-27T12:00:00Z")

	appendAt(t, ctx, s, "acct_eval_release", "subject.created", now, nil)

	claimed, err := s.ClaimSubjectForEvaluate(ctx, testTenant, "acct_eval_release", now)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if err := s.ReleaseClaim(ctx, testTenant, "acct_eval_release", claimed.ClaimedUntil); err != nil {
		t.Fatalf("ReleaseClaim: %v", err)
	}
	if _, err := s.ClaimSubjectForEvaluate(ctx, testTenant, "acct_eval_release", now); err != nil {
		t.Fatalf("second claim after release: %v", err)
	}
}

// TestReleaseClaim_DoesNotClearANewerLease is T3 (round 3): ReleaseClaim
// must compare-and-clear against the EXACT claimed_until value the caller
// itself set, not clear the row unconditionally. Reproduces the scenario
// worker.EvaluateSubject's two release call sites (the ErrClaimAmbiguous
// path and the release-on-not-committed defer) both guard against: a
// caller's own claim attempt becomes stale (here, released for real by an
// UNRELATED path — in production this is an ambiguous-commit outcome or a
// round that never committed) and, before that staleness is discovered,
// the worker's own Tick legitimately claims the SAME subject with a NEW
// lease. The stale caller's eventual (delayed) release call must be a
// no-op against that newer lease, not clear it out from under Tick.
func TestReleaseClaim_DoesNotClearANewerLease(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-09-27T12:00:00Z")

	appendAt(t, ctx, s, "acct_release_cas", "subject.created", now, nil)

	// The "stale" caller's own claim — its ClaimedUntil is what a delayed
	// release call will (incorrectly, if unconditional) try to clear
	// later.
	stale, err := s.ClaimSubjectForEvaluate(ctx, testTenant, "acct_release_cas", now)
	if err != nil {
		t.Fatalf("stale claim: %v", err)
	}

	// That claim is released for real through the ordinary path (standing
	// in for however it actually became stale in production — an
	// ambiguous commit, or a round that errored before UpsertVerdicts).
	if err := s.ReleaseClaim(ctx, testTenant, "acct_release_cas", stale.ClaimedUntil); err != nil {
		t.Fatalf("release the stale claim: %v", err)
	}

	// The worker's own Tick claims the subject for real, at a LATER
	// instant so its ClaimedUntil is a genuinely DIFFERENT value than the
	// stale claim's.
	tickNow := now.Add(time.Hour)
	tickClaims, err := s.ClaimDirtySubjects(ctx, tickNow, 0)
	if err != nil {
		t.Fatalf("ClaimDirtySubjects: %v", err)
	}
	var tick store.DirtySubject
	for _, c := range tickClaims {
		if c.Subject == "acct_release_cas" {
			tick = c
		}
	}
	if tick.Subject == "" {
		t.Fatalf("expected Tick to claim acct_release_cas, got %+v", tickClaims)
	}
	if tick.ClaimedUntil.Equal(stale.ClaimedUntil) {
		t.Fatalf("test setup broken: Tick's ClaimedUntil (%v) must differ from the stale claim's (%v)", tick.ClaimedUntil, stale.ClaimedUntil)
	}

	// The stale caller's own (delayed) release call arrives now, still
	// carrying its OWN old ClaimedUntil — it must be a no-op against
	// Tick's newer lease, not clear it.
	if err := s.ReleaseClaim(ctx, testTenant, "acct_release_cas", stale.ClaimedUntil); err != nil {
		t.Fatalf("stale ReleaseClaim: %v", err)
	}

	// Tick's lease must have survived: a fresh evaluate claim attempt at
	// tickNow must see *store.ErrBusy, not succeed.
	_, err = s.ClaimSubjectForEvaluate(ctx, testTenant, "acct_release_cas", tickNow)
	var busy *store.ErrBusy
	if !errors.As(err, &busy) {
		t.Fatalf("expected Tick's lease to survive the stale release (want *store.ErrBusy), got %v", err)
	}
}

// TestClaimSubjectForEvaluate_ClaimedUntilIsMicrosecondPrecise is a
// follow-up to T3 (CI red on Linux, 4609030): timestamptz stores
// microsecond precision, but a real wall-clock-derived time.Time commonly
// carries a nonzero sub-microsecond (nanosecond) remainder on Linux —
// naively storing that raw, untruncated value as DirtySubject.ClaimedUntil
// (as this code did before this fix) meant ReleaseClaim's later
// compare-and-clear could be handed a value that no longer matches
// whatever the row's own claimed_until actually is by the time a
// different code path reads it back, since only the ROW is guaranteed
// microsecond-truncated, not an untruncated Go-side value kept around in
// memory. This is deterministic given ANY nanosecond-bearing clock, on
// ANY OS — unlike the real Linux CI failure itself (a narrow, real-timing
// race in the ambiguous-commit path that a local run doesn't reliably
// hit), this test doesn't depend on hitting that race: it directly checks
// the INVARIANT the fix establishes, that ClaimedUntil is always
// microsecond-aligned by construction, never a raw nanosecond-bearing
// value.
func TestClaimSubjectForEvaluate_ClaimedUntilIsMicrosecondPrecise(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	// 789 nanoseconds below the microsecond boundary — deliberately NOT a
	// multiple of 1000ns, so a bug that stores the raw value is caught
	// regardless of which specific sub-microsecond digits it happens to
	// use.
	now := time.Date(2031, time.January, 1, 0, 0, 0, 123456789, time.UTC)

	appendAt(t, ctx, s, "acct_claim_ns", "subject.created", now, nil)

	claimed, err := s.ClaimSubjectForEvaluate(ctx, testTenant, "acct_claim_ns", now)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed.ClaimedUntil.Nanosecond()%1000 != 0 {
		t.Fatalf("expected ClaimedUntil truncated to microsecond precision, got nanosecond=%d (%v)", claimed.ClaimedUntil.Nanosecond(), claimed.ClaimedUntil)
	}

	// The exact value handed back must also be what ReleaseClaim's
	// compare-and-clear needs: releasing with it must actually clear the
	// row, not silently no-op.
	if err := s.ReleaseClaim(ctx, testTenant, "acct_claim_ns", claimed.ClaimedUntil); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := s.ClaimSubjectForEvaluate(ctx, testTenant, "acct_claim_ns", now); err != nil {
		var busy *store.ErrBusy
		if errors.As(err, &busy) {
			t.Fatalf("expected the claim to have been released (ClaimedUntil matched the stored row), got *store.ErrBusy: %+v", busy)
		}
		t.Fatalf("second claim: %v", err)
	}
}

func TestInsertCorpusExample_RoundTrips(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-09-27T12:00:00Z")

	appendAt(t, ctx, s, "acct_corpus_1", "subject.created", now, map[string]any{"channel": "signup"})
	labelID, err := s.PutLabel(ctx, testTenant, store.Label{Subject: "acct_corpus_1", Label: "benign", Source: "operator", Actor: "test-operator"})
	if err != nil {
		t.Fatalf("PutLabel: %v", err)
	}

	id, err := s.InsertCorpusExample(ctx, testTenant, store.CorpusExample{
		Subject:    "acct_corpus_1",
		LabelID:    labelID,
		DecisionAt: now,
		EventSlice: []map[string]any{{"id": "e1", "type": "subject.created"}},
		Features:   map[string]float64{"subject_age_h": 0, "resource_total": 1},
		Split:      "train",
	})
	if err != nil {
		t.Fatalf("InsertCorpusExample: %v", err)
	}
	if id == 0 {
		t.Fatalf("expected a non-zero corpus example id")
	}
}

// TestListCorpusExamples_JoinsLabelAndFiltersSplit is S4's own addition
// (cmd/abusekit's `corpus export`): two corpus_examples rows, one train
// one test, each joined back to its own labels row's label/source/rule —
// and a --split filter that only ever returns the matching one.
func TestListCorpusExamples_JoinsLabelAndFiltersSplit(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-09-27T12:00:00Z")

	appendAt(t, ctx, s, "acct_corpus_train", "subject.created", now, map[string]any{"channel": "signup"})
	trainLabelID, err := s.PutLabel(ctx, testTenant, store.Label{Subject: "acct_corpus_train", Label: "benign", Source: "operator", Actor: "test-operator"})
	if err != nil {
		t.Fatalf("PutLabel(train): %v", err)
	}
	if _, err := s.InsertCorpusExample(ctx, testTenant, store.CorpusExample{
		Subject: "acct_corpus_train", LabelID: trainLabelID, DecisionAt: now,
		EventSlice: []map[string]any{{"id": "e1", "type": "subject.created"}},
		Features:   map[string]float64{"subject_age_h": 0},
		Split:      "train",
	}); err != nil {
		t.Fatalf("InsertCorpusExample(train): %v", err)
	}

	appendAt(t, ctx, s, "acct_corpus_test", "subject.created", now, map[string]any{"channel": "signup"})
	testLabelID, err := s.PutLabel(ctx, testTenant, store.Label{Subject: "acct_corpus_test", Rule: "new_account_velocity", Label: "abusive", Source: "outcome", Actor: "test-operator"})
	if err != nil {
		t.Fatalf("PutLabel(test): %v", err)
	}
	if _, err := s.InsertCorpusExample(ctx, testTenant, store.CorpusExample{
		Subject: "acct_corpus_test", LabelID: testLabelID, DecisionAt: now,
		EventSlice: []map[string]any{{"id": "e2", "type": "subject.created"}},
		Features:   map[string]float64{"subject_age_h": 1},
		Split:      "test",
	}); err != nil {
		t.Fatalf("InsertCorpusExample(test): %v", err)
	}

	all, err := s.ListCorpusExamples(ctx, testTenant, "all")
	if err != nil {
		t.Fatalf("ListCorpusExamples(all): %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("ListCorpusExamples(all) returned %d rows, want 2", len(all))
	}

	trainOnly, err := s.ListCorpusExamples(ctx, testTenant, "train")
	if err != nil {
		t.Fatalf("ListCorpusExamples(train): %v", err)
	}
	if len(trainOnly) != 1 || trainOnly[0].Subject != "acct_corpus_train" || trainOnly[0].Label != "benign" || trainOnly[0].LabelSource != "operator" {
		t.Fatalf("ListCorpusExamples(train) = %+v, want exactly the acct_corpus_train row with its label/source joined", trainOnly)
	}

	testOnly, err := s.ListCorpusExamples(ctx, testTenant, "test")
	if err != nil {
		t.Fatalf("ListCorpusExamples(test): %v", err)
	}
	if len(testOnly) != 1 || testOnly[0].Subject != "acct_corpus_test" || testOnly[0].Label != "abusive" || testOnly[0].LabelSource != "outcome" || testOnly[0].Rule != "new_account_velocity" {
		t.Fatalf("ListCorpusExamples(test) = %+v, want exactly the acct_corpus_test row with its label/source/rule joined", testOnly)
	}
	if testOnly[0].Gated {
		t.Errorf("Gated = true, want false — nothing in this repo sets it yet (see corpus_cmd.go's doc comment)")
	}
}
