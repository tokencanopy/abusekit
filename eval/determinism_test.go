package eval

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// TestRun_Deterministic proves two Run calls over the identical
// Dataset/Rule/Scorer/Options produce byte-identical run.json apart from
// Manifest.At (task brief: "determinism (same input produces a
// byte-identical run.json apart from at)"). Uses the real shipped
// config and the committed synthetic corpus — the same combination
// `make gate` runs — rather than a synthetic stand-in, so this test
// would also catch a genuine source of nondeterminism the gate itself
// depends on (unsorted map iteration, Go's randomized map order, ...).
func TestRun_Deterministic(t *testing.T) {
	cfg, brands := loadShippedRuleConfig(t)
	rule := ruleByNameT(t, cfg, "new_account_velocity")
	scorer, _ := cfg.ScorerFor(rule)
	dataset := loadSyntheticDataset(t, brands)

	fixedNow := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	opts := Options{Tiers: cfg.Tiers, Now: func() time.Time { return fixedNow }}

	run1, err := Run(context.Background(), dataset, rule, scorer, opts)
	if err != nil {
		t.Fatalf("Run (1): %v", err)
	}
	run2, err := Run(context.Background(), dataset, rule, scorer, opts)
	if err != nil {
		t.Fatalf("Run (2): %v", err)
	}

	if !run1.Manifest.At.Equal(run2.Manifest.At) {
		t.Fatalf("At differs across two Run calls with the SAME opts.Now: %v vs %v", run1.Manifest.At, run2.Manifest.At)
	}

	b1, err := json.Marshal(run1)
	if err != nil {
		t.Fatalf("marshal run1: %v", err)
	}
	b2, err := json.Marshal(run2)
	if err != nil {
		t.Fatalf("marshal run2: %v", err)
	}
	if string(b1) != string(b2) {
		t.Fatalf("two Run calls over identical inputs produced different run.json")
	}

	// Now vary ONLY opts.Now (a later wall-clock instant) and confirm
	// every OTHER field still matches — i.e. `at` really is the only
	// field allowed to differ.
	laterNow := fixedNow.Add(time.Hour)
	run3, err := Run(context.Background(), dataset, rule, scorer, Options{Tiers: cfg.Tiers, Now: func() time.Time { return laterNow }})
	if err != nil {
		t.Fatalf("Run (3): %v", err)
	}
	if run3.Manifest.At.Equal(run1.Manifest.At) {
		t.Fatalf("run3 used a different Now but Manifest.At didn't change")
	}
	run1NoAt, run3NoAt := run1, run3
	run1NoAt.Manifest.At, run3NoAt.Manifest.At = time.Time{}, time.Time{}
	b1NoAt, _ := json.Marshal(run1NoAt)
	b3NoAt, _ := json.Marshal(run3NoAt)
	if string(b1NoAt) != string(b3NoAt) {
		t.Fatalf("run.json differs by more than `at` between two runs that only varied opts.Now")
	}
}
