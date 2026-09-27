package core_test

import (
	"math"
	"testing"

	"github.com/tokencanopy/abusekit/internal/config"
	"github.com/tokencanopy/abusekit/internal/core"
	"github.com/tokencanopy/abusekit/internal/model"
)

func rule(name string, mode config.Mode, opts ...func(*config.Rule)) config.Rule {
	r := config.Rule{
		Name:        name,
		Mode:        mode,
		Scorer:      "local",
		Labels:      []string{"benign", "abusive"},
		BenignLabel: "benign",
		Threshold:   0.6,
	}
	for _, o := range opts {
		o(&r)
	}
	return r
}

func withInputs(inputs ...string) func(*config.Rule) {
	return func(r *config.Rule) { r.Inputs = inputs }
}
func withText(text ...string) func(*config.Rule) {
	return func(r *config.Rule) { r.Text = text }
}
func withStage(stage map[string]float64) func(*config.Rule) {
	return func(r *config.Rule) { r.Stage = stage }
}
func withScorer(s string) func(*config.Rule) {
	return func(r *config.Rule) { r.Scorer = s }
}

func floatp(f float64) *float64 { return &f }

// --- Plan ---------------------------------------------------------------

func TestPlan_BuildsRequestFromInputsAndText(t *testing.T) {
	features := map[string]float64{"subject_age_h": 2, "resource_velocity_1h": 6, "unrelated": 99}
	text := map[string][]string{"subject_line_skeleton": {"urgent action"}, "other_text": {"ignored"}}

	rules := []core.RuleState{
		{Rule: rule("feature_rule", config.ModeAdvise, withInputs("subject_age_h", "resource_velocity_1h"))},
		{Rule: rule("text_rule", config.ModeShadow, withText("subject_line_skeleton"))},
	}

	calls := core.Plan(features, text, rules)
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}

	featureCall := calls[0]
	if featureCall.Skip {
		t.Fatalf("expected feature_rule not to be skipped")
	}
	if got := featureCall.Request.Features; got["subject_age_h"] != 2 || got["resource_velocity_1h"] != 6 {
		t.Fatalf("unexpected features in request: %#v", got)
	}
	if _, ok := featureCall.Request.Features["unrelated"]; ok {
		t.Fatalf("expected only the rule's own inputs to be included, got %#v", featureCall.Request.Features)
	}

	textCall := calls[1]
	if len(textCall.Request.Text) != 1 || textCall.Request.Text[0] != "urgent action" {
		t.Fatalf("unexpected text in request: %#v", textCall.Request.Text)
	}
}

func TestPlan_MissingFeatureDefaultsToZero(t *testing.T) {
	rules := []core.RuleState{
		{Rule: rule("r", config.ModeAdvise, withInputs("never_set"))},
	}
	calls := core.Plan(map[string]float64{}, nil, rules)
	if v, ok := calls[0].Request.Features["never_set"]; !ok || v != 0 {
		t.Fatalf("expected missing feature to default to 0, got %#v", calls[0].Request.Features)
	}
}

func TestPlan_SkipsOnUnchangedInputHash(t *testing.T) {
	features := map[string]float64{"x": 1}
	r := rule("r", config.ModeAdvise, withInputs("x"))

	// First round: no prior hash, must not skip.
	first := core.Plan(features, nil, []core.RuleState{{Rule: r}})
	if first[0].Skip {
		t.Fatalf("expected first round not to skip")
	}

	// Second round with the same features and the recorded hash: must skip.
	second := core.Plan(features, nil, []core.RuleState{{Rule: r, LastInputHash: first[0].InputHash}})
	if !second[0].Skip || second[0].SkipReason != core.SkipInputUnchanged {
		t.Fatalf("expected second round to skip with reason %q, got skip=%v reason=%q",
			core.SkipInputUnchanged, second[0].Skip, second[0].SkipReason)
	}

	// Third round with a changed feature: must not skip, and the hash differs.
	third := core.Plan(map[string]float64{"x": 2}, nil, []core.RuleState{{Rule: r, LastInputHash: first[0].InputHash}})
	if third[0].Skip {
		t.Fatalf("expected a changed feature to avoid the skip")
	}
	if third[0].InputHash == first[0].InputHash {
		t.Fatalf("expected a changed feature to change the input hash")
	}
}

