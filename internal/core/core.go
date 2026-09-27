// Package core implements abusekit's pure scoring core (design §4.7):
// Plan decides which (rule, scorer, request) calls to make, and Combine
// reduces their results to a Verdict. Neither function performs I/O or
// calls a Scorer — that split is what makes the harness (S4) and CI able
// to drive the exact same logic the worker (S2) uses in production, just
// fed recorded results instead of live ones.
package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"

	"github.com/tokencanopy/abusekit/internal/config"
	"github.com/tokencanopy/abusekit/internal/model"
)

// ErrorCodeInvalidResult is the RuleOutcome/Signal error code Combine
// assigns when a scorer's own result — or its calibrated risk — fails the
// sanity checks in validateProbs/isValidRisk below (design §4.4: a
// malformed result must be "unscored", never silently folded into a real
// score). This is a Combine-assigned code, unlike "cost_cap"/"timeout"/
// "backoff", which the worker assigns to RuleOutcome before Combine ever
// sees it.
const ErrorCodeInvalidResult = "invalid_result"

// capEpsilon keeps a capped text-only risk strictly below tiers.Medium
// rather than exactly at it, so "cannot raise score above medium" (design
// §5) is a strict, testable inequality rather than a boundary a floating
// point comparison could accidentally cross.
const capEpsilon = 1e-6

// RuleState is one rule's static config plus whatever the caller already
// knows about its most recent verdict — the minimum a pure function needs
// to implement input-hash skipping and a rule-referencing stage condition
// without reaching into a store itself.
type RuleState struct {
	Rule config.Rule
	// LastInputHash is the InputHash recorded on the rule's most recent
	// verdict, or "" if it has never been scored. When this round's
	// computed hash matches, Plan marks the resulting Call to be skipped
	// with reason "input_unchanged" (design §4.7: "unchanged inputs reuse
	// the stored verdict").
	LastInputHash string
	// LastRisk is the calibrated risk recorded on the rule's most recent
	// scored verdict, or nil if it has never been scored. Consulted by
	// another rule's `stage: {min_local_risk: ...}` condition (design
	// §4.5's example gates a shadow rule on the local rule's risk).
	LastRisk *float64
	// ScorerVersion is model.Scorer.Version() for this rule's scorer at
	// Plan time (S2). Folded into the input hash so a scorer upgrade
	// (new weights, a vendor rotating its model) always forces a
	// rescore, even when every feature value is unchanged. The caller
	// (the worker, from S2 of the v0 plan onward) looks this up from the
	// registry before building []RuleState; "" is fine for a caller that
	// doesn't track it yet, and simply omits this from the hash.
	ScorerVersion string
	// CalibrationID is the id of the calibration map currently on record
	// for (rule, scorer, checkpoint) — "none" for an already-calibrated
	// scorer with no map. Folded into the input hash so a newly fitted
	// calibration forces a rescore instead of reusing a pre-calibration
	// verdict.
	CalibrationID string
}

// Call is one (rule, scorer, request) Plan has decided to make, or
// explicitly decided to skip. InputHash is set either way, so a caller
// that does make the call can record it on the resulting verdict for the
// next round's skip check.
type Call struct {
	Rule      config.Rule
	Request   model.ScoreRequest
	InputHash string
	// Skip, when true, means the caller should not invoke the scorer —
	// SkipReason says why. A skipped rule that was previously scored
	// keeps its prior verdict (the caller's job, e.g. the worker copying
	// the stored risk forward into this round's RuleOutcome).
	Skip       bool
	SkipReason SkipReason
}

// SkipReason enumerates why Plan skipped a call.
type SkipReason string

const (
	SkipNone           SkipReason = ""
	SkipStageCondition SkipReason = "stage_condition"
	SkipInputUnchanged SkipReason = "input_unchanged"
)

