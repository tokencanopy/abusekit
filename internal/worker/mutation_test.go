package worker

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/tokencanopy/abusekit/internal/event"
	"github.com/tokencanopy/abusekit/internal/feature"
	"github.com/tokencanopy/abusekit/internal/model"
	"github.com/tokencanopy/abusekit/internal/model/local"
)

// mutationScenario is one committed replay fixture's feature vector, band,
// and the outcome it must reach — mirroring one of replay_test.go's
// DB-backed assertions, but computed directly via feature.Extract (with a
// canned Neighbors reproducing exactly what the real store-backed replay
// test observes) so TestWeightMutation can score it many times over
// without a database.
type mutationScenario struct {
	name     string
	features map[string]float64
	min, max float64
}

// fakeNeighborsWith returns a feature.Neighbors reporting exactly ev,
// regardless of which subject is asked about.
type fakeNeighborsWith struct{ ev feature.NeighborEvidence }

func (f fakeNeighborsWith) Evidence(context.Context, string, string) (feature.NeighborEvidence, error) {
	return f.ev, nil
}

// extractFixture loads fixtureFile, runs it through feature.Extract as of
// lastEventAt(events)+after (or, for burst's before-first-send case, only
// the events up to and including the last one before the first
// content.sent), and returns the resulting feature map. ev is the
// same-tenant linking evidence to report (feature.NeighborEvidence{} for
// every fixture except churn's).
func extractFixture(t *testing.T, brands feature.BrandSet, fixtureFile string, after time.Duration, stopBeforeContentSent bool, ev feature.NeighborEvidence) map[string]float64 {
	t.Helper()
	events := loadFixture(t, filepath.Join(repoRoot(t), "eval", "fixtures", fixtureFile))
	if stopBeforeContentSent {
		var setup []event.Event
		for _, e := range events {
			if e.Type == "content.sent" {
				break
			}
			setup = append(setup, e)
		}
		events = setup
	}
	now := lastEventAt(events).Add(after)
	res, err := feature.Extract(context.Background(), testTenant, "subject", events, fakeNeighborsWith{ev}, feature.DefaultWindows(now), brands)
	if err != nil {
		t.Fatalf("feature.Extract(%s): %v", fixtureFile, err)
	}
	return res.Features.Map()
}

// mutationScenarios returns every committed replay fixture's scenario,
// with the identical bands replay_test.go/replay_churn_test.go assert
// through the full DB-backed worker+store pipeline — see those tests for
// why each band is what it is. Kept in sync with them deliberately (not
// derived from them mechanically): this file exists specifically so a
// weight mutation can be checked against many scenarios fast, without
// needing those same assertions to also be data-driven from here.
func mutationScenarios(t *testing.T) []mutationScenario {
	t.Helper()
	brands := loadShippedBrands(t)

	churnEvents := loadFixture(t, filepath.Join(repoRoot(t), "eval", "fixtures", "churn.jsonl"))
	const eventsPerSubject = 5
	subject3Onboarding := churnEvents[2*eventsPerSubject : 2*eventsPerSubject+4]
	now3 := lastEventAt(subject3Onboarding).Add(time.Second)
	res3, err := feature.Extract(context.Background(), testTenant, "acct_example_churn_3", subject3Onboarding, fakeNeighborsWith{feature.NeighborEvidence{DeletedCount: 2, FingerprintShared: true}}, feature.DefaultWindows(now3), brands)
	if err != nil {
		t.Fatalf("feature.Extract(churn subject 3): %v", err)
	}
	subject6Onboarding := churnEvents[5*eventsPerSubject : 5*eventsPerSubject+4]
	now6 := lastEventAt(subject6Onboarding).Add(time.Second)
	res6, err := feature.Extract(context.Background(), testTenant, "acct_example_churn_6", subject6Onboarding, fakeNeighborsWith{feature.NeighborEvidence{DeletedCount: 5, FingerprintShared: true}}, feature.DefaultWindows(now6), brands)
	if err != nil {
		t.Fatalf("feature.Extract(churn subject 6): %v", err)
	}

	return []mutationScenario{
		{"reference_operator", extractFixture(t, brands, "reference_operator.jsonl", time.Minute, false, feature.NeighborEvidence{}), 0.99, 1.0},
		{"benign_transactional", extractFixture(t, brands, "benign_transactional.jsonl", time.Minute, false, feature.NeighborEvidence{}), 0.0, 0.05},
		{"burst_before_send", extractFixture(t, brands, "burst.jsonl", 15*time.Second, true, feature.NeighborEvidence{}), 0.9, 1.0},
		{"burst_final", extractFixture(t, brands, "burst.jsonl", time.Minute, false, feature.NeighborEvidence{}), 0.9, 1.0},
		{"benign_fast_onboarding", extractFixture(t, brands, "benign_fast_onboarding.jsonl", time.Minute, false, feature.NeighborEvidence{}), 0.15, 0.4},
		{"benign_integration_heavy", extractFixture(t, brands, "benign_integration_heavy.jsonl", time.Minute, false, feature.NeighborEvidence{}), 0.05, 0.35},
		{"dormant_then_blast", extractFixture(t, brands, "dormant_then_blast.jsonl", time.Minute, false, feature.NeighborEvidence{}), 0.8, 0.98},
		{"churn_subject_3", res3.Features.Map(), 0.8, 0.95},
		{"churn_subject_saturated", res6.Features.Map(), 0.9, 1.0},
	}
}