func TestPlan_StageMinLocalRisk(t *testing.T) {
	localRule := rule("local_rule", config.ModeAdvise, withScorer("local"), withInputs("x"))
	shadowRule := rule("shadow_rule", config.ModeShadow, withScorer("jev"), withText("t"),
		withStage(map[string]float64{"min_local_risk": 0.3}))

	// No prior local risk at all -> gated off.
	calls := core.Plan(map[string]float64{"x": 1}, map[string][]string{"t": {"hi"}}, []core.RuleState{
		{Rule: localRule},
		{Rule: shadowRule},
	})
	if !calls[1].Skip || calls[1].SkipReason != core.SkipStageCondition {
		t.Fatalf("expected shadow_rule to be stage-skipped with no prior local risk, got %#v", calls[1])
	}

	// Prior local risk below the gate -> still gated off.
	below := core.Plan(map[string]float64{"x": 1}, map[string][]string{"t": {"hi"}}, []core.RuleState{
		{Rule: localRule, LastRisk: floatp(0.2)},
		{Rule: shadowRule},
	})
	if !below[1].Skip {
		t.Fatalf("expected shadow_rule to be stage-skipped when local risk is below the gate")
	}

	// Prior local risk at/above the gate -> runs.
	above := core.Plan(map[string]float64{"x": 1}, map[string][]string{"t": {"hi"}}, []core.RuleState{
		{Rule: localRule, LastRisk: floatp(0.5)},
		{Rule: shadowRule},
	})
	if above[1].Skip {
		t.Fatalf("expected shadow_rule to run once local risk clears the gate, got skip reason %q", above[1].SkipReason)
	}
}

// --- S2: input hash must reflect everything that changes the answer -----

// TestPlan_NaNFeatureNeverPermanentlySkips is S2. Proven: before this fix,
// a NaN feature made json.Marshal fail inside inputHash, which fell back
// to hashing just "ruleName/scorer" — a CONSTANT regardless of the actual
// (broken) feature values, so a rule with a permanently-NaN feature would
// compute the identical hash every round, get recorded as
// LastInputHash, and then skip forever on "input_unchanged" even though
// its input was never validly hashed at all.
func TestPlan_NaNFeatureNeverPermanentlySkips(t *testing.T) {
	r := rule("r", config.ModeAdvise, withInputs("x"))
	features := map[string]float64{"x": math.NaN()}

	first := core.Plan(features, nil, []core.RuleState{{Rule: r}})
	if first[0].InputHash != "" {
		t.Fatalf("expected a NaN feature to produce the empty sentinel hash, got %q", first[0].InputHash)
	}
	if first[0].Skip {
		t.Fatalf("expected the first round not to skip")
	}

	// Simulate the caller recording round 1's (empty) hash, then Plan
	// running again with the SAME NaN feature: it must still not skip.
	second := core.Plan(features, nil, []core.RuleState{{Rule: r, LastInputHash: first[0].InputHash}})
	if second[0].Skip {
		t.Fatalf("expected a NaN feature to never trigger input_unchanged skipping, even across rounds")
	}
}

// TestPlan_BenignLabelChangeForcesRescore is S2. Proven: changing a
// rule's benign_label (e.g. via a config edit) while its Labels list and
// Features stay identical did not change the input hash at all, since
// BenignLabel was never part of the hashed payload — Plan would skip
// re-scoring a rule whose risk polarity just flipped.
func TestPlan_BenignLabelChangeForcesRescore(t *testing.T) {
	features := map[string]float64{"x": 1}
	base := rule("r", config.ModeAdvise, withInputs("x"))
	base.Labels = []string{"benign", "abusive"}
	base.BenignLabel = "benign"

	first := core.Plan(features, nil, []core.RuleState{{Rule: base}})

	changed := base
	changed.BenignLabel = "abusive" // same Labels list, different benign label
	second := core.Plan(features, nil, []core.RuleState{{Rule: changed, LastInputHash: first[0].InputHash}})

	if second[0].InputHash == first[0].InputHash {
		t.Fatalf("expected a benign_label change to change the input hash")
	}
	if second[0].Skip {
		t.Fatalf("expected a benign_label change to force a rescore, not skip as input_unchanged")
	}
}

