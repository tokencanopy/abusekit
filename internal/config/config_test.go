package config_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/tokencanopy/abusekit/internal/config"
	"github.com/tokencanopy/abusekit/internal/model"
	"github.com/tokencanopy/abusekit/internal/model/fake"
	"github.com/tokencanopy/abusekit/internal/model/local"
)

// newFixture builds a registry with a "local"-like fake open-mode scorer
// (calibrated, accepts features, not text) and a "jev"-like fake scorer
// (calibrated, accepts text, different label set) so tests can exercise
// every validation path without needing internal/model/local.
func newFixture(t *testing.T) (deps config.Dependencies, reg *model.Registry) {
	t.Helper()
	reg = model.NewRegistry()

	localLike := fake.New()
	localLike.NameValue = "local"
	localLike.Caps = model.Capabilities{
		LabelMode:       model.OpenLabelMode(),
		AcceptsText:     false,
		AcceptsFeatures: true,
		Calibrated:      true,
	}
	if err := reg.Register(localLike); err != nil {
		t.Fatal(err)
	}

	textVendor := fake.New()
	textVendor.NameValue = "textvendor"
	textVendor.Caps = model.Capabilities{
		LabelMode:       model.FixedLabelMode("benign", "phishing"),
		AcceptsText:     true,
		AcceptsFeatures: false,
		Calibrated:      false, // requires a recorded calibration
	}
	if err := reg.Register(textVendor); err != nil {
		t.Fatal(err)
	}

	otherFixed := fake.New()
	otherFixed.NameValue = "otherfixed"
	otherFixed.Caps = model.Capabilities{
		LabelMode:       model.FixedLabelMode("benign", "scam"), // differs from textVendor's set
		AcceptsText:     true,
		AcceptsFeatures: false,
		Calibrated:      true,
	}
	if err := reg.Register(otherFixed); err != nil {
		t.Fatal(err)
	}

	deps = config.Dependencies{
		Registry: reg,
		Features: config.NewFeatureSet("subject_age_h", "resource_velocity_1h"),
		Vendors: map[string]config.VendorEntry{
			"local":      {Name: "local", Policy: model.DataPolicy{AllowsText: false}},
			"textvendor": {Name: "textvendor", Policy: model.DataPolicy{AllowsText: true}},
			// "otherfixed" intentionally NOT listed, to test the allowlist check.
		},
	}
	return deps, reg
}

const validYAML = `
tiers: {medium: 0.4, high: 0.8}
min_scored_advise: 1
rules:
  - name: new_account_velocity
    mode: advise
    scorer: local
    inputs: [subject_age_h, resource_velocity_1h]
    labels: [benign, suspicious, abusive]
    benign_label: benign
    threshold: 0.6
`

func TestLoad_Valid(t *testing.T) {
	deps, _ := newFixture(t)
	cfg, err := config.Load([]byte(validYAML), deps)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Rules) != 1 || cfg.Rules[0].Name != "new_account_velocity" {
		t.Fatalf("unexpected rules: %#v", cfg.Rules)
	}
	if cfg.Tiers.Medium != 0.4 || cfg.Tiers.High != 0.8 {
		t.Fatalf("unexpected tiers: %#v", cfg.Tiers)
	}
	if _, ok := cfg.ScorerFor(cfg.Rules[0]); !ok {
		t.Fatalf("expected ScorerFor to resolve the rule's scorer")
	}
}

func TestLoad_MinScoredAdviseDefaultsToOne(t *testing.T) {
	deps, _ := newFixture(t)
	yaml := strings.Replace(validYAML, "min_scored_advise: 1\n", "", 1)
	cfg, err := config.Load([]byte(yaml), deps)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MinScoredAdvise != 1 {
		t.Fatalf("expected default min_scored_advise=1, got %d", cfg.MinScoredAdvise)
	}
}

