package worker

import (
	"context"
	"math"
	"path/filepath"
	"testing"

	"github.com/tokencanopy/abusekit/internal/model"
	"github.com/tokencanopy/abusekit/internal/model/local"
)

// fullFeatureVector is a synthetic feature vector with every v0 feature
// set to a representative non-zero value — used by
// TestAblation_EveryWeightedFeatureMovesTheScore to check each feature's
// OWN sensitivity independent of whether any one committed replay fixture
// happens to exercise it.
//
// Deliberately scaled so the BASELINE risk lands near the sensitive middle
// of the sigmoid (roughly 0.3-0.6), not near 0 or 1: sigmoid's derivative
// vanishes in saturation, so a baseline vector where every feature fires
// at once (all fairly large) would make zeroing any ONE of them barely
// move the final probability even though its weighted contribution to the
// underlying linear score is exactly the same size either way — the first
// version of this test tried exactly that and saturated at risk >0.9999
// for every case, making the whole check vacuous.
func fullFeatureVector() map[string]float64 {
	return map[string]float64{
		"subject_age_h":                      0.5,
		"resource_velocity_1h":               0.75,
		"resource_total":                     1.25,
		"key_velocity_1h":                    0.5,
		"key_total":                          1,
		"upgrade_delay_min":                  2.5,
		"upgraded":                           0.25,
		"declines_before_first_success":      0.5,
		"first_funding_prepaid":              0.25,
		"name_brand_match":                   0.25,
		"name_has_at":                        0.25,
		"first_day_distinct_domains":         0.75,
		"self_send_before_external":          0.5,
		"linked_deleted_n":                   0.5,
		"linked_labelled_abusive_n":          0.25,
		"fingerprint_seen_on_other_subjects": 0.25,
		"neighbors_truncated":                0.25,
		"burst_ratio_24h_vs_lifetime":        0.2,
	}
}

func scoreFeatures(t *testing.T, scorer *local.Scorer, features map[string]float64) float64 {
	t.Helper()
	res, err := scorer.Score(context.Background(), model.ScoreRequest{
		Labels:   []string{"benign", "suspicious", "abusive"},
		Features: features,
	})
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	return 1 - res.Probs["benign"]
}

// TestAblation_EveryWeightedFeatureMovesTheScore is B1 fix round's
// per-feature ablation check: starting from a representative feature
// vector with every v0 feature non-zero, zeroing OUT any single feature
// that has a non-zero weight in the shipped config/local_weights.yaml
// must change the resulting risk by more than a small epsilon — proving
// every weighted feature actually participates in scoring, not just that
// it's present in the YAML.
func TestAblation_EveryWeightedFeatureMovesTheScore(t *testing.T) {
	weights, err := local.LoadWeightsFile(filepath.Join(repoRoot(t), "config", "local_weights.yaml"))
	if err != nil {
		t.Fatalf("LoadWeightsFile: %v", err)
	}
	scorer, err := local.New(weights)
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}

	baseline := fullFeatureVector()
	baselineRisk := scoreFeatures(t, scorer, baseline)

	const epsilon = 1e-4
	for name, weight := range weights.Weight {
		if weight == 0 {
			continue // a zero weight in the YAML can't move anything by construction; nothing to prove.
		}
		if _, ok := baseline[name]; !ok {
			t.Errorf("config/local_weights.yaml has a weight for %q, which fullFeatureVector doesn't set — add it so ablation can cover it", name)
			continue
		}
		variant := make(map[string]float64, len(baseline))
		for k, v := range baseline {
			variant[k] = v
		}
		variant[name] = 0

		risk := scoreFeatures(t, scorer, variant)
		if math.Abs(risk-baselineRisk) <= epsilon {
			t.Errorf("zeroing %q (weight %v) barely moved the score: %v -> %v (want a difference > %v)", name, weight, baselineRisk, risk, epsilon)
		}
	}
}