// TestPlan_ScorerVersionChangeForcesRescore is S2: a RuleState carrying a
// different ScorerVersion (model.Scorer.Version(), e.g. after a weights
// file edit) must change the input hash even when features/text/labels
// are byte-identical, so a scorer upgrade can never be masked by
// input-hash skipping.
func TestPlan_ScorerVersionChangeForcesRescore(t *testing.T) {
	features := map[string]float64{"x": 1}
	r := rule("r", config.ModeAdvise, withInputs("x"))

	v1 := core.Plan(features, nil, []core.RuleState{{Rule: r, ScorerVersion: "weights-v1"}})
	v2 := core.Plan(features, nil, []core.RuleState{{Rule: r, ScorerVersion: "weights-v2", LastInputHash: v1[0].InputHash}})

	if v2[0].InputHash == v1[0].InputHash {
		t.Fatalf("expected a ScorerVersion change to change the input hash")
	}
	if v2[0].Skip {
		t.Fatalf("expected a ScorerVersion change to force a rescore")
	}
}

// TestPlan_CalibrationIDChangeForcesRescore is S2: a newly fitted
// calibration map (a different CalibrationID for the same rule/scorer)
// must force a rescore even with identical features.
func TestPlan_CalibrationIDChangeForcesRescore(t *testing.T) {
	features := map[string]float64{"x": 1}
	r := rule("r", config.ModeAdvise, withInputs("x"))

	c1 := core.Plan(features, nil, []core.RuleState{{Rule: r, CalibrationID: "cal_1"}})
	c2 := core.Plan(features, nil, []core.RuleState{{Rule: r, CalibrationID: "cal_2", LastInputHash: c1[0].InputHash}})

	if c2[0].InputHash == c1[0].InputHash {
		t.Fatalf("expected a CalibrationID change to change the input hash")
	}
	if c2[0].Skip {
		t.Fatalf("expected a CalibrationID change to force a rescore")
	}
}

func TestPlan_StageSubjectAge(t *testing.T) {
	old := rule("old_gate", config.ModeShadow, withText("t"), withStage(map[string]float64{"max_subject_age_h": 168}))

	young := core.Plan(map[string]float64{"subject_age_h": 10}, map[string][]string{"t": {"x"}}, []core.RuleState{{Rule: old}})
	if young[0].Skip {
		t.Fatalf("expected a young subject to pass max_subject_age_h")
	}

	agedOut := core.Plan(map[string]float64{"subject_age_h": 200}, map[string][]string{"t": {"x"}}, []core.RuleState{{Rule: old}})
	if !agedOut[0].Skip {
		t.Fatalf("expected an old subject to be stage-skipped by max_subject_age_h")
	}
}

// --- Combine --------------------------------------------------------------

func scoredResult(pBenign float64) *model.ScoreResult {
	return &model.ScoreResult{Probs: map[string]float64{"benign": pBenign, "abusive": 1 - pBenign}, Model: "local", Checkpoint: "v1"}
}

func TestCombine_RiskFlaggedFromProbs(t *testing.T) {
	outcomes := []core.RuleOutcome{
		{Rule: rule("r", config.ModeAdvise), Result: scoredResult(0.3)}, // risk 0.7
	}
	v := core.Combine(outcomes, core.CombineParams{Tiers: config.Tiers{Medium: 0.4, High: 0.8}, MinScoredAdvise: 1}, nil)
	if len(v.Signals) != 1 {
		t.Fatalf("expected 1 signal, got %d", len(v.Signals))
	}
	sig := v.Signals[0]
	if sig.Risk != 0.7 {
		t.Fatalf("expected risk 0.7, got %v", sig.Risk)
	}
	if !sig.Flagged {
		t.Fatalf("expected flagged=true for risk 0.7 >= threshold 0.6")
	}
	if v.Score != 0.7 || v.Tier != "medium" {
		t.Fatalf("expected score=0.7 tier=medium, got score=%v tier=%s", v.Score, v.Tier)
	}
}

