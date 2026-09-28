package eval

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/tokencanopy/abusekit/internal/model/local"
)

func f64(v float64) *float64 { return &v }

func TestFloorEntry_Check(t *testing.T) {
	entry := FloorEntry{MinPrecision: f64(0.8), MinRecall: f64(0.8), MaxECE: f64(0.1)}

	t.Run("all pass", func(t *testing.T) {
		m := Metrics{Threshold: PRF{Precision: Rate{Value: 0.9}, Recall: Rate{Value: 0.85}}, ECE: ECE{Value: 0.05}}
		if v := entry.Check(m); len(v) != 0 {
			t.Fatalf("Check = %v, want no violations", v)
		}
	})
	t.Run("precision fails", func(t *testing.T) {
		m := Metrics{Threshold: PRF{Precision: Rate{Value: 0.5}, Recall: Rate{Value: 0.85}}, ECE: ECE{Value: 0.05}}
		v := entry.Check(m)
		if len(v) != 1 || v[0].Metric != "precision" {
			t.Fatalf("Check = %v, want exactly one precision violation", v)
		}
	})
	t.Run("ece is a ceiling not a floor", func(t *testing.T) {
		m := Metrics{Threshold: PRF{Precision: Rate{Value: 0.9}, Recall: Rate{Value: 0.9}}, ECE: ECE{Value: 0.2}}
		v := entry.Check(m)
		if len(v) != 1 || v[0].Metric != "ece" {
			t.Fatalf("Check = %v, want exactly one ece violation", v)
		}
	})
	t.Run("unconfigured metric is never checked", func(t *testing.T) {
		bare := FloorEntry{MinPrecision: f64(0.99)}
		m := Metrics{Threshold: PRF{Precision: Rate{Value: 0.0}, Recall: Rate{Value: 0.0}}, ECE: ECE{Value: 0.99}}
		v := bare.Check(m)
		if len(v) != 1 || v[0].Metric != "precision" {
			t.Fatalf("Check = %v, want only the configured precision floor checked", v)
		}
	})
}

func TestFloors_For(t *testing.T) {
	floors := Floors{Entries: []FloorEntry{
		{Rule: "r1", Scorer: "local", Slice: "full", MinPrecision: f64(0.5)},
	}}
	if _, ok := floors.For("r1", "local", "full"); !ok {
		t.Fatalf("For(r1, local, full) not found")
	}
	if _, ok := floors.For("r1", "local", "early_15m"); ok {
		t.Fatalf("For(r1, local, early_15m) unexpectedly found")
	}
}

// TestGate_WeightRegressionFailsFloors is the task brief's own
// acceptance test: "a gate test in which lowering a weight makes make
// gate fail." Runs eval.Run twice against the real committed synthetic
// corpus (the same one `make gate` scores) — once with the shipped
// config/local_weights.yaml (must clear every floor in the shipped
// eval/floors.yaml) and once with name_brand_match zeroed out in an
// otherwise-identical copy of those weights (must violate at least one
// floor). This is a Go-level equivalent of `make gate`'s own shell
// command, not a re-implementation of it — see Makefile's gate target
// for the actual CLI invocation this mirrors.
func TestGate_WeightRegressionFailsFloors(t *testing.T) {
	cfg, brands := loadShippedRuleConfig(t)
	rule := ruleByNameT(t, cfg, "new_account_velocity")
	dataset := loadSyntheticDataset(t, brands)

	floors, err := LoadFloorsFile(repoRootJoin(t, "eval", "floors.yaml"))
	if err != nil {
		t.Fatalf("LoadFloorsFile: %v", err)
	}
	entry, ok := floors.For(rule.Name, "local", "full")
	if !ok {
		t.Fatalf("no floors entry for (%s, local, full) — eval/floors.yaml and this test have drifted", rule.Name)
	}

	baselineWeights := loadShippedLocalWeights(t)
	baselineScorer, err := local.New(baselineWeights)
	if err != nil {
		t.Fatalf("local.New(baseline): %v", err)
	}
	baselineRun, err := Run(context.Background(), dataset, rule, baselineScorer, Options{Tiers: cfg.Tiers})
	if err != nil {
		t.Fatalf("Run(baseline): %v", err)
	}
	if v := entry.Check(baselineRun.Metrics); len(v) != 0 {
		t.Fatalf("shipped weights already violate eval/floors.yaml: %v (metrics=%+v) — floors and weights have drifted", v, baselineRun.Metrics.Threshold)
	}

	mutatedWeights := baselineWeights
	mutatedWeights.Weight = make(map[string]float64, len(baselineWeights.Weight))
	for k, v := range baselineWeights.Weight {
		mutatedWeights.Weight[k] = v
	}
	if _, ok := mutatedWeights.Weight["name_brand_match"]; !ok {
		t.Fatalf("config/local_weights.yaml has no name_brand_match weight to mutate — this test needs updating")
	}
	mutatedWeights.Weight["name_brand_match"] = 0 // the deliberate regression

	mutatedScorer, err := local.New(mutatedWeights)
	if err != nil {
		t.Fatalf("local.New(mutated): %v", err)
	}
	mutatedRun, err := Run(context.Background(), dataset, rule, mutatedScorer, Options{Tiers: cfg.Tiers})
	if err != nil {
		t.Fatalf("Run(mutated): %v", err)
	}
	violations := entry.Check(mutatedRun.Metrics)
	if len(violations) == 0 {
		t.Fatalf("zeroing name_brand_match did not violate any floor (baseline=%+v, mutated=%+v) — either the corpus doesn't exercise this weight, or the floors have too much slack",
			baselineRun.Metrics.Threshold, mutatedRun.Metrics.Threshold)
	}
	t.Logf("mutated-weight run correctly failed the gate: %v", violations)
}

func repoRootJoin(t *testing.T, parts ...string) string {
	t.Helper()
	all := append([]string{repoRoot(t)}, parts...)
	return filepath.Join(all...)
}