func TestLoad_RejectsWholeConfigOnAnyError(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "unknown scorer",
			yaml: `
tiers: {medium: 0.4, high: 0.8}
rules:
  - name: r1
    mode: advise
    scorer: does_not_exist
    inputs: [subject_age_h]
    labels: [benign, abusive]
    benign_label: benign
    threshold: 0.5
`,
			wantErr: "unknown scorer",
		},
		{
			name: "unknown feature",
			yaml: `
tiers: {medium: 0.4, high: 0.8}
rules:
  - name: r1
    mode: advise
    scorer: local
    inputs: [totally_made_up_feature]
    labels: [benign, abusive]
    benign_label: benign
    threshold: 0.5
`,
			wantErr: "unknown feature",
		},
		{
			name: "labels not accepted by fixed-mode adapter",
			yaml: `
tiers: {medium: 0.4, high: 0.8}
rules:
  - name: r1
    mode: shadow
    scorer: textvendor
    text: [subject_line_skeleton]
    labels: [benign, some_other_label]
    benign_label: benign
    threshold: 0.5
`,
			wantErr: "not accepted by scorer",
		},
		{
			name: "text sent to an adapter that does not accept text",
			yaml: `
tiers: {medium: 0.4, high: 0.8}
rules:
  - name: r1
    mode: advise
    scorer: local
    text: [subject_line_skeleton]
    labels: [benign, abusive]
    benign_label: benign
    threshold: 0.5
`,
			wantErr: "does not accept text",
		},
		{
			name: "missing calibration for uncalibrated scorer",
			yaml: `
tiers: {medium: 0.4, high: 0.8}
rules:
  - name: r1
    mode: shadow
    scorer: textvendor
    text: [subject_line_skeleton]
    labels: [benign, phishing]
    benign_label: benign
    threshold: 0.5
`,
			wantErr: "no recorded calibration",
		},
		{
			name: "benign_label not in labels",
			yaml: `
tiers: {medium: 0.4, high: 0.8}
rules:
  - name: r1
    mode: advise
    scorer: local
    inputs: [subject_age_h]
    labels: [suspicious, abusive]
    benign_label: benign
    threshold: 0.5
`,
			wantErr: "not in its labels",
		},
		{
			name: "unrecognized stage key",
			yaml: `
tiers: {medium: 0.4, high: 0.8}
rules:
  - name: r1
    mode: advise
    scorer: local
    inputs: [subject_age_h]
    labels: [benign, abusive]
    benign_label: benign
    threshold: 0.5
    stage: {made_up_condition: 1}
`,
			wantErr: "unknown stage condition",
		},
		{
			name: "bad tiers",
			yaml: `
tiers: {medium: 0.8, high: 0.4}
rules:
  - name: r1
    mode: advise
    scorer: local
    inputs: [subject_age_h]
    labels: [benign, abusive]
    benign_label: benign
    threshold: 0.5
`,
			wantErr: "must be greater than",
		},
		{
			name: "vendor allowlist",
			yaml: `
tiers: {medium: 0.4, high: 0.8}
rules:
  - name: r1
    mode: shadow
    scorer: otherfixed
    text: [subject_line_skeleton]
    labels: [benign, scam]
    benign_label: benign
    threshold: 0.5
`,
			wantErr: "not in the vendor allowlist",
		},
		{
			name: "same_as referencing unknown rule",
			yaml: `
tiers: {medium: 0.4, high: 0.8}
rules:
  - name: r1
    mode: advise
    scorer: local
    inputs: {same_as: does_not_exist}
    labels: [benign, abusive]
    benign_label: benign
    threshold: 0.5
`,
			wantErr: "unknown rule",
		},
		{
			name: "duplicate rule name",
			yaml: `
tiers: {medium: 0.4, high: 0.8}
rules:
  - name: r1
    mode: advise
    scorer: local
    inputs: [subject_age_h]
    labels: [benign, abusive]
    benign_label: benign
    threshold: 0.5
  - name: r1
    mode: advise
    scorer: local
    inputs: [subject_age_h]
    labels: [benign, abusive]
    benign_label: benign
    threshold: 0.5
`,
			wantErr: "duplicate rule name",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			deps, _ := newFixture(t)
			_, err := config.Load([]byte(tc.yaml), deps)
			if err == nil {
				t.Fatalf("expected an error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got: %v", tc.wantErr, err)
			}
		})
	}
}