func TestCombine_ShadowExcludedFromScore(t *testing.T) {
	outcomes := []core.RuleOutcome{
		{Rule: rule("advise_low", config.ModeAdvise), Result: scoredResult(0.9)},   // risk 0.1
		{Rule: rule("shadow_high", config.ModeShadow), Result: scoredResult(0.01)}, // risk 0.99, but shadow
	}
	v := core.Combine(outcomes, core.CombineParams{Tiers: config.Tiers{Medium: 0.4, High: 0.8}, MinScoredAdvise: 1}, nil)
	if diff := v.Score - 0.1; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("expected shadow rule excluded from score (want ~0.1), got %v", v.Score)
	}
	if v.Tier != "low" {
		t.Fatalf("expected tier=low, got %s", v.Tier)
	}
	// The shadow rule still appears as a signal.
	found := false
	for _, s := range v.Signals {
		if s.Rule == "shadow_high" {
			found = true
			if s.Risk != 0.99 {
				t.Errorf("expected shadow signal to report its own real risk, got %v", s.Risk)
			}
		}
	}
	if !found {
		t.Fatalf("expected a signal for the shadow rule")
	}
}

func TestCombine_DegradedOnlyFromUnscoredAdvise(t *testing.T) {
	tests := []struct {
		name         string
		outcomes     []core.RuleOutcome
		wantDegraded bool
	}{
		{
			name: "unscored advise degrades",
			outcomes: []core.RuleOutcome{
				{Rule: rule("a", config.ModeAdvise), Unscored: true, ErrorCode: "cost_cap"},
			},
			wantDegraded: true,
		},
		{
			name: "unscored shadow does not degrade",
			outcomes: []core.RuleOutcome{
				{Rule: rule("a", config.ModeAdvise), Result: scoredResult(0.9)},
				{Rule: rule("b", config.ModeShadow), Unscored: true, ErrorCode: "cost_cap"},
			},
			wantDegraded: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := core.Combine(tc.outcomes, core.CombineParams{Tiers: config.Tiers{Medium: 0.4, High: 0.8}, MinScoredAdvise: 1}, nil)
			if v.Degraded != tc.wantDegraded {
				t.Fatalf("Degraded = %v, want %v", v.Degraded, tc.wantDegraded)
			}
		})
	}
}

func TestCombine_UnscoredSignalShape(t *testing.T) {
	outcomes := []core.RuleOutcome{
		{Rule: rule("a", config.ModeAdvise), Unscored: true, ErrorCode: "cost_cap"},
	}
	v := core.Combine(outcomes, core.CombineParams{Tiers: config.Tiers{Medium: 0.4, High: 0.8}, MinScoredAdvise: 1}, nil)
	sig := v.Signals[0]
	if sig.Status != "unscored" || sig.ErrorCode != "cost_cap" {
		t.Fatalf("unexpected unscored signal: %#v", sig)
	}
}

func TestCombine_TierUnknownBelowMinScoredAdvise(t *testing.T) {
	outcomes := []core.RuleOutcome{
		{Rule: rule("a", config.ModeAdvise), Unscored: true, ErrorCode: "backoff"},
	}
	v := core.Combine(outcomes, core.CombineParams{Tiers: config.Tiers{Medium: 0.4, High: 0.8}, MinScoredAdvise: 1}, nil)
	if v.Tier != "unknown" {
		t.Fatalf("expected tier=unknown with zero scored advise rules, got %s", v.Tier)
	}
}

func TestCombine_TierCutPoints(t *testing.T) {
	tiers := config.Tiers{Medium: 0.4, High: 0.8}
	tests := []struct {
		risk float64
		want string
	}{
		{0.0, "low"},
		{0.39, "low"},
		{0.4, "medium"},
		{0.79, "medium"},
		{0.8, "high"},
		{1.0, "high"},
	}
	for _, tc := range tests {
		outcomes := []core.RuleOutcome{{Rule: rule("a", config.ModeAdvise), Result: scoredResult(1 - tc.risk)}}
		v := core.Combine(outcomes, core.CombineParams{Tiers: tiers, MinScoredAdvise: 1}, nil)
		if v.Tier != tc.want {
			t.Errorf("risk %v: tier = %s, want %s", tc.risk, v.Tier, tc.want)
		}
	}
}

