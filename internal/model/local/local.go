// Package local implements abusekit's deterministic, always-available
// scorer (design §4.6): a logistic model over named features with
// hand-set weights, no network call, and zero cost. It is the "advise
// rule of last resort" — every deployment registers it, and rules should
// generally always have at least one local-scored advise rule so a
// vendor outage never leaves a subject with tier "unknown" for long.
//
// local has no vendor SDK dependency, so it is exempt from the
// internal/model/<adapter>/-only import restriction, but it lives in its
// own package anyway to keep internal/model itself free of any concrete
// scorer implementation.
package local

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"

	"github.com/tokencanopy/abusekit/internal/model"
)

// Version is recorded as ScoreResult.Checkpoint so a verdict pins exactly
// which weight set produced it. Bump it whenever Weights.Version changes
// in a shipped config/local_weights.yaml.
const defaultCheckpoint = "v1"

// Weights is the local scorer's logistic model, loaded from
// config/local_weights.yaml. Every field here is a v0 placeholder: hand-
// set to a plausible sign and magnitude per feature, not fit against
// labelled data. They are reviewed like code and are expected to be
// replaced by the first `abusekit calibrate`/gate run (design §4.6, open
// question 5).
type Weights struct {
	// Version identifies this weight set; recorded as the scorer's
	// checkpoint on every verdict.
	Version string `yaml:"version"`
	// BenignLabel is the label this scorer treats as "not abusive". Score
	// requires it to be present in every request's Labels (see
	// Capabilities below).
	BenignLabel string `yaml:"benign_label"`
	// Bias is the logistic model's intercept.
	Bias float64 `yaml:"bias"`
	// Weight is the per-feature coefficient. A feature named in a rule's
	// `inputs` but absent from this map contributes 0 (documented, not
	// silently mysterious — see Score's doc comment).
	Weight map[string]float64 `yaml:"weights"`
}

// Validate reports whether w is usable: BenignLabel and at least one
// weight must be set. Called by New so a malformed weights file fails at
// construction, not on the first Score call.
func (w Weights) Validate() error {
	if w.BenignLabel == "" {
		return fmt.Errorf("local: weights.benign_label is required")
	}
	if len(w.Weight) == 0 {
		return fmt.Errorf("local: weights.weights must have at least one entry")
	}
	return nil
}

// Scorer is the local adapter. Construct with New; the zero value is not
// usable.
type Scorer struct {
	weights    Weights
	checkpoint string
}

// New builds the local Scorer from an already-parsed Weights value (see
// LoadWeightsFile to load one from YAML). Returns an error rather than
// panicking if w is invalid, per AGENTS.md's no-panic rule for library
// code.
func New(w Weights) (*Scorer, error) {
	if err := w.Validate(); err != nil {
		return nil, err
	}
	cp := w.Version
	if cp == "" {
		cp = defaultCheckpoint
	}
	return &Scorer{weights: w, checkpoint: cp}, nil
}

func (s *Scorer) Name() string { return "local" }