func scoreScenario(t *testing.T, scorer *local.Scorer, sc mutationScenario) float64 {
	t.Helper()
	res, err := scorer.Score(context.Background(), model.ScoreRequest{
		Labels:   []string{"benign", "suspicious", "abusive"},
		Features: sc.features,
	})
	if err != nil {
		t.Fatalf("Score(%s): %v", sc.name, err)
	}
	return 1 - res.Probs["benign"]
}

func inBand(risk float64, sc mutationScenario) bool {
	return risk >= sc.min && risk <= sc.max
}

// TestWeightMutation_EveryWeightIsLoadBearing is B1 fix round's mutation
// check: zeroing any SINGLE weight in the shipped config/local_weights.yaml
// must make at least one committed replay fixture's band assertion fail.
// A weight that survives being zeroed out — every scenario still lands in
// its expected band — isn't actually load-bearing for anything this repo
// tests, which is exactly the gap a silent weight-tuning regression could
// hide behind.
func TestWeightMutation_EveryWeightIsLoadBearing(t *testing.T) {
	weights, err := local.LoadWeightsFile(filepath.Join(repoRoot(t), "config", "local_weights.yaml"))
	if err != nil {
		t.Fatalf("LoadWeightsFile: %v", err)
	}
	scenarios := mutationScenarios(t)

	// Sanity check: the UNMUTATED weights must pass every scenario's band
	// first, or a "mutation broke it" result below would be meaningless.
	baseline, err := local.New(weights)
	if err != nil {
		t.Fatalf("local.New (baseline): %v", err)
	}
	for _, sc := range scenarios {
		risk := scoreScenario(t, baseline, sc)
		if !inBand(risk, sc) {
			t.Fatalf("baseline (unmutated) weights: %s risk = %v, want in [%v, %v] — fix the weights before trusting the mutation check", sc.name, risk, sc.min, sc.max)
		}
	}

	// The ablation vector (fullFeatureVector, deliberately scaled to sit
	// near the sigmoid's sensitive middle — see its own doc comment) is
	// folded in as one more avenue for "load bearing", alongside the
	// fixture-derived scenarios above: several weights are intentionally
	// small (resource_total/key_total are minor companions to their
	// velocity counterparts; upgrade_delay_min's magnitude was cut ~20x by
	// B5's fix so it can't swamp the model — see local_weights.yaml's own
	// comment) and never move any WIDE fixture band by enough to cross an
	// edge, even though they measurably move the score. The reviewed ask
	// was "fail at least one replay/ablation test" — an OR, not just the
	// fixture bands alone.
	ablationBaseline := fullFeatureVector()
	ablationBaselineRisk := scoreFeatures(t, baseline, ablationBaseline)
	const ablationEpsilon = 1e-4

	for name, original := range weights.Weight {
		if original == 0 {
			continue // already zero; "mutating" it to zero is a no-op, not a meaningful check.
		}
		mutated := weights
		mutated.Weight = make(map[string]float64, len(weights.Weight))
		for k, v := range weights.Weight {
			mutated.Weight[k] = v
		}
		mutated.Weight[name] = 0

		scorer, err := local.New(mutated)
		if err != nil {
			t.Fatalf("local.New (mutated %s): %v", name, err)
		}

		brokeSomething := false
		for _, sc := range scenarios {
			if _, ok := sc.features[name]; !ok || sc.features[name] == 0 {
				continue // this scenario doesn't exercise this feature at all; zeroing its weight can't change it.
			}
			risk := scoreScenario(t, scorer, sc)
			if !inBand(risk, sc) {
				brokeSomething = true
				break
			}
		}
		if !brokeSomething {
			if v, ok := ablationBaseline[name]; ok && v != 0 {
				mutatedRisk := scoreFeatures(t, scorer, ablationBaseline)
				if math.Abs(mutatedRisk-ablationBaselineRisk) > ablationEpsilon {
					brokeSomething = true
				}
			}
		}
		if !brokeSomething {
			t.Errorf("zeroing weight %q (was %v) neither pushed any replay scenario out of its band nor moved the ablation vector's score by more than %v", name, original, ablationEpsilon)
		}
	}
}