func TestCombine_TextRuleCannotCrossTierAlone(t *testing.T) {
	textRule := rule("text_rule", config.ModeAdvise, withText("t"))
	textRule.Threshold = 0.0
	outcomes := []core.RuleOutcome{
		{Rule: textRule, Result: scoredResult(0.05)}, // risk 0.95, text-only, no feature support
	}
	params := core.CombineParams{Tiers: config.Tiers{Medium: 0.4, High: 0.8}, MinScoredAdvise: 1, TextRulesNeedFeatureSupport: true}
	v := core.Combine(outcomes, params, nil)

	if v.Score >= params.Tiers.Medium {
		t.Fatalf("expected the text-only rule's contribution to score to be capped below medium, got %v", v.Score)
	}
	if v.Tier != "low" {
		t.Fatalf("expected tier=low despite a 0.95-risk text rule, got %s", v.Tier)
	}
	// The rule's own reported signal is NOT capped — only its contribution to `score`.
	if v.Signals[0].Risk != 0.95 {
		t.Fatalf("expected the signal's own risk to remain uncapped at 0.95, got %v", v.Signals[0].Risk)
	}
	if !v.Signals[0].Flagged {
		t.Fatalf("expected the signal to still be flagged against its own threshold")
	}
}

func TestCombine_TextRuleUncappedWithFeatureSupport(t *testing.T) {
	featureRule := rule("feature_rule", config.ModeAdvise, withInputs("x"))
	textRule := rule("text_rule", config.ModeAdvise, withText("t"))
	outcomes := []core.RuleOutcome{
		{Rule: featureRule, Result: scoredResult(1 - 0.5)}, // risk 0.5 >= medium(0.4): feature support present
		{Rule: textRule, Result: scoredResult(1 - 0.95)},   // risk 0.95
	}
	params := core.CombineParams{Tiers: config.Tiers{Medium: 0.4, High: 0.8}, MinScoredAdvise: 1, TextRulesNeedFeatureSupport: true}
	v := core.Combine(outcomes, params, nil)
	if v.Score != 0.95 {
		t.Fatalf("expected the text rule's full risk once a feature rule supports it, got %v", v.Score)
	}
	if v.Tier != "high" {
		t.Fatalf("expected tier=high, got %s", v.Tier)
	}
}

func TestCombine_TextRuleUncappedWhenFeatureFlagOff(t *testing.T) {
	textRule := rule("text_rule", config.ModeAdvise, withText("t"))
	outcomes := []core.RuleOutcome{
		{Rule: textRule, Result: scoredResult(1 - 0.95)},
	}
	params := core.CombineParams{Tiers: config.Tiers{Medium: 0.4, High: 0.8}, MinScoredAdvise: 1, TextRulesNeedFeatureSupport: false}
	v := core.Combine(outcomes, params, nil)
	if v.Score != 0.95 {
		t.Fatalf("expected no capping when TextRulesNeedFeatureSupport is false, got %v", v.Score)
	}
}

func TestCombine_AppliesCalibration(t *testing.T) {
	outcomes := []core.RuleOutcome{
		{Rule: rule("r", config.ModeAdvise), Result: scoredResult(1 - 0.5)}, // raw risk 0.5
	}
	calib := core.CalibrationSet{
		core.Key("r", "local", "v1"): core.CalibrationEntry{ID: "cal_test", Calibrator: core.CalibratorFunc(func(raw float64) float64 { return raw * 0.5 })},
	}
	v := core.Combine(outcomes, core.CombineParams{Tiers: config.Tiers{Medium: 0.4, High: 0.8}, MinScoredAdvise: 1}, calib)
	if v.Signals[0].Risk != 0.25 {
		t.Fatalf("expected calibrated risk 0.25, got %v", v.Signals[0].Risk)
	}
	// B2/S12: the recorded calibration id must come from the SAME lookup
	// that produced the applied Calibrator, not from whatever the caller
	// happened to pass on RuleOutcome.Calibration.
	if v.Signals[0].Calibration != "cal_test" {
		t.Fatalf("expected recorded calibration id %q from the applied lookup, got %q", "cal_test", v.Signals[0].Calibration)
	}
}