// Version returns a content hash of s's weights (S2), so
// internal/core's input-hash changes whenever the actual scoring math
// changes — including a weight VALUE edit that the deploying human forgot
// to reflect in local_weights.yaml's `version:` string. This is
// deliberately separate from Checkpoint (Weights.Version, recorded
// verbatim as ScoreResult.Checkpoint on every verdict): Checkpoint is the
// human-readable identifier design §4.4 shows in the API; Version is
// content-derived and exists purely to make a silent weights change
// impossible to mask via skip-if-unchanged.
func (s *Scorer) Version() string {
	b, err := json.Marshal(s.weights)
	if err != nil {
		// s.weights is plain string/float64/map data — this cannot fail in
		// practice. Fall back to the human checkpoint string rather than
		// an empty Version(), which core.Plan's inputHash treats as "could
		// not hash, never skip" if propagated — the safe direction either
		// way.
		return s.checkpoint
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Capabilities reports LabelMode as Open (design: "LabelMode: Open
// restricted to label sets containing the benign label") — Score enforces
// the "containing the benign label" restriction itself, since
// model.LabelMode's Open variant has no way to express that extra
// constraint generically.
func (s *Scorer) Capabilities() model.Capabilities {
	return model.Capabilities{
		LabelMode:       model.OpenLabelMode(),
		AcceptsText:     false,
		AcceptsFeatures: true,
		MaxTokens:       0,
		Calibrated:      true, // a hand-set logistic model reports its own probability, no map required
	}
}

// Policy reports that the local scorer never leaves the process: no
// retention beyond what the caller itself stores, no training, no text.
func (s *Scorer) Policy() model.DataPolicy {
	return model.DataPolicy{
		TermsVersion:   "n/a",
		RetainsInputs:  false,
		TrainsOnInputs: false,
		AllowsText:     false,
	}
}

// Score computes P(label) for every label in req.Labels from a single
// logistic score over req.Features:
//
//	linear   = bias + sum(weight[f] * features[f] for f in weights)
//	p(!benign) = sigmoid(linear)
//	p(benign)  = 1 - p(!benign)
//
// Features present in req.Features but absent from the weight map, and
// weights present but absent from req.Features, both contribute 0 — the
// caller (via internal/config's feature-name validation) is responsible
// for making sure a rule's `inputs` line up with what's in
// config/local_weights.yaml; Score itself never rejects a mismatch, since
// a newly added feature with no weight yet is a normal rollout state, not
// an error.
//
// The non-benign mass is split evenly across every other requested label.
// This is a deliberate v0 simplification: design §4.4 only ever reads
// `risk = 1 - P(benign)` off a signal, so how the remaining mass divides
// among e.g. "suspicious" vs "abusive" does not affect scoring — only a
// future rule that inspects a specific non-benign probability would need
// a real per-label split, which is out of scope for S1.
//
// Score honours ctx: it does no I/O, but still checks ctx.Err() first so
// a caller that raced a deadline against a busy scheduler gets a
// consistent "timeout -> error" contract across every adapter, local
// included.
func (s *Scorer) Score(ctx context.Context, req model.ScoreRequest) (model.ScoreResult, error) {
	if err := ctx.Err(); err != nil {
		return model.ScoreResult{}, err
	}
	if len(req.Text) > 0 {
		return model.ScoreResult{}, model.ErrTextNotAccepted
	}
	if !s.Capabilities().LabelMode.Accepts(req.Labels) {
		return model.ScoreResult{}, model.ErrUnknownLabel
	}
	if !containsLabel(req.Labels, s.weights.BenignLabel) {
		return model.ScoreResult{}, fmt.Errorf("%w: local requires benign label %q in the request's label set",
			model.ErrUnknownLabel, s.weights.BenignLabel)
	}

	linear := s.weights.Bias
	for feature, weight := range s.weights.Weight {
		linear += weight * req.Features[feature]
	}
	pNonBenign := sigmoid(linear)
	pBenign := 1 - pNonBenign

	others := make([]string, 0, len(req.Labels)-1)
	for _, l := range req.Labels {
		if l != s.weights.BenignLabel {
			others = append(others, l)
		}
	}
	sort.Strings(others) // deterministic iteration for deterministic splitting

	probs := make(map[string]float64, len(req.Labels))
	probs[s.weights.BenignLabel] = pBenign
	if len(others) > 0 {
		share := pNonBenign / float64(len(others))
		for _, l := range others {
			probs[l] = share
		}
	}

	return model.ScoreResult{
		Probs:      probs,
		Model:      s.Name(),
		Checkpoint: s.checkpoint,
		Render:     req.RenderVersion,
		LatencyMS:  0,
		CostMicro:  0,
		Truncated:  false,
	}, nil
}

func containsLabel(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}

// sigmoid is the standard logistic function, computed in the usual
// numerically stable form that avoids math.Exp overflowing for a
// large-magnitude positive x.
func sigmoid(x float64) float64 {
	if x >= 0 {
		z := math.Exp(-x)
		return 1 / (1 + z)
	}
	z := math.Exp(x)
	return z / (1 + z)
}
