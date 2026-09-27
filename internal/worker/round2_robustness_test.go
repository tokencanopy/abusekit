package worker

import (
	"context"
	"testing"
	"time"

	"github.com/tokencanopy/abusekit/internal/model"
	"github.com/tokencanopy/abusekit/internal/model/fake"
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
