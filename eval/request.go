package eval

import (
	"context"
	"math"

	"github.com/tokencanopy/abusekit/internal/config"
	"github.com/tokencanopy/abusekit/internal/model"
)

// buildRequest resolves rule's `inputs`/`text` against pt, mirroring
// internal/core.Plan's subsetFeatures/collectText — duplicated here
// (rather than imported) because Plan's version is keyed to a
// core.RuleState (skip/staging bookkeeping the harness has no use for);
// the actual selection logic is the same three lines either way.
func buildRequest(rule config.Rule, pt Point, promptVersion string) model.ScoreRequest {
	req := model.ScoreRequest{Labels: rule.Labels, RenderVersion: promptVersion, Context: pt.Context}
	if len(rule.Inputs) > 0 {
		req.Features = make(map[string]float64, len(rule.Inputs))
		for _, name := range rule.Inputs {
			req.Features[name] = pt.Features[name] // 0 if absent, same convention as internal/model/local's Score
		}
	}
	if len(rule.Text) > 0 {
		req.Text = make([]string, 0, len(rule.Text))
		for _, name := range rule.Text {
			req.Text = append(req.Text, pt.Text[name]...)
		}
	}
	return req
}

// scoreOne calls scorer once for pt against rule, returning the
// calibrated, clamped risk and its flagged verdict, or unscored=true with
// an errCode when the result fails validation. callErr is non-nil only
// when scorer.Score itself returned an error (a real scorer/adapter
// failure, as opposed to a structurally invalid but successfully
// returned result).
func scoreOne(ctx context.Context, rule config.Rule, scorer model.Scorer, opts Options, pt Point) (risk float64, flagged bool, unscored bool, res model.ScoreResult, errCode string, callErr error) {
	req := buildRequest(rule, pt, opts.promptVersion())
	res, err := scorer.Score(ctx, req)
	// Fix round nit: "take latency from the scorer when it reports one;
	// keep it deterministic otherwise" — a wall-clock measurement here
	// (the original S4 landing's approach) is nondeterministic in
	// principle even when it happens to round to 0ms in practice, which
	// would silently threaten determinism_test.go's byte-identical-
	// apart-from-`at` guarantee. res.LatencyMS is used exactly as the
	// scorer reported it: 0 for the local scorer (no I/O to time at all),
	// a live adapter's own measured figure otherwise.
	if err != nil {
		return 0, false, true, res, "scorer_error", err
	}

	raw, ok := validateProbs(res.Probs, rule.Labels, rule.BenignLabel)
	if !ok {
		return 0, false, true, res, "invalid_result", nil
	}
	risk = raw
	if opts.Calibration != nil {
		risk = opts.Calibration.Calibrate(risk)
	}
	if math.IsNaN(risk) || math.IsInf(risk, 0) {
		return 0, false, true, res, "invalid_result", nil
	}
	if risk < 0 {
		risk = 0
	} else if risk > 1 {
		risk = 1
	}
	return risk, risk >= rule.Threshold, false, res, "", nil
}

// validateProbs mirrors internal/core's unexported validateProbs
// (design §4.6's contract): every requested label present, every value
// finite and in [0,1], the benign label present, and the values summing
// to 1±0.01. Duplicated rather than imported because core doesn't export
// it — see internal/core/core.go's own doc comment on the function this
// mirrors.
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

// ScoreOne scores a single Point against rule using scorer, per opts
// (opts.Slice is ignored — pt is already resolved to whichever slice the
// caller cares about). It is the same per-point scoring path Run uses
// internally, exported for a caller (the `abusekit score --jsonl`
// subcommand — design §4.10) that wants to score one resolved input at a
// time without building a whole Dataset. The returned Verdict's
// Subject/Label are left zero; the caller fills them in from whatever
// identifies its own row.
func ScoreOne(ctx context.Context, rule config.Rule, scorer model.Scorer, opts Options, pt Point) (Verdict, model.ScoreResult, error) {
	risk, flagged, unscored, res, errCode, callErr := scoreOne(ctx, rule, scorer, opts, pt)
	if callErr != nil {
		return Verdict{Unscored: true, ErrorCode: "scorer_error"}, res, callErr
	}
	if unscored {
		return Verdict{Unscored: true, ErrorCode: errCode}, res, nil
	}
	return Verdict{Risk: risk, Flagged: flagged, Tier: tierFor(risk, opts.Tiers)}, res, nil
}

// tierFor buckets risk by tiers' cut points, mirroring
// internal/core.Combine's own tier logic. Returns "unknown" when tiers
// isn't a validly configured Tiers (the zero value included) — a caller
// that didn't pass Options.Tiers gets every Verdict.Tier as "unknown"
// rather than Run silently guessing at cut points.
func tierFor(risk float64, tiers config.Tiers) string {
	if !validTiers(tiers) {
		return "unknown"
	}
	switch {
	case risk >= tiers.High:
		return "high"
	case risk >= tiers.Medium:
		return "medium"
	default:
		return "low"
	}
}

// validTiers mirrors internal/config's own load-time validation (both
// cut points finite, in (0,1], High strictly greater than Medium) — see
// internal/core.validTiers's identical doc comment for why Run re-checks
// this itself rather than trusting every caller went through
// config.Load.
func validTiers(t config.Tiers) bool {
	if math.IsNaN(t.Medium) || math.IsInf(t.Medium, 0) || t.Medium <= 0 || t.Medium > 1 {
		return false
	}
	if math.IsNaN(t.High) || math.IsInf(t.High, 0) || t.High <= 0 || t.High > 1 {
		return false
	}
	return t.High > t.Medium
}