func TestCombine_CalibrationFallsBackWhenNoEntryForCheckpoint(t *testing.T) {
	// scoredResult's Checkpoint is "v1"; a calibration recorded for a
	// different checkpoint must not be applied, and the caller-supplied
	// fallback Calibration id (e.g. "none" for an already-calibrated
	// scorer) is what gets recorded.
	outcomes := []core.RuleOutcome{
		{Rule: rule("r", config.ModeAdvise), Result: scoredResult(1 - 0.5), Calibration: "none"},
	}
	calib := core.CalibrationSet{
		core.Key("r", "local", "v2"): core.CalibrationEntry{ID: "cal_other", Calibrator: core.CalibratorFunc(func(raw float64) float64 { return raw * 0.5 })},
	}
	v := core.Combine(outcomes, core.CombineParams{Tiers: config.Tiers{Medium: 0.4, High: 0.8}, MinScoredAdvise: 1}, calib)
	if v.Signals[0].Risk != 0.5 {
		t.Fatalf("expected uncalibrated raw risk 0.5 (no entry for checkpoint v1), got %v", v.Signals[0].Risk)
	}
	if v.Signals[0].Calibration != "none" {
		t.Fatalf("expected fallback calibration id %q, got %q", "none", v.Signals[0].Calibration)
	}
}

// --- B2: Combine must never fail open on a malformed scoring result -------

func TestCombine_InvalidResult_NaNBenignProbability(t *testing.T) {
	// Proven: {benign: NaN} currently produces tier=low, score=0,
	// degraded=false — a NaN probability must instead be treated as
	// unscored/invalid_result, excluded from tier, and must degrade an
	// advise rule.
	outcomes := []core.RuleOutcome{
		{Rule: rule("a", config.ModeAdvise), Result: &model.ScoreResult{
			Probs: map[string]float64{"benign": math.NaN(), "abusive": 1},
		}},
	}
	v := core.Combine(outcomes, core.CombineParams{Tiers: config.Tiers{Medium: 0.4, High: 0.8}, MinScoredAdvise: 1}, nil)
	if v.Tier != "unknown" {
		t.Fatalf("expected tier=unknown for a NaN probability, got %s", v.Tier)
	}
	if v.Score != 0 {
		t.Fatalf("expected score=0, got %v", v.Score)
	}
	if !v.Degraded {
		t.Fatalf("expected degraded=true for an invalid advise-rule result")
	}
	if len(v.Signals) != 1 || v.Signals[0].Status != "unscored" || v.Signals[0].ErrorCode != "invalid_result" {
		t.Fatalf("expected an unscored signal with error_code=invalid_result, got %#v", v.Signals)
	}
}

func TestCombine_InvalidResult_ProbabilityOutOfRange(t *testing.T) {
	// Proven: P(benign) = 1.7 currently produces tier=low.
	outcomes := []core.RuleOutcome{
		{Rule: rule("a", config.ModeAdvise), Result: &model.ScoreResult{
			Probs: map[string]float64{"benign": 1.7, "abusive": -0.7},
		}},
	}
	v := core.Combine(outcomes, core.CombineParams{Tiers: config.Tiers{Medium: 0.4, High: 0.8}, MinScoredAdvise: 1}, nil)
	if v.Tier != "unknown" {
		t.Fatalf("expected tier=unknown for an out-of-range probability, got %s", v.Tier)
	}
	if v.Signals[0].Status != "unscored" || v.Signals[0].ErrorCode != "invalid_result" {
		t.Fatalf("expected unscored/invalid_result, got %#v", v.Signals[0])
	}
}

