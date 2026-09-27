package worker

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tokencanopy/abusekit/internal/event"
	"github.com/tokencanopy/abusekit/internal/model"
	"github.com/tokencanopy/abusekit/internal/model/fake"
)

// TestScoreSubject_AgeDriftAloneReusesResultAcrossRounds is R7 round 2's
// integration-level proof, exercising the real store/worker wiring (not
// just internal/core's pure inputHash unit test): a subject whose ONLY
// input to a rule is subject_age_h, rescored repeatedly with no new event
// at all — only elapsed time passing — must call the scorer at most twice
// across 10 rounds (once at the start, and at most once more when
// subject_age_h happens to cross into the next hour bucket partway
// through), not once per round.
//
// scoreSubject is called directly (bypassing ClaimDirtySubjects'
// claim/lease machinery, which R3/R4 cover on their own) with a fixed
// DirtySubject and an advancing clock, so this test is specifically about
// whether Plan's input_unchanged skip actually fires — the one thing R7's
// fix (core.quantizeAgeFeaturesForHash) is responsible for.
func TestScoreSubject_AgeDriftAloneReusesResultAcrossRounds(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	var calls int32
	scorer := fake.New()
	scorer.ScoreFunc = func(context.Context, model.ScoreRequest) (model.ScoreResult, error) {
		atomic.AddInt32(&calls, 1)
		return model.ScoreResult{Probs: map[string]float64{"benign": 0.9, "abusive": 0.1}, Model: "fake", Checkpoint: "fake-1"}, nil
	}
	cfg := newFakeRuleConfig(t, scorer)

	const subject = "acct_example_r2_hash_stable"
	created, err := time.Parse(time.RFC3339, "2031-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("parse created time: %v", err)
	}
	appendEvent(t, ctx, s, subject, "subject.created", created, event.Links{}, map[string]any{})

	dirty, err := s.ClaimDirtySubjects(ctx, created, 10)
	if err != nil {
		t.Fatalf("ClaimDirtySubjects: %v", err)
	}
	if len(dirty) != 1 || dirty[0].Subject != subject {
		t.Fatalf("ClaimDirtySubjects = %+v, want exactly the one subject just created", dirty)
	}
	d := dirty[0]

	// baseNow: subject_age_h starts at 5.0 (comfortably under the 24h
	// clamp, so it keeps drifting rather than sitting still). 10 rounds, 8
	// minutes apart, span 72 minutes — enough to cross from hour-bucket 5
	// into hour-bucket 6 partway through, matching R7's own "<=2 calls"
	// (not exactly 1): one call for the initial round, and at most one
	// more for the bucket crossing.
	baseNow := created.Add(5 * time.Hour)
	w, err := New(Deps{Store: s, Config: cfg, Neighbors: nil})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for i := 0; i < 10; i++ {
		now := baseNow.Add(time.Duration(i) * 8 * time.Minute)
		scored, err := w.scoreSubject(ctx, d, now)
		if err != nil {
			t.Fatalf("round %d: scoreSubject: %v", i, err)
		}
		if !scored {
			t.Fatalf("round %d: scoreSubject returned stale, want committed", i)
		}
	}

	if got := atomic.LoadInt32(&calls); got > 2 {
		t.Errorf("scorer called %d times across 10 rounds with no new input (only elapsed time changed), want <= 2", got)
	}
}

// TestScoreSubject_ErroredRoundNeverSkipsAsInputUnchanged is a direct
// regression test for a bug R7's own fix (age-feature quantization)
// exposed: an ERRORED round's input hash must never be treated as
// "unchanged, reuse it" once a rule's backoff clears — the round that
// produced that hash never actually answered, so there is nothing valid
// to reuse. Before worker.go's fix (only a "scored" LatestVerdict feeds
// RuleState.LastInputHash), a still-quantization-stable subject_age_h
// made a past-backoff rule silently skip retrying forever, stuck on the
// same recorded error.
func TestScoreSubject_ErroredRoundNeverSkipsAsInputUnchanged(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	const subject = "acct_example_r2_err_no_skip"
	created, err := time.Parse(time.RFC3339, "2031-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("parse created time: %v", err)
	}
	appendEvent(t, ctx, s, subject, "subject.created", created, event.Links{}, map[string]any{})

	var calls int32
	scorer := fake.New()
	scorer.ScoreFunc = func(context.Context, model.ScoreRequest) (model.ScoreResult, error) {
		atomic.AddInt32(&calls, 1)
		return model.ScoreResult{}, errors.New("synthetic adapter failure")
	}
	cfg := newFakeRuleConfig(t, scorer)

	dirty, err := s.ClaimDirtySubjects(ctx, created, 10)
	if err != nil {
		t.Fatalf("ClaimDirtySubjects: %v", err)
	}
	if len(dirty) != 1 {
		t.Fatalf("ClaimDirtySubjects = %+v, want exactly one subject", dirty)
	}
	d := dirty[0]

	// now1: subject_age_h ~= 5.0h. now2: ~5.001h later, well past the 30s
	// rule backoff schedule but still inside the SAME 1-hour hash bucket —
	// exactly the case R7's quantization makes common.
	now1 := created.Add(5 * time.Hour)
	w, err := New(Deps{Store: s, Config: cfg, Now: func() time.Time { return now1 }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := w.scoreSubject(ctx, d, now1); err != nil {
		t.Fatalf("round 1: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("round 1: scorer called %d times, want 1", got)
	}

	now2 := now1.Add(45 * time.Second) // past the 30s backoff, same hour bucket
	if _, err := w.scoreSubject(ctx, d, now2); err != nil {
		t.Fatalf("round 2: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("round 2 (past backoff, same age bucket): scorer called %d times total, want 2 (must retry, not silently skip on the errored round's stale hash)", got)
	}

	backoff, err := s.GetRuleBackoff(ctx, testTenant, subject, "fake_rule")
	if err != nil {
		t.Fatalf("GetRuleBackoff: %v", err)
	}
	if backoff.Attempts != 2 {
		t.Errorf("attempts after round 2 = %d, want 2 (the retry must actually count as a new failed attempt)", backoff.Attempts)
	}
}
