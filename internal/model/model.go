// Package model defines abusekit's scorer seam (design §4.6): the types
// every adapter — deterministic or vendor-backed — speaks, a name-keyed
// registry, and the label-set/capability rules the config loader and the
// worker both depend on.
//
// This package must never import a vendor SDK. AGENTS.md and
// vendorlint_test.go enforce that vendor SDK imports live only under
// internal/model/<adapter>/ (e.g. a future internal/model/gemini): this
// package, and anything outside internal/model/*, may only depend on the
// types declared here.
package model

import (
	"context"
	"errors"
	"sort"
	"strings"
)

// LabelMode describes which label sets a Scorer can be asked to score
// against (design §4.6: "Open (any label set) | Fixed(set)").
//
// A zero LabelMode is neither Open nor Fixed and Accepts always returns
// false for it — adapters must construct one with Open() or Fixed(...).
type LabelMode struct {
	open  bool
	fixed []string // non-nil only when !open
}

// OpenLabelMode returns a LabelMode that accepts any label set a rule
// configures, as long as the set otherwise satisfies the scorer's own
// rules (e.g. the local scorer additionally requires its benign label to
// be present — see internal/model/local).
func OpenLabelMode() LabelMode { return LabelMode{open: true} }

// FixedLabelMode returns a LabelMode that accepts exactly one label set
// (order-independent), such as a vendor checkpoint trained against a
// specific taxonomy.
func FixedLabelMode(labels ...string) LabelMode {
	cp := make([]string, len(labels))
	copy(cp, labels)
	return LabelMode{fixed: cp}
}

// IsOpen reports whether m is an open label mode.
func (m LabelMode) IsOpen() bool { return m.open }

// FixedLabels returns the fixed label set, or nil if m is open.
func (m LabelMode) FixedLabels() []string { return m.fixed }

// Accepts reports whether labels is a valid request for m: any non-empty
// set for Open, or exactly the configured set (regardless of order) for
// Fixed. An empty zero-value LabelMode accepts nothing.
func (m LabelMode) Accepts(labels []string) bool {
	if len(labels) == 0 {
		return false
	}
	if m.open {
		return true
	}
	if m.fixed == nil {
		return false
	}
	if len(labels) != len(m.fixed) {
		return false
	}
	want := make(map[string]struct{}, len(m.fixed))
	for _, l := range m.fixed {
		want[l] = struct{}{}
	}
	for _, l := range labels {
		if _, ok := want[l]; !ok {
			return false
		}
		delete(want, l)
	}
	return len(want) == 0
}

