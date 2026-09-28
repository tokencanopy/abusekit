package worker

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tokencanopy/abusekit/internal/event"
	"github.com/tokencanopy/abusekit/internal/model"
	"github.com/tokencanopy/abusekit/internal/model/fake"
	"github.com/tokencanopy/abusekit/internal/store"
)

// TestSafeScore_CtxIgnoringScorerCannotHangTick is R3 round 2. Proven: the
// original safeScore called scorer.Score(callCtx, req) directly — a
// context.WithTimeout only cancels the CONTEXT, it can't force an
// uncooperative callee to return, so a scorer that ignores ctx entirely
// (blocks on its own channel/mutex/syscall) hung safeScore, and through it
// Tick, and through it Start's whole loop, forever — Stop() would then
// block on <-done for the same reason. safeScore must run Score in its own
// goroutine and race it against callCtx.Done() so a non-cooperative
// scorer can only ever hang ITSELF, never the caller.
func TestSafeScore_CtxIgnoringScorerCannotHangTick(t *testing.T) {
	scorer := fake.New()
	release := make(chan struct{}) // deliberately never closed: this scorer never returns on its own
	scorer.ScoreFunc = func(ctx context.Context, req model.ScoreRequest) (model.ScoreResult, error) {
		<-release // ignores ctx entirely, unlike fake.Scorer's own Delay handling
		return model.ScoreResult{}, nil
	}

	type outcome struct {
		res model.ScoreResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := safeScore(context.Background(), scorer, model.ScoreRequest{Labels: []string{"benign", "abusive"}}, 50*time.Millisecond)
		done <- outcome{res, err}
	}()

	select {
	case o := <-done:
		if o.err == nil {
			t.Fatalf("expected a timeout error from a ctx-ignoring scorer, got nil (result %+v)", o.res)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("safeScore did not return within 2s of a 50ms timeout — a ctx-ignoring scorer hung it")
	}
}

// TestSafeScore_StillRecoversAPanicInTheGoroutine is R3 round 2's
// companion: running Score in its own goroutine must not lose B2's
// existing panic recovery — a panic there must still surface as a plain
// error, not crash the test binary.
func TestSafeScore_StillRecoversAPanicInTheGoroutine(t *testing.T) {
	scorer := fake.New()
	scorer.ScoreFunc = func(ctx context.Context, req model.ScoreRequest) (model.ScoreResult, error) {
		panic("synthetic scorer panic")
	}

	_, err := safeScore(context.Background(), scorer, model.ScoreRequest{Labels: []string{"benign", "abusive"}}, time.Second)
	if err == nil {
		t.Fatalf("expected a panic to surface as an error")
	}
}

// TestStartStop_ConcurrentStartAndStopRace is R9 round 2. Proven risk: the
// original Start set w.cancel/w.done under w.mu, and Stop read them under
// the SAME mutex — individually race-free, but if a caller's own
// `go w.Start(ctx); w.Stop()` pattern (Start's own doc comment says Stop
// is callable from another goroutine while Start runs in its own — that's
// the whole reason Stop exists as a separate method) has Stop's goroutine
// win the race and run BEFORE Start has gotten far enough to set w.cancel,
// Stop sees a nil cancel and treats it as "Start was never called",
// returning immediately as a no-op — even though Start is about to run its
// scoring loop indefinitely, now with nothing left to ever stop it.
//
// Repeated many times (the race window is a handful of instructions
// between the `go` statement and Start's first mutex acquisition — small,
// but real, and -race widens it) rather than relying on a single
// precisely-timed reproduction.
func TestStartStop_ConcurrentStartAndStopRace(t *testing.T) {
	s := newTestStore(t)
	cfg := loadShippedConfig(t)

	const iterations = 200
	for i := 0; i < iterations; i++ {
		w, err := New(Deps{Store: s, Config: cfg, Interval: time.Millisecond, Logger: discardLogger()})
		if err != nil {
			t.Fatalf("iteration %d: New: %v", i, err)
		}

		startReturned := make(chan struct{})
		go func() {
			w.Start(context.Background())
			close(startReturned)
		}()
		w.Stop() // races Start's own goroutine, deliberately with no synchronization in between

		select {
		case <-startReturned:
			// expected: Start honored the (possibly pre-empted) stop and
			// returned, whether or not it ever ran a tick.
		case <-time.After(2 * time.Second):
			t.Fatalf("iteration %d: Start did not return after a concurrent Stop — the Start/Stop race left it running forever", i)
		}
	}
}

// R9 round 2's other half ("Stop on a never-started worker is a no-op")
// is already covered by robustness_test.go's existing
// TestStop_NoopWithoutStart, unaffected by this round's changes — not
// duplicated here.

// TestTick_LeaseSurvivesBatchLongerThanOneLease is R4 round 2, using the
// reviewer's own numbers: a 200ms claim lease, three subjects, a scorer
// that genuinely takes 150ms (real wall-clock time, unconditionally — not
// gated on ctx, so R3's cancellation short-circuit doesn't mask this) per
// call. The batch's own total processing time (450ms) already exceeds the
// lease a single tick-start claim sets, so without extending each
// remaining subject's lease as the batch is worked through, a SECOND
// worker instance sharing the same database would reclaim and re-score a
// subject still legitimately being processed by the first.
//
// Two real *store.Store values (two pools, ONE shared throwaway database —
// exactly like two real worker processes sharing one production Postgres)
// with a short lease via store.WithClaimLease, since DefaultClaimLease's
// real 2 minutes would make this test impractically slow.
func TestTick_LeaseSurvivesBatchLongerThanOneLease(t *testing.T) {
	dbURL, ok := newThrowawayDatabaseURL(t)
	if !ok {
		return // newThrowawayDatabaseURL already skipped/failed the test
	}
	ctx := context.Background()
	const lease = 200 * time.Millisecond

	poolA, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("open pool A: %v", err)
	}
	t.Cleanup(poolA.Close)
	storeA := store.New(poolA, store.WithClaimLease(lease))
	if err := storeA.ApplyMigrations(ctx); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	poolB, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("open pool B: %v", err)
	}
	t.Cleanup(poolB.Close)
	storeB := store.New(poolB, store.WithClaimLease(lease))

	// A REAL, advancing clock for both workers — not a frozen fake one:
	// this test's whole point is that real elapsed wall-clock time (the
	// scorer's real time.Sleep calls) outlasts a single tick-start lease,
	// which a fixed Deps.Now would never actually exercise (claimed_until
	// would forever stay a fixed offset ahead of an unmoving "now").
	realNow := func() time.Time { return time.Now().UTC() }
	eventAt := time.Now().UTC().Add(-time.Hour)

	subjects := []string{"acct_r2_lease_1", "acct_r2_lease_2", "acct_r2_lease_3"}
	for _, subj := range subjects {
		appendEvent(t, ctx, storeA, subj, "subject.created", eventAt, event.Links{}, map[string]any{})
	}

	var totalCalls int32
	scorer := scorerFunc{name: "fake_test_scorer", fn: func(ctx context.Context, req model.ScoreRequest) (model.ScoreResult, error) {
		atomic.AddInt32(&totalCalls, 1)
		time.Sleep(150 * time.Millisecond) // unconditional: does not check ctx at all
		return model.ScoreResult{Probs: map[string]float64{"benign": 1, "abusive": 0}}, nil
	}}
	cfg := newFakeRuleConfigWithScorer(t, scorer)

	wA, err := New(Deps{Store: storeA, Config: cfg, BatchSize: 10, Now: realNow, Logger: discardLogger()})
	if err != nil {
		t.Fatalf("New wA: %v", err)
	}
	wB, err := New(Deps{Store: storeB, Config: cfg, BatchSize: 10, Now: realNow, Logger: discardLogger()})
	if err != nil {
		t.Fatalf("New wB: %v", err)
	}

	tickADone := make(chan struct{})
	go func() {
		defer close(tickADone)
		if _, err := wA.Tick(ctx); err != nil {
			t.Errorf("wA.Tick: %v", err)
		}
	}()

	// Give wA time to claim the batch and start processing (well within
	// subject 1's own 150ms call, comfortably before the ORIGINAL
	// tick-start lease would expire at 200ms).
	time.Sleep(50 * time.Millisecond)

	// wB polls repeatedly while wA works through its batch — spanning past
	// the point (200ms) where the ORIGINAL, un-extended lease on subjects
	// 2 and 3 would have expired, and into the window (300ms+) where
	// subject 3 specifically would have been vulnerable under a
	// current-subject-only extension.
	for i := 0; i < 6; i++ {
		bResult, err := wB.Tick(ctx)
		if err != nil {
			t.Fatalf("wB.Tick (poll %d): %v", i, err)
		}
		if bResult.Claimed != 0 {
			t.Errorf("wB.Tick (poll %d) claimed %d subjects while wA's batch was still in flight — a subject's lease expired out from under wA", i, bResult.Claimed)
		}
		time.Sleep(75 * time.Millisecond)
	}

	select {
	case <-tickADone:
	case <-time.After(5 * time.Second):
		t.Fatalf("wA.Tick did not finish")
	}

	if got := atomic.LoadInt32(&totalCalls); got != int32(len(subjects)) {
		t.Errorf("scorer called %d times total across both workers, want exactly %d (no subject scored twice)", got, len(subjects))
	}
}
