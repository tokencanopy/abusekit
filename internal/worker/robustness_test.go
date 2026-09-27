package worker

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/tokencanopy/abusekit/internal/config"
	"github.com/tokencanopy/abusekit/internal/event"
	"github.com/tokencanopy/abusekit/internal/feature"
	"github.com/tokencanopy/abusekit/internal/model"
)

// discardLogger is used by tests that deliberately trigger a logged
// failure (e.g. Stop cancelling an in-flight Tick) and don't want it
// cluttering test output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestTick_WholePassFailureRecordsSubjectFailure(t *testing.T) {
	// B3 fix round: a scoring pass that fails outright (not one rule's
	// scorer — the whole pass, e.g. EventsForSubject erroring) must back
	// off the SUBJECT via store.RecordSubjectFailure, or it would be
	// reclaimed and retried every single tick forever (proven).
	s := newTestStore(t)
	ctx := context.Background()
	cfg := loadShippedConfig(t)

	appendEvent(t, ctx, s, "acct_worker_wholepass_fail", "subject.created", replayBase, event.Links{}, nil)

	fs := &fakeStore{real: s}
	fs.setEventsForSubjectErr(errInjected)

	now := replayBase.Add(time.Minute)
	w, err := New(Deps{Store: fs, Config: cfg, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	result, err := w.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("result.Errors = %v, want exactly 1 whole-pass failure", result.Errors)
	}

	failures := fs.recordedFailures()
	if len(failures) != 1 {
		t.Fatalf("RecordSubjectFailure called %d times, want 1", len(failures))
	}
	if failures[0].Subject != "acct_worker_wholepass_fail" {
		t.Errorf("failure recorded for %q, want acct_worker_wholepass_fail", failures[0].Subject)
	}
	wantRetryAt := now.Add(30 * time.Second) // fail_count starts at 0 -> backoffDuration(1) = 30s
	if !failures[0].NextAttemptAt.Equal(wantRetryAt) {
		t.Errorf("NextAttemptAt = %v, want %v (30s schedule)", failures[0].NextAttemptAt, wantRetryAt)
	}

	// The failure must actually back the subject off: an immediate re-tick
	// (fixing the injected error first) must not reclaim it yet.
	fs.setEventsForSubjectErr(nil)
	tooSoon, err := w.Tick(ctx)
	if err != nil {
		t.Fatalf("second Tick: %v", err)
	}
	if tooSoon.Claimed != 0 {
		t.Errorf("second Tick claimed %d subjects, want 0 (still within the failure backoff window)", tooSoon.Claimed)
	}
}

func TestSafeScore_RecoversPanic(t *testing.T) {
	// B2 fix round, proven necessary: a panicking scorer must never escape
	// Tick or kill Start's loop.
	panicking := scorerFunc{name: "panics", fn: func(context.Context, model.ScoreRequest) (model.ScoreResult, error) {
		panic("synthetic scorer panic")
	}}
	_, err := safeScore(context.Background(), panicking, model.ScoreRequest{}, time.Second)
	if err == nil {
		t.Fatalf("expected safeScore to convert the panic into an error")
	}
}

func TestSafeScore_EnforcesPerCallTimeout(t *testing.T) {
	slow := scorerFunc{name: "slow", fn: func(ctx context.Context, req model.ScoreRequest) (model.ScoreResult, error) {
		select {
		case <-time.After(time.Second):
			return model.ScoreResult{}, nil
		case <-ctx.Done():
			return model.ScoreResult{}, ctx.Err()
		}
	}}
	start := time.Now()
	_, err := safeScore(context.Background(), slow, model.ScoreRequest{}, 20*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("expected a timeout error")
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("safeScore took %v, want it to time out near the 20ms deadline, not wait out the full 1s call", elapsed)
	}
}

// scorerFunc is a minimal model.Scorer for exercising safeScore directly
// without pulling in internal/model/fake's full Capabilities/Policy setup.
type scorerFunc struct {
	name string
	fn   func(context.Context, model.ScoreRequest) (model.ScoreResult, error)
}

