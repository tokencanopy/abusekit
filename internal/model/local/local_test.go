package local

import (
	"context"
	"fmt"
	"math"
	"os"
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
		{"valid", Weights{BenignLabel: "benign", Weight: map[string]float64{"custom.x": 1}}, false},
		{"missing benign label", Weights{Weight: map[string]float64{"custom.x": 1}}, true},
		{"missing weights", Weights{BenignLabel: "benign"}, true},
		// R5 (round 2): a NaN/Inf bias or weight makes the scorer's every
		// answer NaN forever (NaN propagates through the linear sum and
		// sigmoid) without ever failing anywhere — a "dark rule" that
		// looks registered and healthy but can never usefully score.
		// Reject it at load time instead.
		{"NaN bias", Weights{BenignLabel: "benign", Bias: math.NaN(), Weight: map[string]float64{"custom.x": 1}}, true},
		{"Inf bias", Weights{BenignLabel: "benign", Bias: math.Inf(1), Weight: map[string]float64{"custom.x": 1}}, true},
		{"NaN weight", Weights{BenignLabel: "benign", Weight: map[string]float64{"custom.x": math.NaN()}}, true},
		{"Inf weight", Weights{BenignLabel: "benign", Weight: map[string]float64{"custom.x": math.Inf(-1)}}, true},
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
		Weight:      map[string]float64{"core.resource_velocity_1h": 1.0},
	}
	s, err := New(w)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	low, err := s.Score(context.Background(), model.ScoreRequest{
		Labels:   []string{"benign", "abusive"},
		Features: map[string]float64{"core.resource_velocity_1h": 0},
	})
	if err != nil {
		t.Fatalf("Score(low): %v", err)
	}
	high, err := s.Score(context.Background(), model.ScoreRequest{
		Labels:   []string{"benign", "abusive"},
		Features: map[string]float64{"core.resource_velocity_1h": 10},
	})
	if err != nil {
		t.Fatalf("Score(high): %v", err)
	}

	riskLow := 1 - low.Probs["benign"]
	riskHigh := 1 - high.Probs["benign"]
	if riskHigh <= riskLow {
		t.Fatalf("expected higher core.resource_velocity_1h to raise risk: low=%v high=%v", riskLow, riskHigh)
	}
}

