package config_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/tokencanopy/abusekit/internal/config"
	"github.com/tokencanopy/abusekit/internal/feature"
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
	// S9: the loader compares this against Vendors["local"].Policy below —
	// they must agree for tests that don't specifically exercise the
	// mismatch check.
	localLike.DataPolicyValue = model.DataPolicy{AllowsText: false, TermsVersion: "n/a"}
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
	textVendor.DataPolicyValue = model.DataPolicy{AllowsText: true, TermsVersion: "v1"}
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
		Features: config.NewFeatureSet("core.subject_age_h", "core.resource_velocity_1h"),
		Vendors: map[string]config.VendorEntry{
			"local":      {Name: "local", Policy: model.DataPolicy{AllowsText: false, TermsVersion: "n/a"}},
			"textvendor": {Name: "textvendor", Policy: model.DataPolicy{AllowsText: true, TermsVersion: "v1"}},
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
    inputs: [core.subject_age_h, core.resource_velocity_1h]
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
    inputs: [core.subject_age_h]
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
    inputs: [core.subject_age_h]
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
    inputs: [core.subject_age_h]
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
    inputs: [core.subject_age_h]
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
			// R11 (round 2): the `inputs: {same_as: ...}` mapping is decoded
			// via yaml.Node.Decode, which does NOT go through the top-level
			// Decoder's KnownFields(true) — so an extra, unrecognized key
			// alongside same_as was silently ignored despite the rest of
			// Load being strict.
			name: "same_as mapping with an extra unknown key",
			yaml: `
tiers: {medium: 0.4, high: 0.8}
rules:
  - name: base
    mode: advise
    scorer: local
    inputs: [core.subject_age_h]
    labels: [benign, abusive]
    benign_label: benign
    threshold: 0.5
  - name: derived
    mode: shadow
    scorer: local
    inputs: {same_as: base, totally_bogus_key: 1}
    labels: [benign, abusive]
    benign_label: benign
    threshold: 0.5
`,
			wantErr: "same_as",
		},
		{
			name: "duplicate rule name",
			yaml: `
tiers: {medium: 0.4, high: 0.8}
rules:
  - name: r1
    mode: advise
    scorer: local
    inputs: [core.subject_age_h]
    labels: [benign, abusive]
    benign_label: benign
    threshold: 0.5
  - name: r1
    mode: advise
    scorer: local
    inputs: [core.subject_age_h]
    labels: [benign, abusive]
    benign_label: benign
    threshold: 0.5
`,
			wantErr: "duplicate rule name",
		},
		// --- S3: config loader strictness -------------------------------
		{
			name: "unknown top-level field",
			yaml: `
tiers: {medium: 0.4, high: 0.8}
totally_unknown_top_level_field: 1
rules:
  - name: r1
    mode: advise
    scorer: local
    inputs: [core.subject_age_h]
    labels: [benign, abusive]
    benign_label: benign
    threshold: 0.5
`,
			wantErr: "field totally_unknown_top_level_field not found",
		},
		{
			name: "unknown rule-level field (typo)",
			yaml: `
tiers: {medium: 0.4, high: 0.8}
rules:
  - name: r1
    mode: advise
    scorer: local
    inputs: [core.subject_age_h]
    labels: [benign, abusive]
    benign_label: benign
    theshold: 0.5
`,
			wantErr: "field theshold not found",
		},
		{
			name: "missing threshold",
			yaml: `
tiers: {medium: 0.4, high: 0.8}
rules:
  - name: r1
    mode: advise
    scorer: local
    inputs: [core.subject_age_h]
    labels: [benign, abusive]
    benign_label: benign
`,
			wantErr: "missing threshold",
		},
		{
			name: "NaN threshold",
			yaml: `
tiers: {medium: 0.4, high: 0.8}
rules:
  - name: r1
    mode: advise
    scorer: local
    inputs: [core.subject_age_h]
    labels: [benign, abusive]
    benign_label: benign
    threshold: .nan
`,
			wantErr: "threshold",
		},
		{
			name: "NaN tier cut point",
			yaml: `
tiers: {medium: .nan, high: 0.8}
rules:
  - name: r1
    mode: advise
    scorer: local
    inputs: [core.subject_age_h]
    labels: [benign, abusive]
    benign_label: benign
    threshold: 0.5
`,
			wantErr: "tiers.medium",
		},
		{
			name: "NaN min_local_risk stage condition",
			yaml: `
tiers: {medium: 0.4, high: 0.8}
rules:
  - name: local_rule
    mode: advise
    scorer: local
    inputs: [core.subject_age_h]
    labels: [benign, abusive]
    benign_label: benign
    threshold: 0.5
  - name: r1
    mode: shadow
    scorer: textvendor
    text: [subject_line_skeleton]
    labels: [benign, phishing]
    benign_label: benign
    threshold: 0.5
    stage: {min_local_risk: .nan}
`,
			wantErr: "min_local_risk",
		},
		{
			name: "negative max_subject_age_h stage condition",
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
    stage: {max_subject_age_h: -1}
`,
			wantErr: "max_subject_age_h",
		},
		{
			name: "duplicate labels",
			yaml: `
tiers: {medium: 0.4, high: 0.8}
rules:
  - name: r1
    mode: advise
    scorer: local
    inputs: [core.subject_age_h]
    labels: [benign, abusive, benign]
    benign_label: benign
    threshold: 0.5
`,
			wantErr: "duplicate label",
		},
		{
			name: "empty label",
			yaml: `
tiers: {medium: 0.4, high: 0.8}
rules:
  - name: r1
    mode: advise
    scorer: local
    inputs: [core.subject_age_h]
    labels: [benign, "", abusive]
    benign_label: benign
    threshold: 0.5
`,
			wantErr: "empty label",
		},
		{
			name: "duplicate inputs",
			yaml: `
tiers: {medium: 0.4, high: 0.8}
rules:
  - name: r1
    mode: advise
    scorer: local
    inputs: [core.subject_age_h, core.subject_age_h]
    labels: [benign, abusive]
    benign_label: benign
    threshold: 0.5
`,
			wantErr: "duplicate input",
		},
		{
			name: "rule gated on min_local_risk against itself",
			yaml: `
tiers: {medium: 0.4, high: 0.8}
rules:
  - name: local_rule
    mode: advise
    scorer: local
    inputs: [core.subject_age_h]
    labels: [benign, abusive]
    benign_label: benign
    threshold: 0.5
    stage: {min_local_risk: 0.3}
`,
			wantErr: "against itself",
		},
		{
			name: "rule name with a slash",
			yaml: `
tiers: {medium: 0.4, high: 0.8}
rules:
  - name: r1/bad
    mode: advise
    scorer: local
    inputs: [core.subject_age_h]
    labels: [benign, abusive]
    benign_label: benign
    threshold: 0.5
`,
			wantErr: "rule name",
		},
		{
			name: "min_scored_advise exceeds the number of advise rules",
			yaml: `
tiers: {medium: 0.4, high: 0.8}
min_scored_advise: 5
rules:
  - name: r1
    mode: advise
    scorer: local
    inputs: [core.subject_age_h]
    labels: [benign, abusive]
    benign_label: benign
    threshold: 0.5
`,
			wantErr: "min_scored_advise",
		},
		{
			// R11 (round 2): zero advise rules at all (only shadow) with
			// min_scored_advise defaulting to 1 (or set explicitly) means
			// scoredAdviseCount can never reach minScoredAdvise — every
			// subject would be tier=unknown forever. The old check
			// (`adviseCount > 0 && minScoredAdvise > adviseCount`) skipped
			// entirely when adviseCount was 0, so this config loaded fine.
			name: "zero advise rules with min_scored_advise >= 1",
			yaml: `
tiers: {medium: 0.4, high: 0.8}
rules:
  - name: r1
    mode: shadow
    scorer: local
    inputs: [core.subject_age_h]
    labels: [benign, abusive]
    benign_label: benign
    threshold: 0.5
`,
			wantErr: "min_scored_advise",
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
    inputs: [core.subject_age_h, core.resource_velocity_1h]
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

// TestRule_IsTextOnly is S15. Decision (documented on IsTextOnly's doc
// comment and in the design amendment): a rule declaring ANY `text`
// counts as needing the text_rules_need_feature_support cap, regardless
// of whether it also has `inputs` — the reviewer's "simplest" option.
// Proven gap in the old "no inputs at all" definition: a rule with text
// PLUS a trivial/onboarding-fact input (e.g. `email_domain_class`, which
// never ages out and never itself signals risk) escaped the cap entirely,
// even though nothing about that rule's contribution is genuinely
// feature-driven.
func TestRule_IsTextOnly(t *testing.T) {
	tests := []struct {
		name string
		r    config.Rule
		want bool
	}{
		{"text only", config.Rule{Text: []string{"a"}}, true},
		{"inputs only", config.Rule{Inputs: []string{"a"}}, false},
		{"text plus inputs is still capped", config.Rule{Text: []string{"a"}, Inputs: []string{"b"}}, true},
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
	// S2 fix round: sourced from internal/feature.Names (the same single
	// source of truth cmd/abusekit uses) instead of a hand-copied literal,
	// so this test can't silently drift from what Extract actually
	// computes the way the S1-era literal here did the moment B5/S1 fix
	// round added `core.upgraded`/`core.neighbors_truncated`.
	features := config.NewFeatureSet(feature.Names...)
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

// --- S3: strictness cases that don't fit the table-driven shape above ---

func TestLoad_NilFeaturesIsAnError(t *testing.T) {
	deps, _ := newFixture(t)
	deps.Features = nil // was silently "every feature is unknown... or is it?" before S3
	_, err := config.Load([]byte(validYAML), deps)
	if err == nil || !strings.Contains(err.Error(), "Features") {
		t.Fatalf("expected an error mentioning Features for a nil FeatureSet, got %v", err)
	}
}

func TestLoad_TextRulesNeedFeatureSupportDefaultsToTrue(t *testing.T) {
	deps, _ := newFixture(t)
	yaml := `
tiers: {medium: 0.4, high: 0.8}
rules:
  - name: r1
    mode: advise
    scorer: local
    inputs: [core.subject_age_h]
    labels: [benign, abusive]
    benign_label: benign
    threshold: 0.5
`
	cfg, err := config.Load([]byte(yaml), deps)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.TextRulesNeedFeatureSupport {
		t.Fatalf("expected text_rules_need_feature_support to default to true when omitted")
	}
}

func TestLoad_TextRulesNeedFeatureSupportExplicitFalse(t *testing.T) {
	deps, _ := newFixture(t)
	yaml := `
tiers: {medium: 0.4, high: 0.8}
text_rules_need_feature_support: false
rules:
  - name: r1
    mode: advise
    scorer: local
    inputs: [core.subject_age_h]
    labels: [benign, abusive]
    benign_label: benign
    threshold: 0.5
`
	cfg, err := config.Load([]byte(yaml), deps)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TextRulesNeedFeatureSupport {
		t.Fatalf("expected an explicit false to be honoured, not overridden by the default")
	}
}

// TestLoad_RejectsVendorPolicyMismatch is S9: the loader must compare an
// adapter's own reported Policy() against its vendors.yaml entry and
// reject any disagreement, so a stale allowlist entry can never diverge
// silently from what the adapter actually does with its inputs.
func TestLoad_RejectsVendorPolicyMismatch(t *testing.T) {
	reg := model.NewRegistry()
	drift := fake.New()
	drift.NameValue = "driftvendor"
	drift.Caps = model.Capabilities{LabelMode: model.OpenLabelMode(), AcceptsFeatures: true, Calibrated: true}
	drift.DataPolicyValue = model.DataPolicy{AllowsText: false, TermsVersion: "v1"}
	if err := reg.Register(drift); err != nil {
		t.Fatal(err)
	}

	deps := config.Dependencies{
		Registry: reg,
		Features: config.NewFeatureSet("core.subject_age_h"),
		Vendors: map[string]config.VendorEntry{
			// AllowsText disagrees with drift.DataPolicyValue above.
			"driftvendor": {Name: "driftvendor", Policy: model.DataPolicy{AllowsText: true, TermsVersion: "v1"}},
		},
	}
	yaml := `
tiers: {medium: 0.4, high: 0.8}
rules:
  - name: r1
    mode: advise
    scorer: driftvendor
    inputs: [core.subject_age_h]
    labels: [benign, abusive]
    benign_label: benign
    threshold: 0.5
`
	_, err := config.Load([]byte(yaml), deps)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("expected a vendor policy mismatch error, got %v", err)
	}
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

// TestLoadVendors_RejectsMissingTermsOrDPA is S9: an allowlist entry
// recording no terms_version or no dpa_ref is a data-governance gap, not
// a valid placeholder — vendors.yaml uses the literal string "n/a" when
// an adapter genuinely has no vendor terms (e.g. local), so an empty
// string always means "forgot to fill this in".
func TestLoadVendors_RejectsMissingTermsOrDPA(t *testing.T) {
	tests := []struct {
		name string
		yaml string
	}{
		{
			name: "missing terms_version",
			yaml: `
vendors:
  - name: local
    dpa_ref: "n/a"
    policy: {allows_text: false}
`,
		},
		{
			name: "missing dpa_ref",
			yaml: `
vendors:
  - name: local
    terms_version: "n/a"
    policy: {allows_text: false}
`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := config.LoadVendors([]byte(tc.yaml)); err == nil {
				t.Fatalf("expected an error for an entry with no %s", tc.name)
			}
		})
	}
}

// TestLoadVendors_RejectsUnknownPolicyField is R6 (round 2): strict
// decoding (KnownFields) so a misspelled policy key (e.g. "allow_text"
// instead of "allows_text") fails the load instead of silently leaving
// the real field at its zero value (false) — a vendor that DOES allow
// text would load as if it didn't, and the loader's own AllowsText check
// elsewhere would then wrongly reject a rule sending it text.
func TestLoadVendors_RejectsUnknownPolicyField(t *testing.T) {
	yaml := `
vendors:
  - name: local
    terms_version: "n/a"
    dpa_ref: "n/a"
    policy:
      allow_text: false
      retains_inputs: false
      trains_on_inputs: false
`
	if _, err := config.LoadVendors([]byte(yaml)); err == nil {
		t.Fatalf("expected an error for a misspelled policy field")
	}
}
