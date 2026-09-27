package model

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// voteScorer is the "vote(a,b,…)" combinator from design §4.6: it scores
// every member with the same request and reports the mean of their
// (already-calibrated, by the time they reach here) risks per label.
type voteScorer struct {
	name    string
	members []Scorer
	caps    Capabilities
	policy  DataPolicy
}

// Vote builds a combined Scorer over two or more members that share an
// identical label mode (design §4.5: "vote(...) members with differing
// label sets" is a config-load error — Vote returns that error here so
// both the config loader and direct callers get the same check).
//
// The combined Capabilities are the conservative intersection of the
// members': it accepts text/features only if every member does (so
// Score never forwards a field a member would reject), and reports
// Calibrated only if every member's own probabilities are already
// meaningful — a vote containing one uncalibrated member still needs an
// external calibration map for the vote as a whole.
func Vote(name string, members ...Scorer) (Scorer, error) {
	if len(members) < 2 {
		return nil, fmt.Errorf("model: vote %q needs at least two members, got %d", name, len(members))
	}
	first := members[0].Capabilities()
	acceptsText, acceptsFeatures, calibrated := true, true, true
	minTokens := 0
	for _, m := range members {
		c := m.Capabilities()
		if !c.LabelMode.Equal(first.LabelMode) {
			return nil, fmt.Errorf("model: vote %q members have differing label sets: %s vs %s",
				name, first.LabelMode.String(), c.LabelMode.String())
		}
		acceptsText = acceptsText && c.AcceptsText
		acceptsFeatures = acceptsFeatures && c.AcceptsFeatures
		calibrated = calibrated && c.Calibrated
		if c.MaxTokens > 0 && (minTokens == 0 || c.MaxTokens < minTokens) {
			minTokens = c.MaxTokens
		}
	}

	allowsText, retains, trains := true, false, false
	terms := make([]string, 0, len(members))
	for _, m := range members {
		p := m.Policy()
		allowsText = allowsText && p.AllowsText
		retains = retains || p.RetainsInputs
		trains = trains || p.TrainsOnInputs
		terms = append(terms, m.Name()+":"+p.TermsVersion)
	}

	return &voteScorer{
		name:    name,
		members: append([]Scorer(nil), members...),
		caps: Capabilities{
			LabelMode:       first.LabelMode,
			AcceptsText:     acceptsText,
			AcceptsFeatures: acceptsFeatures,
			MaxTokens:       minTokens,
			Calibrated:      calibrated,
		},
		policy: DataPolicy{
			TermsVersion:   strings.Join(terms, ","),
			RetainsInputs:  retains,
			TrainsOnInputs: trains,
			AllowsText:     allowsText,
		},
	}, nil
}

func (v *voteScorer) Name() string               { return v.name }
func (v *voteScorer) Capabilities() Capabilities { return v.caps }
func (v *voteScorer) Policy() DataPolicy         { return v.policy }

// Score calls every member with req, in order, stopping at the first
// error (which it wraps with the failing member's name) or the first
// context cancellation. On success it returns the per-label mean of the
// members' Probs, defensively renormalized to sum to 1, with CostMicro
// summed and Truncated set if any member truncated.
func (v *voteScorer) Score(ctx context.Context, req ScoreRequest) (ScoreResult, error) {
	if !v.caps.LabelMode.Accepts(req.Labels) {
		return ScoreResult{}, ErrUnknownLabel
	}
	if len(req.Text) > 0 && !v.caps.AcceptsText {
		return ScoreResult{}, ErrTextNotAccepted
	}
	if len(req.Features) > 0 && !v.caps.AcceptsFeatures {
		return ScoreResult{}, ErrFeaturesNotAccepted
	}

	start := time.Now()
	sums := make(map[string]float64, len(req.Labels))
	var totalCost int64
	truncated := false
	names := make([]string, 0, len(v.members))
	checkpoints := make([]string, 0, len(v.members))

	for _, m := range v.members {
		if err := ctx.Err(); err != nil {
			return ScoreResult{}, err
		}
		res, err := m.Score(ctx, req)
		if err != nil {
			return ScoreResult{}, fmt.Errorf("model: vote %q member %q: %w", v.name, m.Name(), err)
		}
		for _, label := range req.Labels {
			sums[label] += res.Probs[label]
		}
		totalCost += res.CostMicro
		truncated = truncated || res.Truncated
		names = append(names, m.Name())
		checkpoints = append(checkpoints, res.Checkpoint)
	}

	n := float64(len(v.members))
	probs := make(map[string]float64, len(sums))
	var total float64
	for label, sum := range sums {
		probs[label] = sum / n
		total += probs[label]
	}
	// Defensive renormalization against floating-point drift so the
	// contract's "sums to 1±0.01" holds even if a member's own Probs were
	// already slightly off.
	if total > 0 {
		for label := range probs {
			probs[label] /= total
		}
	}

	return ScoreResult{
		Probs:      probs,
		Model:      v.name,
		Checkpoint: strings.Join(checkpoints, ","),
		Render:     req.RenderVersion,
		LatencyMS:  int(time.Since(start).Milliseconds()),
		CostMicro:  totalCost,
		Truncated:  truncated,
	}, nil
}

// String gives a stable, human-readable rendering of a LabelMode for
// error messages (e.g. "fixed[abusive,benign]" or "open").
func (m LabelMode) String() string {
	if m.open {
		return "open"
	}
	sorted := append([]string(nil), m.fixed...)
	sort.Strings(sorted)
	return "fixed[" + strings.Join(sorted, ",") + "]"
}
