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
		m := Metrics{Threshold: PRF{Precision: Rate{Value: 0.9, Defined: true}, Recall: Rate{Value: 0.85, Defined: true}}, ECE: ECE{Value: 0.05, Defined: true}}
		if v := entry.Check(m); len(v) != 0 {
			t.Fatalf("Check = %v, want no violations", v)
		}
	})
	t.Run("precision fails", func(t *testing.T) {
		m := Metrics{Threshold: PRF{Precision: Rate{Value: 0.5, Defined: true}, Recall: Rate{Value: 0.85, Defined: true}}, ECE: ECE{Value: 0.05, Defined: true}}
		v := entry.Check(m)
		if len(v) != 1 || v[0].Metric != "precision" {
			t.Fatalf("Check = %v, want exactly one precision violation", v)
		}
	})
	t.Run("ece is a ceiling not a floor", func(t *testing.T) {
		m := Metrics{Threshold: PRF{Precision: Rate{Value: 0.9, Defined: true}, Recall: Rate{Value: 0.9, Defined: true}}, ECE: ECE{Value: 0.2, Defined: true}}
		v := entry.Check(m)
		if len(v) != 1 || v[0].Metric != "ece" {
			t.Fatalf("Check = %v, want exactly one ece violation", v)
		}
	})
	t.Run("unconfigured metric is never checked", func(t *testing.T) {
		bare := FloorEntry{MinPrecision: f64(0.99)}
		m := Metrics{Threshold: PRF{Precision: Rate{Value: 0.0, Defined: true}, Recall: Rate{Value: 0.0, Defined: true}}, ECE: ECE{Value: 0.99, Defined: true}}
		v := bare.Check(m)
		if len(v) != 1 || v[0].Metric != "precision" {
			t.Fatalf("Check = %v, want only the configured precision floor checked", v)
		}
	})
	// Fix round B3: "ECE over an empty set is not 0 ... fail any floor
	// that references it" — an undefined rate/ECE against a CONFIGURED
	// floor must always be reported as a violation, never silently pass
	// just because Value happens to be the Go zero value.
	t.Run("undefined metric against a configured floor is a violation", func(t *testing.T) {
		m := Metrics{
			Threshold: PRF{Precision: Rate{Defined: false}, Recall: Rate{Value: 0.9, Defined: true}},
			ECE:       ECE{Defined: false},
		}
		v := entry.Check(m)
		var sawPrecision, sawECE bool
		for _, viol := range v {
			if viol.Metric == "precision" {
				if !viol.Undefined {
					t.Errorf("precision violation = %+v, want Undefined=true", viol)
				}
				sawPrecision = true
			}
			if viol.Metric == "ece" {
				if !viol.Undefined {
					t.Errorf("ece violation = %+v, want Undefined=true", viol)
				}
				sawECE = true
			}
		}
		if !sawPrecision || !sawECE {
			t.Fatalf("Check = %v, want both an undefined precision AND an undefined ece violation", v)
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
	cfg, brands, webmail := loadShippedRuleConfig(t)
	rule := ruleByNameT(t, cfg, "new_account_velocity")
	dataset := loadSyntheticDataset(t, brands, webmail)

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

// TestGate_NegativeWeightRegressionCaughtByTightECEFloor is fix round
// T3's own acceptance test: subject_age_h and upgrade_delay_min are both
// NEGATIVE-signed weights, so zeroing either one can only ever move
// recall/high-tier-recall UP, never down — no minimum-recall-shaped
// floor can catch either going missing. Only ECE (and, in principle,
// precision) can move the wrong way; eval/floors.yaml's max_ece is set
// with a deliberately tight margin specifically so it catches both.
func TestGate_NegativeWeightRegressionCaughtByTightECEFloor(t *testing.T) {
	cfg, brands, webmail := loadShippedRuleConfig(t)
	rule := ruleByNameT(t, cfg, "new_account_velocity")
	dataset := loadSyntheticDataset(t, brands, webmail)
	floors, err := LoadFloorsFile(repoRootJoin(t, "eval", "floors.yaml"))
	if err != nil {
		t.Fatalf("LoadFloorsFile: %v", err)
	}
	entry, ok := floors.For(rule.Name, "local", "full")
	if !ok {
		t.Fatalf("no floors entry for (%s, local, full)", rule.Name)
	}
	baselineWeights := loadShippedLocalWeights(t)

	for _, weightName := range []string{"subject_age_h", "upgrade_delay_min"} {
		t.Run(weightName, func(t *testing.T) {
			mutated := baselineWeights
			mutated.Weight = make(map[string]float64, len(baselineWeights.Weight))
			for k, v := range baselineWeights.Weight {
				mutated.Weight[k] = v
			}
			if _, ok := mutated.Weight[weightName]; !ok {
				t.Fatalf("config/local_weights.yaml has no %s weight — this test needs updating", weightName)
			}
			mutated.Weight[weightName] = 0

			scorer, err := local.New(mutated)
			if err != nil {
				t.Fatalf("local.New: %v", err)
			}
			run, err := Run(context.Background(), dataset, rule, scorer, Options{Tiers: cfg.Tiers})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			violations := entry.Check(run.Metrics)
			var sawECE bool
			for _, v := range violations {
				if v.Metric == "ece" {
					sawECE = true
				}
			}
			if !sawECE {
				t.Fatalf("zeroing %s did not violate the ece floor (got violations=%v, ece=%.4f) — the tight margin no longer catches it",
					weightName, violations, run.Metrics.ECE.Value)
			}
		})
	}
}

func repoRootJoin(t *testing.T, parts ...string) string {
	t.Helper()
	all := append([]string{repoRoot(t)}, parts...)
	return filepath.Join(all...)
}