// Plan decides, purely, which calls to make for one subject's rule set
// given its current feature vector and text inputs (design §4.7).
//
//   - features holds every feature name any rule might reference (from
//     internal/feature, S2); a rule's own `inputs` selects a subset.
//   - text maps a text-feature name (e.g. "subject_line_skeleton") to the
//     list of values seen for it; a rule's `text` field selects which
//     named entries to concatenate, in order, into ScoreRequest.Text.
//   - rules carries each rule's config plus enough history
//     (LastInputHash, LastRisk) to decide staging and skipping.
//
// Plan never mutates its inputs and never calls a Scorer.
func Plan(features map[string]float64, text map[string][]string, rules []RuleState) []Call {
	maxLocalRisk, haveLocalRisk := maxRiskByScorer(rules, "local")

	calls := make([]Call, 0, len(rules))
	for _, rs := range rules {
		r := rs.Rule
		req := model.ScoreRequest{Labels: r.Labels}
		if len(r.Inputs) > 0 {
			req.Features = subsetFeatures(features, r.Inputs)
		}
		if len(r.Text) > 0 {
			req.Text = collectText(text, r.Text)
		}

		call := Call{
			Rule:      r,
			Request:   req,
			InputHash: inputHash(rs, req),
		}

		if skip := stageSkip(r, features, maxLocalRisk, haveLocalRisk); skip {
			call.Skip = true
			call.SkipReason = SkipStageCondition
			calls = append(calls, call)
			continue
		}
		if rs.LastInputHash != "" && rs.LastInputHash == call.InputHash {
			call.Skip = true
			call.SkipReason = SkipInputUnchanged
			calls = append(calls, call)
			continue
		}

		calls = append(calls, call)
	}
	return calls
}

func maxRiskByScorer(rules []RuleState, scorer string) (max float64, have bool) {
	for _, rs := range rules {
		if rs.Rule.Scorer != scorer || rs.LastRisk == nil {
			continue
		}
		if !have || *rs.LastRisk > max {
			max = *rs.LastRisk
			have = true
		}
	}
	return max, have
}

// stageSkip evaluates the `stage` conditions internal/config's loader
// already validated are one of the known keys:
//
//   - min_local_risk: run only once the highest risk among rules scored
//     by the "local" scorer has reached this value (design §4.5's
//     example: `new_account_velocity_jev` only fires after
//     `new_account_velocity` itself looks suspicious).
//   - max_subject_age_h / min_subject_age_h: run only while
//     features["subject_age_h"] is within the given bound (design's
//     `lure_similarity` stops looking at accounts older than a week).
//
// A rule with no `stage` is never skipped by this function.
func stageSkip(r config.Rule, features map[string]float64, maxLocalRisk float64, haveLocalRisk bool) bool {
	if len(r.Stage) == 0 {
		return false
	}
	if v, ok := r.Stage["min_local_risk"]; ok {
		if !haveLocalRisk || maxLocalRisk < v {
			return true
		}
	}
	if v, ok := r.Stage["max_subject_age_h"]; ok {
		if features["subject_age_h"] > v {
			return true
		}
	}
	if v, ok := r.Stage["min_subject_age_h"]; ok {
		if features["subject_age_h"] < v {
			return true
		}
	}
	return false
}

func subsetFeatures(features map[string]float64, names []string) map[string]float64 {
	out := make(map[string]float64, len(names))
	for _, n := range names {
		out[n] = features[n] // 0 if absent — see internal/model/local's doc comment on missing features
	}
	return out
}

func collectText(text map[string][]string, names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, text[n]...)
	}
	return out
}

