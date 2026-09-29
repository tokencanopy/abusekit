package eval

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/tokencanopy/abusekit/internal/config"
)

// TestRun_AbortsOnCassetteMiss is fix round B3's own Run-level acceptance
// test (review round 2's T1): a replay-mode CassetteScorer with an empty
// cassette, driven through Run (not CassetteScorer.Score directly — see
// cassette_test.go's TestCassetteScorer_ReplayMissIsLoud for that unit
// test), must abort the whole Run with an error wrapping ErrCassetteMiss
// — never silently fold the miss into an "unscored" Verdict for that one
// subject while returning a nil error overall.
func TestRun_AbortsOnCassetteMiss(t *testing.T) {
	dataset := Dataset{Subjects: []Subject{
		{ID: "acct_1", Label: "benign", Points: map[Slice]Point{SliceFull: {Features: map[string]float64{"x": 1}}}},
	}}
	rule := config.Rule{
		Name: "r", Mode: config.ModeAdvise, Scorer: "vendor",
		Inputs: []string{"x"}, Labels: []string{"benign", "abusive"}, BenignLabel: "benign", Threshold: 0.5,
	}

	inner := &stubScorer{name: "vendor"}
	cassette, err := LoadCassette(filepath.Join(t.TempDir(), "empty.json"))
	if err != nil {
		t.Fatalf("LoadCassette: %v", err)
	}
	cs := &CassetteScorer{Inner: inner, Cassette: cassette, Mode: CassetteReplay, PromptVersion: "v1"}

	_, err = Run(context.Background(), dataset, rule, cs, Options{})
	if err == nil {
		t.Fatalf("Run returned a nil error on a cassette miss")
	}
	if !errors.Is(err, ErrCassetteMiss) {
		t.Fatalf("Run's error %v does not wrap ErrCassetteMiss", err)
	}
	if inner.calls != 0 {
		t.Fatalf("inner.calls = %d, want 0 — a replay-mode miss must never reach the live scorer", inner.calls)
	}
}