func (s scorerFunc) Name() string { return s.name }
func (s scorerFunc) Capabilities() model.Capabilities {
	return model.Capabilities{LabelMode: model.OpenLabelMode(), AcceptsFeatures: true, Calibrated: true}
}
func (s scorerFunc) Policy() model.DataPolicy { return model.DataPolicy{} }
func (s scorerFunc) Version() string          { return "v1" }
func (s scorerFunc) Score(ctx context.Context, req model.ScoreRequest) (model.ScoreResult, error) {
	return s.fn(ctx, req)
}

func TestStop_ReturnsPromptlyEvenWithACtxIgnoringScorerBlocked(t *testing.T) {
	// B2 fix round established "Stop must cancel AND WAIT". R3 round 2
	// refines what WAIT means: Stop now waits for safeScore's own bounded,
	// cancellation-driven unwind (near-instant once cancel() propagates
	// into safeScore's callCtx), not unconditionally for the underlying
	// scorer call to return on its own — a scorer that never checks ctx at
	// all (blocks on a channel/mutex/syscall forever) must never be able
	// to hang Stop(), which is exactly what round 1's original version of
	// this test (asserting Stop blocked until a manually-closed `release`
	// channel unblocked the very same kind of ctx-ignoring scorer) was
	// unknowingly relying on as if it were a virtue.
	s := newTestStore(t)
	ctx := context.Background()

	appendEvent(t, ctx, s, "acct_worker_stop_blocks", "subject.created", replayBase, event.Links{}, nil)

	release := make(chan struct{}) // deliberately never closed: this scorer never returns on its own
	scorerReturned := make(chan struct{})
	scorer := scorerFunc{name: "fake_test_scorer", fn: func(ctx context.Context, req model.ScoreRequest) (model.ScoreResult, error) {
		<-release
		close(scorerReturned)
		return model.ScoreResult{Probs: map[string]float64{"benign": 1, "abusive": 0}}, nil
	}}
	cfg := newFakeRuleConfigWithScorer(t, scorer)

	now := replayBase.Add(time.Minute)
	w, err := New(Deps{
		Store: s, Config: cfg, Interval: 5 * time.Millisecond,
		Now: func() time.Time { return now }, Logger: discardLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	go w.Start(ctx)
	// Give Start's ticker time to fire and enter the blocked scorer call.
	time.Sleep(100 * time.Millisecond)

	stopped := make(chan struct{})
	go func() {
		w.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
		// expected: Stop does not wait for the scorer's own channel.
	case <-time.After(2 * time.Second):
		t.Fatalf("Stop did not return promptly — a ctx-ignoring scorer hung it")
	}

	select {
	case <-scorerReturned:
		t.Fatalf("the scorer's own call returned before Stop did — this test no longer exercises the ctx-ignoring case it's named for")
	default:
		// expected: the orphaned goroutine is still blocked on release: Stop
		// returned via safeScore's cancellation path, not by waiting for it.
	}
	close(release) // let the orphaned goroutine exit rather than leaking past the test.
}

func TestStop_NoopWithoutStart(t *testing.T) {
	s := newTestStore(t)
	cfg := loadShippedConfig(t)
	w, err := New(Deps{Store: s, Config: cfg})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	done := make(chan struct{})
	go func() {
		w.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("Stop blocked forever when Start was never called")
	}
}

// newFakeRuleConfigWithScorer is like newFakeRuleConfig but takes an
// arbitrary model.Scorer (e.g. scorerFunc) rather than requiring a
// *fake.Scorer specifically.
func newFakeRuleConfigWithScorer(t *testing.T, scorer model.Scorer) *config.Config {
	t.Helper()
	reg := model.NewRegistry()
	if err := reg.Register(scorer); err != nil {
		t.Fatalf("register scorer: %v", err)
	}
	const rulesYAML = `
tiers: {medium: 0.4, high: 0.8}
min_scored_advise: 1
rules:
  - name: fake_rule
    mode: advise
    scorer: fake_test_scorer
    inputs: [subject_age_h]
    labels: [benign, abusive]
    benign_label: benign
    threshold: 0.5
`
	cfg, err := config.Load([]byte(rulesYAML), config.Dependencies{
		Registry: reg,
		Features: config.NewFeatureSet(feature.Names...),
		Vendors: map[string]config.VendorEntry{
			"fake_test_scorer": {Name: "fake_test_scorer", TermsVersion: "n/a", DPARef: "n/a", Policy: scorer.Policy()},
		},
	})
	if err != nil {
		t.Fatalf("load fake rule config: %v", err)
	}
	return cfg
}