// inputHash is a stable digest of everything that would change this
// call's answer (S2): the rule identity, its scorer AND the scorer's own
// current version/checkpoint (so a scorer upgrade is never masked by
// identical features), the render template version, the calibration map
// currently on record, the benign label (flipping which label counts as
// "not abusive" changes risk polarity even with an unchanged label SET),
// and the resolved request — with its Features quantized first (R7 round
// 2, see quantizeAgeFeaturesForHash). json.Marshal serializes map keys in
// sorted order, so the digest doesn't depend on Go's randomized map
// iteration.
func inputHash(rs RuleState, req model.ScoreRequest) string {
	payload := struct {
		Rule          string
		Scorer        string
		ScorerVersion string
		RenderVersion string
		CalibrationID string
		BenignLabel   string
		Labels        []string
		Features      map[string]float64
		Text          []string
	}{
		rs.Rule.Name, rs.Rule.Scorer, rs.ScorerVersion, req.RenderVersion, rs.CalibrationID, rs.Rule.BenignLabel,
		req.Labels, quantizeAgeFeaturesForHash(req.Features), req.Text,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		// B2/S2: NEVER fall back to a constant hash on marshal failure — a
		// constant fallback (e.g. hashing just the rule+scorer name) made
		// every future round with the same broken feature (e.g. a NaN,
		// which json.Marshal refuses to encode) look "unchanged" forever,
		// silently freezing the verdict on "input_unchanged" skips.
		// Instead: return the empty string, which Plan's skip check
		// (`rs.LastInputHash != "" && ...`) can never match against a
		// previously-recorded non-empty hash, and which two consecutive
		// failed-to-hash rounds correctly never skip against each other
		// either (a never-scored rule's LastInputHash is also "" and is
		// excluded by that same guard) — the safe direction regardless of
		// why marshaling failed.
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// quantizeAgeFeaturesForHash returns a copy of features with
// subject_age_h floored to a 1-hour bucket and upgrade_delay_min floored
// to a 60-minute bucket before hashing (R7 round 2). Proven necessary:
// both are continuously-drifting elapsed-time features for any subject
// not yet at internal/feature's clamp ceiling (B5 fix round) — they
// change on literally every tick even with no new event at all — so
// hashing them at full precision meant a rule that reads either one
// (design's shipped new_account_velocity does) never repeated its input
// hash across two consecutive rounds, permanently defeating
// SkipInputUnchanged (S6/S7) for exactly new, still-under-clamp accounts,
// the population this system most needs to score efficiently. A value
// already AT its clamp ceiling is already a whole-bucket multiple (24 and
// 1440 both divide evenly), so clamped values are untouched by the floor.
//
// Every other feature passes through unchanged: only these two specific,
// known-continuously-drifting names get bucketed — a feature genuinely
// changing in value (e.g. resource_total incrementing) must still change
// the hash exactly as before.
func quantizeAgeFeaturesForHash(features map[string]float64) map[string]float64 {
	if len(features) == 0 {
		return features
	}
	out := make(map[string]float64, len(features))
	for k, v := range features {
		switch k {
		case "subject_age_h":
			v = math.Floor(v)
		case "upgrade_delay_min":
			v = math.Floor(v/60) * 60
		}
		out[k] = v
	}
	return out
}

// RuleOutcome is one rule's resolved result for a scoring round: either a
// ScoreResult (freshly computed, or carried forward from a skipped call
// per design §4.7), or an explicit unscored status with a code. Combine
// is pure — resolving a Call into a RuleOutcome (actually calling the
// scorer, reusing a prior verdict on a skip, or recording why scoring
// failed) is the caller's job (the worker, S2; a test, here).
type RuleOutcome struct {
	Rule config.Rule
	// Result is non-nil exactly when the rule was scored, this round or a
	// prior one (a skip that carries forward a stored verdict still
	// produces a Result here — Combine cannot tell scored-now from
	// reused-from-skip, by design: both contribute to score the same
	// way).
	Result *model.ScoreResult
	// Unscored, when true, means Result is ignored (nil or not) and the
	// rule contributes only a Status:"unscored" signal.
	Unscored bool
	// ErrorCode is set when Unscored: e.g. "cost_cap", "timeout",
	// "backoff" (worker-assigned, S2).
	ErrorCode string
	// Reason is a pre-rendered explanation (internal/model's template
	// explainer, S2/S4) — Combine only forwards it onto the signal.
	Reason string
	// Calibration is accepted for the caller's own bookkeeping but is NOT
	// consulted by Combine (R7, round 2): the id actually recorded on the
	// signal always comes from whichever CalibrationSet entry matched (or
	// the literal "none" if no entry matched), never from this field — a
	// caller-supplied id could be wrong or stale (e.g. copied forward
	// from a previous round) in a way Combine has no way to verify.
	Calibration string
}

// Signal is one rule's contribution to a Verdict, matching the shape of
// the `signals` array in the GET /v1/subjects/{subject} response (design
// §4.4), minus HTTP-layer concerns (added in S3).
type Signal struct {
	Rule        string
	Mode        config.Mode
	Status      string // "scored" | "unscored"
	Risk        float64
	Flagged     bool
	Model       string
	Checkpoint  string
	Calibration string
	Reason      string
	ErrorCode   string
}

// Verdict is Combine's pure output for one subject's scoring round.
type Verdict struct {
	// Score is the max risk over scored advise rules (shadow excluded),
	// after the text_rules_need_feature_support cap. 0 when no advise
	// rule scored.
	Score float64
	// Tier is "unknown" (fewer than MinScoredAdvise advise rules scored),
	// "low", "medium", or "high" by Params.Tiers's cut points.
	Tier string
	// Degraded is true whenever any advise rule is unscored (design
	// §4.4). Shadow rules being unscored does not set this.
	Degraded bool
	Signals  []Signal
}

// CombineParams are Combine's non-per-rule inputs: the pieces of a loaded
// config.Config that determine scoring but aren't part of any one rule.
// Kept as its own small struct (rather than taking a *config.Config
// directly) so Combine stays trivially constructible in tests without a
// full Load call.
type CombineParams struct {
	Tiers                       config.Tiers
	MinScoredAdvise             int
	TextRulesNeedFeatureSupport bool
}

// Calibrator adjusts a scorer's raw risk (1 - P(benign)) to a calibrated
// one. See CalibrationSet.
type Calibrator interface {
	Calibrate(raw float64) float64
}

// CalibratorFunc adapts a plain function to a Calibrator.
type CalibratorFunc func(raw float64) float64

func (f CalibratorFunc) Calibrate(raw float64) float64 { return f(raw) }

// CalibrationEntry pairs a Calibrator with the id design §4.4/§4.10
// records on every verdict ("calibration":"cal_7f", or "none"). Combine
// always records the id from the SAME lookup whose Calibrator it actually
// applied (S12) — never a caller-supplied id that might not match — so a
// verdict's recorded calibration can never point at a map that wasn't the
// one used to produce its risk.
type CalibrationEntry struct {
	ID         string
	Calibrator Calibrator
}

// CalibrationSet looks up a CalibrationEntry by (rule, scorer, checkpoint)
// — design §4.6: "a map per (rule, scorer, checkpoint)". A triple with no
// entry records the literal calibration id "none" (R7, round 2 — never a
// caller-supplied fallback): internal/config's loader already requires
// every uncalibrated scorer to have a calibration on record before its
// rule can run at all, so by the time Combine runs, "no entry for this
// exact checkpoint" only ever means the scorer's own probabilities were
// already vendor-calibrated (design §4.6's Capabilities.Calibrated) or
// that the checkpoint just rolled and a new map hasn't been recorded yet.
type CalibrationSet map[string]CalibrationEntry

// Key builds the CalibrationSet lookup key for (ruleName, scorer,
// checkpoint). Exported so callers building a CalibrationSet don't have
// to guess the separator.
func Key(ruleName, scorer, checkpoint string) string {
	return ruleName + "/" + scorer + "/" + checkpoint
}

func (cs CalibrationSet) lookup(ruleName, scorer, checkpoint string) (CalibrationEntry, bool) {
	e, ok := cs[Key(ruleName, scorer, checkpoint)]
	return e, ok
}

// validateProbs checks a scored result's raw probabilities against design
// §4.6's contract (mirrored by internal/model/contract_test.go): every
// requested label present, every value finite and in [0,1], the benign
// label present, and the values summing to 1±0.01. It returns the raw
// risk (1 - P(benign)) and ok=true only when every check passes — B2:
// a NaN/Inf probability, one outside [0,1] (e.g. 1.7), a missing benign
// label, or a bad sum must never reach Combine's scoring math silently.
func validateProbs(probs map[string]float64, labels []string, benignLabel string) (raw float64, ok bool) {
	if len(labels) == 0 || benignLabel == "" {
		return 0, false
	}
	var sum float64
	haveBenign := false
	for _, l := range labels {
		v, present := probs[l]
		if !present || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
			return 0, false
		}
		sum += v
		if l == benignLabel {
			haveBenign = true
		}
	}
	if !haveBenign {
		return 0, false
	}
	if math.Abs(sum-1) > 0.01 {
		return 0, false
	}
	return 1 - probs[benignLabel], true
}

// Combine reduces a scoring round's outcomes to a Verdict (design §4.4 /
// §4.7):
//
//   - Each scored rule's risk is `1 - P(benign_label)`, calibrated if a
//     Calibrator is on record for (rule, scorer); `flagged` is that risk
//     compared against the rule's own threshold (per-rule thresholds
//     never affect tier, only `flagged`).
//   - `score` is the max risk over scored **advise** rules; shadow rules
//     are excluded from score entirely, though they still appear as
//     signals.
//   - A scored advise rule with `IsTextOnly()` true (Text set — S15,
//     round 2: regardless of whether Inputs is ALSO set) has its
//     contribution to `score` (not to its own reported `risk` or
//     `flagged`) capped just under Params.Tiers.Medium when
//     Params.TextRulesNeedFeatureSupport is set and no feature-based
//     (IsTextOnly() false) advise rule has itself reached `Medium` this
//     round (design §5: "a text rule alone cannot raise score above
//     medium unless a feature rule is >= medium").
//   - `tier` is "unknown" when fewer than Params.MinScoredAdvise advise
//     rules were scored; otherwise it's "high"/"medium"/"low" by
//     Params.Tiers.
//   - `degraded` is true whenever any advise rule is unscored (shadow
//     rules being unscored does not set it).
//
// Combine never calls a Scorer and never mutates outcomes.
func Combine(outcomes []RuleOutcome, params CombineParams, calib CalibrationSet) Verdict {
	signals := make([]Signal, 0, len(outcomes))
	degraded := false

	type adviseRisk struct {
		risk     float64
		textOnly bool
	}
	var adviseScored []adviseRisk

	for _, o := range outcomes {
		sig := Signal{Rule: o.Rule.Name, Mode: o.Rule.Mode}

		if o.Unscored || o.Result == nil {
			sig.Status = "unscored"
			sig.ErrorCode = o.ErrorCode
			signals = append(signals, sig)
			if o.Rule.Mode == config.ModeAdvise {
				degraded = true
			}
			continue
		}

		// B2: a malformed result (NaN/Inf, out-of-[0,1], missing benign
		// label, or a bad sum) is never scored — it fails open into
		// "unscored", the same as an adapter error or a budget cap, never
		// into a silent "low"/"high".
		raw, validResult := validateProbs(o.Result.Probs, o.Rule.Labels, o.Rule.BenignLabel)
		if !validResult {
			sig.Status = "unscored"
			sig.ErrorCode = ErrorCodeInvalidResult
			signals = append(signals, sig)
			if o.Rule.Mode == config.ModeAdvise {
				degraded = true
			}
			continue
		}

		risk := raw
		// R7 (round 2): "none" unless a CalibrationSet entry actually
		// matched — never the caller's own o.Calibration as a fallback.
		// The caller's field can be wrong or stale (e.g. copied forward
		// from a previous round), and by the time Combine runs, "no entry
		// for this exact (rule, scorer, checkpoint)" only ever means the
		// scorer's own probabilities were already vendor-calibrated
		// (design §4.6's Capabilities.Calibrated) — the correct recorded
		// id for that case is always "none", regardless of what the
		// caller happened to pass.
		calibrationID := "none"
		if entry, ok := calib.lookup(o.Rule.Name, o.Rule.Scorer, o.Result.Checkpoint); ok {
			// S12: the id recorded on the signal always comes from this
			// SAME lookup — never from a caller-supplied id that might not
			// be the map actually applied here.
			risk = entry.Calibrator.Calibrate(risk)
			calibrationID = entry.ID
		}
		// R9 (round 2): a calibrator returning ±Inf is unscored/
		// invalid_result, not clamped — clamping a genuinely infinite
		// value (unlike a finite but out-of-range one, still clamped
		// below) would mask a real calibrator bug behind a plausible-
		// looking boundary score.
		if math.IsNaN(risk) || math.IsInf(risk, 0) {
			sig.Status = "unscored"
			sig.ErrorCode = ErrorCodeInvalidResult
			signals = append(signals, sig)
			if o.Rule.Mode == config.ModeAdvise {
				degraded = true
			}
			continue
		}
		// Clamp (rather than reject) an out-of-range but finite calibrated
		// value — a calibrator overshooting slightly past [0,1] is a
		// calibration-quality problem to fix at the next `calibrate` run,
		// not a reason to drop the signal outright.
		if risk < 0 {
			risk = 0
		} else if risk > 1 {
			risk = 1
		}

		sig.Status = "scored"
		sig.Risk = risk
		sig.Flagged = risk >= o.Rule.Threshold
		sig.Model = o.Result.Model
		sig.Checkpoint = o.Result.Checkpoint
		sig.Calibration = calibrationID
		sig.Reason = o.Reason
		signals = append(signals, sig)

		if o.Rule.Mode == config.ModeAdvise {
			adviseScored = append(adviseScored, adviseRisk{risk: risk, textOnly: o.Rule.IsTextOnly()})
		}
	}

	scoredAdviseCount := len(adviseScored)

	var featureAdviseMax float64
	haveFeatureAdvise := false
	for _, a := range adviseScored {
		if a.textOnly {
			continue
		}
		if !haveFeatureAdvise || a.risk > featureAdviseMax {
			featureAdviseMax = a.risk
			haveFeatureAdvise = true
		}
	}
	featureSupport := haveFeatureAdvise && featureAdviseMax >= params.Tiers.Medium

	var score float64
	for _, a := range adviseScored {
		contribution := a.risk
		if a.textOnly && params.TextRulesNeedFeatureSupport && !featureSupport {
			capped := params.Tiers.Medium - capEpsilon
			if contribution > capped {
				contribution = capped
			}
		}
		if contribution > score {
			score = contribution
		}
	}

	// B2: CombineParams{} (a zero-value MinScoredAdvise) must never let
	// zero scored advise rules produce a real tier — MinScoredAdvise < 1
	// is treated as 1, exactly like internal/config.Load's own default,
	// so "no rule scored" can never satisfy "at least N scored".
	minScoredAdvise := params.MinScoredAdvise
	if minScoredAdvise < 1 {
		minScoredAdvise = 1
	}

	tier := "unknown"
	// R8 (round 2): a zero-value or otherwise invalid Tiers (e.g. High <=
	// Medium, a NaN/Inf/negative cut point) must never produce "high" or
	// "medium" — score is always >= 0, so an unvalidated Tiers{} (High=0)
	// made `score >= params.Tiers.High` true for any scored subject.
	// internal/config.Load already validates a loaded Tiers this
	// strictly; Combine is a pure function tests and other callers can
	// invoke directly, so it validates its own input rather than trusting
	// every caller to have gone through Load first.
	if validTiers(params.Tiers) && scoredAdviseCount >= minScoredAdvise {
		switch {
		case score >= params.Tiers.High:
			tier = "high"
		case score >= params.Tiers.Medium:
			tier = "medium"
		default:
			tier = "low"
		}
	}

	return Verdict{Score: score, Tier: tier, Degraded: degraded, Signals: signals}
}

// validTiers reports whether t is usable for cutting a score into a tier:
// both cut points finite, in (0,1], and High strictly greater than
// Medium. Mirrors internal/config.validateTiers's load-time checks so
// Combine never trusts an invalid Tiers just because it came from a
// caller that skipped Load (a test, or a future caller building
// CombineParams by hand).
func validTiers(t config.Tiers) bool {
	if math.IsNaN(t.Medium) || math.IsInf(t.Medium, 0) || t.Medium <= 0 || t.Medium > 1 {
		return false
	}
	if math.IsNaN(t.High) || math.IsInf(t.High, 0) || t.High <= 0 || t.High > 1 {
		return false
	}
	return t.High > t.Medium
}
