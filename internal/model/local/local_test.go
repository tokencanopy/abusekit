package local

import (
	"context"
	"math"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/tokencanopy/abusekit/internal/model"
)

func TestWeights_Validate(t *testing.T) {
	tests := []struct {
		name    string
		w       Weights
		wantErr bool
	}{
		{"valid", Weights{BenignLabel: "benign", Weight: map[string]float64{"x": 1}}, false},
		{"missing benign label", Weights{Weight: map[string]float64{"x": 1}}, true},
		{"missing weights", Weights{BenignLabel: "benign"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.w.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestNew_RejectsInvalidWeights(t *testing.T) {
	if _, err := New(Weights{}); err == nil {
		t.Fatalf("expected New to reject empty Weights")
	}
}

func TestScore_HigherRiskFeaturesIncreaseRisk(t *testing.T) {
	w := Weights{
		BenignLabel: "benign",
		Bias:        -2,
		Weight:      map[string]float64{"resource_velocity_1h": 1.0},
	}
	s, err := New(w)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	low, err := s.Score(context.Background(), model.ScoreRequest{
		Labels:   []string{"benign", "abusive"},
		Features: map[string]float64{"resource_velocity_1h": 0},
	})
	if err != nil {
		t.Fatalf("Score(low): %v", err)
	}
	high, err := s.Score(context.Background(), model.ScoreRequest{
		Labels:   []string{"benign", "abusive"},
		Features: map[string]float64{"resource_velocity_1h": 10},
	})
	if err != nil {
		t.Fatalf("Score(high): %v", err)
	}

	riskLow := 1 - low.Probs["benign"]
	riskHigh := 1 - high.Probs["benign"]
	if riskHigh <= riskLow {
		t.Fatalf("expected higher resource_velocity_1h to raise risk: low=%v high=%v", riskLow, riskHigh)
	}
}

func TestScore_MissingFeatureContributesZero(t *testing.T) {
	w := Weights{
		BenignLabel: "benign",
		Bias:        0,
		Weight:      map[string]float64{"unset_feature": 5}, // never present in Features
	}
	s, err := New(w)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := s.Score(context.Background(), model.ScoreRequest{
		Labels: []string{"benign", "abusive"},
	})
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	// bias=0 and the only weight's feature is absent (contributes 0) -> linear=0 -> p(benign)=0.5.
	if math.Abs(res.Probs["benign"]-0.5) > 1e-9 {
		t.Fatalf("expected p(benign)=0.5 with linear=0, got %v", res.Probs["benign"])
	}
}

func TestScore_NonBenignMassSplitEvenly(t *testing.T) {
	w := Weights{BenignLabel: "benign", Bias: 5, Weight: map[string]float64{"x": 0}}
	s, err := New(w)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := s.Score(context.Background(), model.ScoreRequest{
		Labels: []string{"benign", "suspicious", "abusive"},
	})
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if math.Abs(res.Probs["suspicious"]-res.Probs["abusive"]) > 1e-9 {
		t.Fatalf("expected non-benign mass split evenly, got suspicious=%v abusive=%v",
			res.Probs["suspicious"], res.Probs["abusive"])
	}
}

func TestScore_RejectsTextInput(t *testing.T) {
	s, err := New(Weights{BenignLabel: "benign", Weight: map[string]float64{"x": 1}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = s.Score(context.Background(), model.ScoreRequest{
		Labels: []string{"benign", "abusive"},
		Text:   []string{"hello"},
	})
	if err == nil {
		t.Fatalf("expected an error when Text is supplied to the local scorer")
	}
}

// TestLoadWeightsFile_ShippedConfig loads the real config/local_weights.yaml
// shipped in this repo and checks it parses into a usable Scorer covering
// every v0 feature named in design §4.5's new_account_velocity rule. This
// is the test that would fail if someone edited the YAML into something
// New can't build from.
func TestLoadWeightsFile_ShippedConfig(t *testing.T) {
	w, err := LoadWeightsFile(repoConfigPath(t, "local_weights.yaml"))
	if err != nil {
		t.Fatalf("LoadWeightsFile: %v", err)
	}
	s, err := New(w)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	wantFeatures := []string{
		"subject_age_h", "resource_velocity_1h", "resource_total", "key_velocity_1h", "key_total",
		"upgrade_delay_min", "declines_before_first_success", "first_funding_prepaid",
		"name_brand_match", "name_has_at", "first_day_distinct_domains", "self_send_before_external",
		"linked_deleted_n", "linked_labelled_abusive_n", "fingerprint_seen_on_other_subjects",
		"burst_ratio_24h_vs_lifetime",
	}
	for _, f := range wantFeatures {
		if _, ok := w.Weight[f]; !ok {
			t.Errorf("shipped weights missing design §4.5 feature %q", f)
		}
	}

	if _, err := s.Score(context.Background(), model.ScoreRequest{
		Labels:   []string{"benign", "suspicious", "abusive"},
		Features: map[string]float64{"resource_velocity_1h": 6, "key_velocity_1h": 4},
	}); err != nil {
		t.Fatalf("Score with shipped weights: %v", err)
	}
}

// repoConfigPath resolves config/<name> relative to the module root,
// independent of `go test`'s working directory (which is the package
// dir).
func repoConfigPath(t *testing.T, name string) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller failed")
	}
	// this file: <root>/internal/model/local/local_test.go
	root := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
	return filepath.Join(root, "config", name)
}