func TestCombine_InvalidResult_ProbabilitiesDontSumToOne(t *testing.T) {
	outcomes := []core.RuleOutcome{
		{Rule: rule("a", config.ModeAdvise), Result: &model.ScoreResult{
			Probs: map[string]float64{"benign": 0.9, "abusive": 0.9}, // sums to 1.8
		}},
	}
	v := core.Combine(outcomes, core.CombineParams{Tiers: config.Tiers{Medium: 0.4, High: 0.8}, MinScoredAdvise: 1}, nil)
	if v.Signals[0].Status != "unscored" || v.Signals[0].ErrorCode != "invalid_result" {
		t.Fatalf("expected unscored/invalid_result for probabilities not summing to 1, got %#v", v.Signals[0])
	}
}

func TestCombine_InvalidResult_MissingBenignLabel(t *testing.T) {
	outcomes := []core.RuleOutcome{
		{Rule: rule("a", config.ModeAdvise), Result: &model.ScoreResult{
			Probs: map[string]float64{"abusive": 1}, // no "benign" key at all
		}},
	}
	v := core.Combine(outcomes, core.CombineParams{Tiers: config.Tiers{Medium: 0.4, High: 0.8}, MinScoredAdvise: 1}, nil)
	if v.Signals[0].Status != "unscored" || v.Signals[0].ErrorCode != "invalid_result" {
		t.Fatalf("expected unscored/invalid_result for a missing benign label, got %#v", v.Signals[0])
	}
}

func TestCombine_InvalidResult_CalibratorProducesNaN(t *testing.T) {
	// Proven: a calibrator returning NaN currently produces tier=low.
	outcomes := []core.RuleOutcome{
		{Rule: rule("r", config.ModeAdvise), Result: scoredResult(1 - 0.5)}, // valid raw risk 0.5
	}
	calib := core.CalibrationSet{
		core.Key("r", "local", "v1"): core.CalibrationEntry{ID: "cal_broken", Calibrator: core.CalibratorFunc(func(raw float64) float64 { return math.NaN() })},
	}
	v := core.Combine(outcomes, core.CombineParams{Tiers: config.Tiers{Medium: 0.4, High: 0.8}, MinScoredAdvise: 1}, calib)
	if v.Tier != "unknown" {
		t.Fatalf("expected tier=unknown when the calibrator produces NaN, got %s", v.Tier)
	}
	if v.Signals[0].Status != "unscored" || v.Signals[0].ErrorCode != "invalid_result" {
		t.Fatalf("expected unscored/invalid_result, got %#v", v.Signals[0])
	}
}

func TestCombine_CalibratorOutputIsClamped(t *testing.T) {
	outcomes := []core.RuleOutcome{
		{Rule: rule("r", config.ModeAdvise), Result: scoredResult(1 - 0.5)},
	}
	calib := core.CalibrationSet{
		core.Key("r", "local", "v1"): core.CalibrationEntry{ID: "cal_over", Calibrator: core.CalibratorFunc(func(raw float64) float64 { return 1.5 })},
	}
	v := core.Combine(outcomes, core.CombineParams{Tiers: config.Tiers{Medium: 0.4, High: 0.8}, MinScoredAdvise: 1}, calib)
	if v.Signals[0].Risk != 1 {
		t.Fatalf("expected calibrator output clamped to 1, got %v", v.Signals[0].Risk)
	}
}

func TestCombine_ZeroOutcomesNeverScoresLowOrHigh(t *testing.T) {
	// Proven: CombineParams{} (zero value, MinScoredAdvise=0) against zero
	// outcomes currently returns tier=high.
	v := core.Combine(nil, core.CombineParams{}, nil)
	if v.Tier != "unknown" {
		t.Fatalf("expected tier=unknown for zero outcomes and a zero-value CombineParams, got %s", v.Tier)
	}
}

func TestCombine_ZeroAdviseRulesNeverScoresLowOrHigh(t *testing.T) {
	outcomes := []core.RuleOutcome{
		{Rule: rule("shadow_only", config.ModeShadow), Result: scoredResult(0.01)}, // risk 0.99, shadow only
	}
	v := core.Combine(outcomes, core.CombineParams{}, nil)
	if v.Tier != "unknown" {
		t.Fatalf("expected tier=unknown with zero advise rules scored, got %s", v.Tier)
	}
}