// Equal reports whether m and other describe the same label set: both
// Open, or both Fixed with identical (order-independent) sets. Design
// §4.5's "vote(...) members with differing label sets" load-time check
// will use this once vote is reintroduced (see the TODO in
// internal/config's resolveScorer); TestLabelMode exercises it directly
// until then.
func (m LabelMode) Equal(other LabelMode) bool {
	if m.open != other.open {
		return false
	}
	if m.open {
		return true
	}
	return m.Accepts(other.fixed) && other.Accepts(m.fixed)
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

// Capabilities describes what a Scorer can be asked to do, and how much
// its answers can be trusted as-is (design §4.6).
type Capabilities struct {
	// LabelMode is the label set(s) the scorer accepts.
	LabelMode LabelMode
	// AcceptsText reports whether Score will use ScoreRequest.Text.
	AcceptsText bool
	// AcceptsFeatures reports whether Score will use
	// ScoreRequest.Features.
	AcceptsFeatures bool
	// MaxTokens bounds text input; 0 means "no declared limit". A scorer
	// that accepts text and enforces a limit reports Truncated in its
	// ScoreResult when it had to cut input to fit.
	MaxTokens int
	// Calibrated reports whether the scorer's own probabilities are
	// already meaningful (vendor-calibrated). false means the config
	// loader requires a recorded calibration map for this (rule, scorer)
	// pair before the rule may run (design §4.5/§4.6).
	Calibrated bool
}

// DataPolicy records what a scorer's vendor (or, for the local scorer,
// abusekit itself) is contractually allowed to do with the inputs it is
// sent, and under which recorded terms (design §4.6). config/vendors.yaml
// is the source of truth for these values in production; adapters report
// their own Policy() so the contract suite can assert the two agree.
type DataPolicy struct {
	TermsVersion   string
	RetainsInputs  bool
	TrainsOnInputs bool
	AllowsText     bool
}

// ScoreRequest is one scoring call's input.
type ScoreRequest struct {
	// Labels is the label set to score against; must satisfy the
	// scorer's Capabilities().LabelMode.Accepts.
	Labels []string
	// Features is the named feature vector (internal/feature, from S2).
	// Only consulted when Capabilities().AcceptsFeatures.
	Features map[string]float64
	// Text is the ordered list of text inputs (e.g. a rule's `text:`
	// fields, already redacted/skeletonized by internal/event). Only
	// consulted when Capabilities().AcceptsText.
	Text []string
	// Context is optional free-form context a renderer may prepend (e.g.
	// a template name); adapters that don't use it ignore it.
	Context string
	// RenderVersion identifies the template version used to turn
	// Features/Text into whatever the adapter actually sends over the
	// wire, so a verdict can be reproduced from its manifest.
	RenderVersion string
}

// ScoreResult is one scoring call's output.
type ScoreResult struct {
	// Probs maps each requested label to its probability; must sum to
	// 1±0.01 (contract-tested).
	Probs map[string]float64
	// Model, Checkpoint, Render identify what produced Probs, recorded on
	// every verdict (design §4.4/§4.10).
	Model, Checkpoint, Render string
	// LatencyMS is the call's observed latency.
	LatencyMS int
	// CostMicro is the call's cost in micro-units of USD (1e-6 USD); 0
	// for the local scorer.
	CostMicro int64
	// Truncated reports whether the adapter had to cut input (e.g. text
	// over MaxTokens) to produce Probs.
	Truncated bool
}

// Scorer is the seam every model — local or vendor — implements.
// Implementations must be safe for concurrent use.
type Scorer interface {
	// Name is the registry key and the value recorded as ScoreResult.Model
	// unless the implementation reports a more specific string.
	Name() string
	Capabilities() Capabilities
	Policy() DataPolicy
	// Version identifies the scorer's current weights/checkpoint, queryable
	// WITHOUT making a Score call (S2). internal/core's input-hash includes
	// it so a scorer upgrade — new weights deployed, a vendor rotating its
	// model — always forces a rescore, even when a subject's features
	// haven't changed. local derives this from a hash of its weights'
	// content (catching a weight edit even if the human-typed `version:`
	// string in local_weights.yaml wasn't bumped); a vendor adapter reports
	// whatever checkpoint identifier it is currently pinned to.
	Version() string
	// Score must respect ctx: returning promptly with ctx.Err() when ctx
	// is done or already expired, rather than blocking past its deadline.
	// It must never panic; any internal failure is returned as an error.
	Score(ctx context.Context, req ScoreRequest) (ScoreResult, error)
}

// Explainer optionally turns a ScoreResult into a human-readable reason.
// Per design §4.6, an LLM explainer receives features and probabilities
// only — never raw event text — and its output is stored separately
// (`llm_reason`, `untrusted: true`); that boundary is enforced by callers,
// not by this interface.
type Explainer interface {
	Name() string
	Policy() DataPolicy
	Explain(ctx context.Context, req ScoreRequest, res ScoreResult) (string, error)
}

// Sentinel errors adapters and the registry may return. Adapters are
// encouraged to wrap these with errors.Wrap-style context rather than
// inventing parallel sentinels, so callers can errors.Is against one set.
var (
	// ErrUnknownLabel means the ScoreRequest's Labels were not accepted by
	// the scorer's Capabilities().LabelMode.
	ErrUnknownLabel = errors.New("model: label set not accepted by scorer")
	// ErrTextNotAccepted means Text was supplied but
	// Capabilities().AcceptsText is false.
	ErrTextNotAccepted = errors.New("model: scorer does not accept text input")
	// ErrFeaturesNotAccepted means Features was supplied but
	// Capabilities().AcceptsFeatures is false.
	ErrFeaturesNotAccepted = errors.New("model: scorer does not accept feature input")
)