func TestScore_MissingFeatureContributesZero(t *testing.T) {
	w := Weights{
		BenignLabel: "benign",
		Bias:        0,
		Weight:      map[string]float64{"custom.unset_feature": 5}, // never present in Features
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
	w := Weights{BenignLabel: "benign", Bias: 5, Weight: map[string]float64{"custom.x": 0}}
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

// TestVersion_ChangesWithWeightsContent is S2: Version() must be derived
// from the weights' actual content (a hash), not just the human-typed
// `version:` string — so editing a weight VALUE without remembering to
// bump `version:` in local_weights.yaml still changes Version(), which
// internal/core's input-hash uses to force a rescore after any scorer
// change (a Weights.Version string alone can't catch a forgotten bump).
func TestVersion_ChangesWithWeightsContent(t *testing.T) {
	base := Weights{Version: "v1", BenignLabel: "benign", Bias: -2, Weight: map[string]float64{"custom.x": 1.0}}
	edited := Weights{Version: "v1", BenignLabel: "benign", Bias: -2, Weight: map[string]float64{"custom.x": 1.5}} // same version string, different weight

	sBase, err := New(base)
	if err != nil {
		t.Fatalf("New(base): %v", err)
	}
	sEdited, err := New(edited)
	if err != nil {
		t.Fatalf("New(edited): %v", err)
	}

	if sBase.Version() == sEdited.Version() {
		t.Fatalf("expected Version() to differ when weight content differs, even with an identical `version:` string")
	}
	if sBase.Version() == "" {
		t.Fatalf("expected a non-empty Version()")
	}
}

func TestVersion_DeterministicForIdenticalWeights(t *testing.T) {
	w := Weights{Version: "v1", BenignLabel: "benign", Bias: -2, Weight: map[string]float64{"custom.x": 1.0, "custom.y": 2.0}}
	s1, err := New(w)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s2, err := New(w)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s1.Version() != s2.Version() {
		t.Fatalf("expected Version() to be deterministic for identical weights: %q != %q", s1.Version(), s2.Version())
	}
}

// TestScore_DeterministicAcrossManyRandomFeatureMaps is S10. Score summed
// weight*feature by ranging over the weights map directly, whose
// iteration order Go deliberately randomizes on every `range` statement —
// even across separate calls over the exact same map object.
// Floating-point addition is not associative: summing (+1e16, -1e16, +1)
// as (+1e16 + -1e16) + 1 = 1, but as (+1e16 + 1) + -1e16 = 0 (the "+1"
// gets rounded away while the running total is ~1e16, then the big terms
// cancel) — so an unfixed summation order can silently produce a
// different risk for the identical feature vector, call to call. 20
// independent adversarial triples (each engineered exactly this way, with
// distinct feature names so their map bucket placement differs) scored 3
// times each gives many chances for Go's randomized map order to expose
// this if Score doesn't force a deterministic (sorted-key) summation
// order.
func TestScore_DeterministicAcrossManyRandomFeatureMaps(t *testing.T) {
	const nTriples = 20
	weight := make(map[string]float64, nTriples*3)
	features := make(map[string]float64, nTriples*3)
	for i := 0; i < nTriples; i++ {
		big, negBig, small := fmt.Sprintf("custom.big%02d", i), fmt.Sprintf("custom.negbig%02d", i), fmt.Sprintf("custom.small%02d", i)
		weight[big], weight[negBig], weight[small] = 1e16, -1e16, 1
		features[big], features[negBig], features[small] = 1, 1, 1
	}
	s, err := New(Weights{BenignLabel: "benign", Bias: 0, Weight: weight})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	req := model.ScoreRequest{Labels: []string{"benign", "abusive"}, Features: features}

	var first float64
	for c := 0; c < 3; c++ {
		res, err := s.Score(context.Background(), req)
		if err != nil {
			t.Fatalf("Score (call %d): %v", c, err)
		}
		if c == 0 {
			first = res.Probs["benign"]
			continue
		}
		if res.Probs["benign"] != first {
			t.Fatalf("non-deterministic Score for an identical feature map: call 0 -> %v, call %d -> %v", first, c, res.Probs["benign"])
		}
	}
}

func TestScore_RejectsTextInput(t *testing.T) {
	s, err := New(Weights{BenignLabel: "benign", Weight: map[string]float64{"custom.x": 1}})
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
		"core.subject_age_h", "core.resource_velocity_1h", "core.resource_total", "core.credential_velocity_1h", "core.credential_total",
		"core.upgrade_delay_min", "core.declines_before_first_success", "core.first_funding_prepaid",
		"brand.name_match", "brand.name_has_at", "email.first_day_distinct_domains", "email.self_send_before_external",
		"core.linked_deleted_n", "core.linked_labelled_abusive_n", "core.fingerprint_seen_on_other_subjects",
		"core.burst_ratio_24h_vs_lifetime",
	}
	for _, f := range wantFeatures {
		if _, ok := w.Weight[f]; !ok {
			t.Errorf("shipped weights missing design §4.5 feature %q", f)
		}
	}

	if _, err := s.Score(context.Background(), model.ScoreRequest{
		Labels:   []string{"benign", "suspicious", "abusive"},
		Features: map[string]float64{"core.resource_velocity_1h": 6, "core.credential_velocity_1h": 4},
	}); err != nil {
		t.Fatalf("Score with shipped weights: %v", err)
	}
}

// TestLoadWeightsFile_RejectsUnknownTopLevelField is R5 (round 2): strict
// decoding (KnownFields) so a typo'd or stale top-level key
// (e.g. "bais" instead of "bias") fails the load instead of silently
// leaving that field at its zero value.
func TestLoadWeightsFile_RejectsUnknownTopLevelField(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "weights.yaml")
	if err := os.WriteFile(path, []byte("version: v1\nbenign_label: benign\nbais: -2\nweights: {x: 1}\n"), 0o644); err != nil {
		t.Fatalf("write weights file: %v", err)
	}
	if _, err := LoadWeightsFile(path); err == nil {
		t.Fatalf("expected LoadWeightsFile to reject an unknown top-level field")
	}
}

// TestLoadWeightsFile_RejectsNaN is R5: LoadWeightsFile's own load path
// (not just a directly-constructed Weights{}) must reject a NaN bias.
func TestLoadWeightsFile_RejectsNaN(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "weights.yaml")
	if err := os.WriteFile(path, []byte("version: v1\nbenign_label: benign\nbias: .nan\nweights: {x: 1}\n"), 0o644); err != nil {
		t.Fatalf("write weights file: %v", err)
	}
	if _, err := LoadWeightsFile(path); err == nil {
		t.Fatalf("expected LoadWeightsFile to reject a NaN bias")
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
