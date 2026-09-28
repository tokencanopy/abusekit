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