func TestLoad_SameAsResolvesInputs(t *testing.T) {
	deps, _ := newFixture(t)
	yaml := `
tiers: {medium: 0.4, high: 0.8}
rules:
  - name: base
    mode: advise
    scorer: local
    inputs: [subject_age_h, resource_velocity_1h]
    labels: [benign, abusive]
    benign_label: benign
    threshold: 0.5
  - name: derived
    mode: shadow
    scorer: local
    inputs: {same_as: base}
    labels: [benign, abusive]
    benign_label: benign
    threshold: 0.5
`
	cfg, err := config.Load([]byte(yaml), deps)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var base, derived config.Rule
	for _, r := range cfg.Rules {
		switch r.Name {
		case "base":
			base = r
		case "derived":
			derived = r
		}
	}
	if len(derived.Inputs) != len(base.Inputs) {
		t.Fatalf("expected derived.Inputs to match base.Inputs, got %v vs %v", derived.Inputs, base.Inputs)
	}
	for i := range base.Inputs {
		if derived.Inputs[i] != base.Inputs[i] {
			t.Fatalf("derived.Inputs[%d] = %q, want %q", i, derived.Inputs[i], base.Inputs[i])
		}
	}
}

func TestRule_IsTextOnly(t *testing.T) {
	tests := []struct {
		name string
		r    config.Rule
		want bool
	}{
		{"text only", config.Rule{Text: []string{"a"}}, true},
		{"inputs only", config.Rule{Inputs: []string{"a"}}, false},
		{"both", config.Rule{Text: []string{"a"}, Inputs: []string{"b"}}, false},
		{"neither", config.Rule{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.r.IsTextOnly(); got != tc.want {
				t.Errorf("IsTextOnly() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLoadVendors(t *testing.T) {
	yaml := `
vendors:
  - name: local
    terms_version: "n/a"
    dpa_ref: "n/a"
    policy:
      allows_text: false
      retains_inputs: false
      trains_on_inputs: false
`
	vendors, err := config.LoadVendors([]byte(yaml))
	if err != nil {
		t.Fatalf("LoadVendors: %v", err)
	}
	local, ok := vendors["local"]
	if !ok {
		t.Fatalf("expected a local entry")
	}
	if local.Policy.AllowsText {
		t.Errorf("expected local's policy to forbid text")
	}
}

// TestShippedConfigsLoadTogether is the end-to-end sanity check that
// config/rules.yaml, config/vendors.yaml, and config/local_weights.yaml —
// the three files this repo actually ships — are mutually consistent
// against a registry containing only the real `local` scorer. This is
// the same wiring cmd/abusekit's `serve --check` performs at boot.
func TestShippedConfigsLoadTogether(t *testing.T) {
	root := repoRoot(t)

	weights, err := local.LoadWeightsFile(filepath.Join(root, "config", "local_weights.yaml"))
	if err != nil {
		t.Fatalf("LoadWeightsFile: %v", err)
	}
	localScorer, err := local.New(weights)
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}
	reg := model.NewRegistry()
	if err := reg.Register(localScorer); err != nil {
		t.Fatalf("Register: %v", err)
	}

	vendorsData, err := os.ReadFile(filepath.Join(root, "config", "vendors.yaml"))
	if err != nil {
		t.Fatalf("read vendors.yaml: %v", err)
	}
	vendors, err := config.LoadVendors(vendorsData)
	if err != nil {
		t.Fatalf("LoadVendors: %v", err)
	}

	rulesData, err := os.ReadFile(filepath.Join(root, "config", "rules.yaml"))
	if err != nil {
		t.Fatalf("read rules.yaml: %v", err)
	}
	features := config.NewFeatureSet(
		"subject_age_h", "resource_velocity_1h", "resource_total", "key_velocity_1h", "key_total",
		"upgrade_delay_min", "declines_before_first_success", "first_funding_prepaid",
		"name_brand_match", "name_has_at", "first_day_distinct_domains", "self_send_before_external",
		"linked_deleted_n", "linked_labelled_abusive_n", "fingerprint_seen_on_other_subjects",
		"burst_ratio_24h_vs_lifetime",
	)
	cfg, err := config.Load(rulesData, config.Dependencies{
		Registry: reg,
		Features: features,
		Vendors:  vendors,
	})
	if err != nil {
		t.Fatalf("Load(shipped rules.yaml): %v", err)
	}
	if len(cfg.Rules) == 0 {
		t.Fatalf("expected at least one rule from the shipped config")
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller failed")
	}
	// this file: <root>/internal/config/config_test.go
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

func TestLoadVendors_RejectsDuplicateNames(t *testing.T) {
	yaml := `
vendors:
  - name: local
    policy: {allows_text: false}
  - name: local
    policy: {allows_text: true}
`
	if _, err := config.LoadVendors([]byte(yaml)); err == nil {
		t.Fatalf("expected an error for a duplicate vendor name")
	}
}
