// Package config loads and validates abusekit's two YAML documents
// (design §4.5): the rule set (tiers, cut points, per-rule scorer/inputs)
// and the vendor/adapter allowlist. "rule" is folded into this package
// rather than living on its own, because — per design §4.1 — it was
// always just YAML parsing.
//
// Load performs every check design §4.5 lists that's in S1's scope at
// load time and rejects the whole document on any single failure,
// collecting all of them into one error rather than stopping at the
// first: "unknown scorer, unknown feature, labels not accepted by the
// adapter's Capabilities, text inputs to an adapter whose policy forbids
// text, a (rule, scorer) pair with no calibration record ... -> the whole
// reload is rejected and the previous config stays live". (Design §4.5
// also lists "vote(...) members with differing label sets" — that check
// applies once vote(...) parsing is reintroduced in internal/core, per
// the TODO on resolveScorer below; S1 doesn't parse vote(...) at all.)
// Keeping the previous config live on a reject is the caller's job (S3's
// hot-reload loop); this package only decides accept/reject.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/tokencanopy/abusekit/internal/model"
)

// ruleNameRe enforces "rule names must match [a-z0-9_]+ (no /)" (S3): a
// rule name is also half of internal/core.Key's calibration lookup key
// (design §4.6: "(rule, scorer, checkpoint)", joined with "/"), so a
// rule name containing "/" could collide with a different (rule, scorer)
// pair's key.
var ruleNameRe = regexp.MustCompile(`^[a-z0-9_]+$`)

// Mode is a rule's contribution mode (design §2/§4.5).
type Mode string

const (
	ModeAdvise Mode = "advise"
	ModeShadow Mode = "shadow"
)

// Rule is one entry from the `rules:` list, fully resolved (any
// `inputs: {same_as: ...}` reference has already been substituted with
// the referenced rule's concrete input list).
type Rule struct {
	Name string
	Mode Mode
	// Scorer is the resolved scorer name as it appears in the YAML — a
	// plain registry name ("local") in S1 (see resolveScorer's TODO: a
	// future slice reintroduces a "vote(a,b,...)" expression naming other
	// registered scorers to combine). Use Config.ScorerFor(rule) to get
	// the constructed model.Scorer.
	Scorer      string
	Inputs      []string
	Text        []string
	Labels      []string
	BenignLabel string
	Threshold   float64
	// Stage holds arbitrary named staging conditions (design §4.5's
	// `stage: {min_local_risk: 0.3}` / `{max_subject_age_h: 168}`).
	// internal/core interprets the keys it knows; an unrecognized key is
	// a load-time error (see validateStage) so a typo never silently
	// no-ops.
	Stage map[string]float64
}

// IsTextOnly reports whether r needs text_rules_need_feature_support's cap
// (design §5: "a text rule alone cannot raise score above medium unless a
// feature rule is >= medium").
//
// S15 decision: ANY rule that declares `text` counts, regardless of
// whether it also declares `inputs`. The two options on the table were
// "no non-feature inputs at all" (the original, narrower definition) vs.
// "no non-feature inputs OR only inputs from an allowlist of onboarding
// facts" vs. this one — the reviewer's own "simplest" suggestion, chosen
// here: a rule mixing `text` with a trivial/permanent onboarding-fact
// input (e.g. `email_domain_class`, which never ages out and carries no
// risk signal on its own) previously escaped the cap entirely just by
// having ANY input at all, even though nothing about its contribution is
// genuinely feature-driven. Treating every text-declaring rule as needing
// independent feature support elsewhere in the batch is conservative in
// the safe direction: a rule that legitimately combines text with a
// strong feature signal is still uncapped as soon as feature support
// exists (Combine's featureAdviseMax check, unaffected by this), so the
// only rules this affects are ones whose only "feature" input was never
// going to reach Medium on its own anyway.
func (r Rule) IsTextOnly() bool { return len(r.Text) > 0 }

// Tiers holds the global score cut points (design §4.4).
type Tiers struct {
	Medium float64
	High   float64
}

// Config is the fully validated, load-time-resolved rule configuration.
type Config struct {
	Tiers                       Tiers
	MinScoredAdvise             int
	TextRulesNeedFeatureSupport bool
	Rules                       []Rule

	scorers map[string]model.Scorer // resolved per rule.Scorer, keyed by that string
}

// ScorerFor returns the constructed model.Scorer for rule (already
// resolved at Load time; S1 resolves only a plain registered name — see
// resolveScorer's TODO for vote(...) construction in a later slice).
func (c *Config) ScorerFor(rule Rule) (model.Scorer, bool) {
	s, ok := c.scorers[rule.Scorer]
	return s, ok
}

// FeatureSet is the set of feature names the config loader may reference
// in a rule's `inputs`. S1 has no feature implementations yet, so
// cmd/abusekit constructs one with just the names from design §4.5 ("a
// Registry of feature names is passed in (S2 fills it, S1 registers the
// names only)"); S2's internal/feature package becomes the real source.
type FeatureSet map[string]struct{}

// NewFeatureSet builds a FeatureSet from a list of names.
func NewFeatureSet(names ...string) FeatureSet {
	s := make(FeatureSet, len(names))
	for _, n := range names {
		s[n] = struct{}{}
	}
	return s
}

// Has reports whether name is a known feature.
func (s FeatureSet) Has(name string) bool {
	_, ok := s[name]
	return ok
}

// VendorEntry is one row of config/vendors.yaml: the adapter allowlist
// (design §4.6). The config loader refuses to use any scorer whose base
// adapter name is not listed here (in S1 that's always the rule's own
// `scorer:` name directly; once vote(...) returns, each of its members'
// names too).
type VendorEntry struct {
	Name         string
	TermsVersion string
	DPARef       string
	Policy       model.DataPolicy
}

// CalibrationLookup reports whether a recorded calibration map exists for
// (ruleName, scorerName). Dependencies.Calibrated defaults to a lookup
// that always returns false when nil, which is correct for S1 (no
// calibration store exists yet — see internal/store's `calibrations`
// table and the harness in a later slice) and simply means every rule
// using an uncalibrated scorer must be given one by the caller in tests.
type CalibrationLookup func(ruleName, scorerName string) bool

// Dependencies are the runtime inputs Load needs beyond the YAML bytes
// themselves: what scorers exist, what features exist, what vendors are
// allowlisted, and what calibrations are on record. Threading these in
// (rather than Load reaching into globals) is what makes Load a pure,
// table-testable function.
type Dependencies struct {
	Registry   *model.Registry
	Features   FeatureSet
	Vendors    map[string]VendorEntry
	Calibrated CalibrationLookup
}

func (d Dependencies) calibrated(rule, scorer string) bool {
	if d.Calibrated == nil {
		return false
	}
	return d.Calibrated(rule, scorer)
}

// --- YAML wire shapes -------------------------------------------------

type rawConfig struct {
	Tiers struct {
		Medium float64 `yaml:"medium"`
		High   float64 `yaml:"high"`
	} `yaml:"tiers"`
	MinScoredAdvise int `yaml:"min_scored_advise"`
	// TextRulesNeedFeatureSupport is a *bool (not bool) so Load can tell
	// "omitted from the YAML" from "explicitly set to false" (S3: this
	// defaults to TRUE — the safer setting — when omitted, not to Go's
	// bool zero value).
	TextRulesNeedFeatureSupport *bool     `yaml:"text_rules_need_feature_support"`
	Rules                       []rawRule `yaml:"rules"`
}

type rawRule struct {
	Name        string    `yaml:"name"`
	Mode        string    `yaml:"mode"`
	Scorer      string    `yaml:"scorer"`
	Inputs      yaml.Node `yaml:"inputs"`
	Text        []string  `yaml:"text"`
	Labels      []string  `yaml:"labels"`
	BenignLabel string    `yaml:"benign_label"`
	// Threshold is a *float64 (not float64) so Load can tell "omitted"
	// from "explicitly set to 0" (S3: threshold is required — a rule
	// that never sets one is a config mistake, not a rule that flags on
	// every request).
	Threshold *float64           `yaml:"threshold"`
	Stage     map[string]float64 `yaml:"stage"`
}

// Load parses and validates rule configuration YAML against deps,
// returning either a fully resolved Config or an error listing every
// problem found (errors.Join — use errors.Is/As or just print it; the
// individual failures are *ValidationIssue values if a caller wants to
// distinguish them programmatically).
func Load(data []byte, deps Dependencies) (*Config, error) {
	var raw rawConfig
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true) // S3: a typo'd or stale field name fails the load, not silently no-ops
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("config: parse rules yaml: %w", err)
	}

	minScoredAdvise := raw.MinScoredAdvise
	if minScoredAdvise == 0 {
		minScoredAdvise = 1
	}

	textRulesNeedFeatureSupport := true // S3: the safer default when omitted
	if raw.TextRulesNeedFeatureSupport != nil {
		textRulesNeedFeatureSupport = *raw.TextRulesNeedFeatureSupport
	}

	var issues []error

	if deps.Features == nil {
		// S3: nil silently meant "every feature check is skipped" (via a
		// `deps.Features != nil &&` guard below) — treat it as a load
		// error instead, so a caller that forgot to build a FeatureSet
		// finds out immediately rather than shipping a config where every
		// `inputs` reference was rubber-stamped.
		issues = append(issues, errors.New("config: Dependencies.Features must not be nil"))
	}

	issues = append(issues, validateTiers(raw.Tiers.Medium, raw.Tiers.High)...)

	rules, resolveIssues := resolveInputs(raw.Rules)
	issues = append(issues, resolveIssues...)

	adviseCount := 0
	for _, r := range rules {
		if r.Mode == ModeAdvise {
			adviseCount++
		}
	}
	if minScoredAdvise < 1 {
		issues = append(issues, fmt.Errorf("config: min_scored_advise (%d) must be >= 1", minScoredAdvise))
	} else if adviseCount == 0 {
		// R11 (round 2): zero advise rules at all means scoredAdviseCount
		// can never reach minScoredAdvise (>= 1) — every subject would be
		// tier=unknown forever. The previous check only compared
		// minScoredAdvise against adviseCount when adviseCount > 0,
		// silently accepting a config with no advise rules whatsoever.
		issues = append(issues, fmt.Errorf("config: min_scored_advise (%d) requires at least one advise-mode rule, but none are configured — every subject would stay tier=unknown forever", minScoredAdvise))
	} else if minScoredAdvise > adviseCount {
		issues = append(issues, fmt.Errorf("config: min_scored_advise (%d) must be <= the number of advise rules (%d)", minScoredAdvise, adviseCount))
	}

	seenNames := make(map[string]bool, len(rules))
	scorers := make(map[string]model.Scorer, len(rules))
	for i, r := range rules {
		if r.Name == "" {
			issues = append(issues, errors.New("config: a rule is missing `name`"))
			continue
		}
		if !ruleNameRe.MatchString(r.Name) {
			issues = append(issues, fmt.Errorf("config: rule name %q must match ^[a-z0-9_]+$ (lower-case letters, digits, underscore only — no \"/\")", r.Name))
		}
		if seenNames[r.Name] {
			issues = append(issues, fmt.Errorf("config: duplicate rule name %q", r.Name))
		}
		seenNames[r.Name] = true

		if r.Mode != ModeAdvise && r.Mode != ModeShadow {
			issues = append(issues, fmt.Errorf("config: rule %q has invalid mode %q (want %q or %q)", r.Name, r.Mode, ModeAdvise, ModeShadow))
		}

		if rawThreshold := raw.Rules[i].Threshold; rawThreshold == nil {
			issues = append(issues, fmt.Errorf("config: rule %q is missing threshold", r.Name))
		} else if math.IsNaN(*rawThreshold) || math.IsInf(*rawThreshold, 0) {
			issues = append(issues, fmt.Errorf("config: rule %q threshold must be a finite number, got %v", r.Name, *rawThreshold))
		} else if *rawThreshold < 0 || *rawThreshold > 1 {
			issues = append(issues, fmt.Errorf("config: rule %q threshold %v must be in [0,1]", r.Name, *rawThreshold))
		}

		if r.BenignLabel == "" {
			issues = append(issues, fmt.Errorf("config: rule %q is missing benign_label", r.Name))
		} else if !containsString(r.Labels, r.BenignLabel) {
			issues = append(issues, fmt.Errorf("config: rule %q benign_label %q is not in its labels %v", r.Name, r.BenignLabel, r.Labels))
		}
		issues = append(issues, validateNoDuplicatesOrEmpty(r.Name, "label", r.Labels)...)

		if len(r.Inputs) == 0 && len(r.Text) == 0 {
			issues = append(issues, fmt.Errorf("config: rule %q has neither inputs nor text", r.Name))
		}
		issues = append(issues, validateNoDuplicatesOrEmpty(r.Name, "input", r.Inputs)...)

		for _, f := range r.Inputs {
			if deps.Features != nil && !deps.Features.Has(f) {
				issues = append(issues, fmt.Errorf("config: rule %q references unknown feature %q", r.Name, f))
			}
		}

		if err := validateStage(r); err != nil {
			issues = append(issues, fmt.Errorf("config: rule %q: %w", r.Name, err))
		}

		scorer, memberNames, err := resolveScorer(r.Scorer, deps.Registry)
		if err != nil {
			issues = append(issues, fmt.Errorf("config: rule %q: %w", r.Name, err))
			continue
		}
		scorers[r.Scorer] = scorer

		caps := scorer.Capabilities()
		if len(r.Labels) > 0 && !caps.LabelMode.Accepts(r.Labels) {
			issues = append(issues, fmt.Errorf("config: rule %q labels %v not accepted by scorer %q's label mode (%s)",
				r.Name, r.Labels, r.Scorer, caps.LabelMode.String()))
		}
		if len(r.Text) > 0 && !caps.AcceptsText {
			issues = append(issues, fmt.Errorf("config: rule %q sends text to scorer %q, which does not accept text", r.Name, r.Scorer))
		}
		if len(r.Inputs) > 0 && !caps.AcceptsFeatures {
			issues = append(issues, fmt.Errorf("config: rule %q sends feature inputs to scorer %q, which does not accept features", r.Name, r.Scorer))
		}

		for _, member := range memberNames {
			vendor, ok := deps.Vendors[member]
			if !ok {
				issues = append(issues, fmt.Errorf("config: rule %q uses scorer %q, whose adapter %q is not in the vendor allowlist", r.Name, r.Scorer, member))
				continue
			}
			if len(r.Text) > 0 && !vendor.Policy.AllowsText {
				issues = append(issues, fmt.Errorf("config: rule %q sends text to adapter %q, whose recorded policy forbids text", r.Name, member))
			}
			// S9: the adapter's own reported Policy() must agree with what
			// vendors.yaml records for it — a stale or hand-edited
			// allowlist entry must never silently diverge from what the
			// adapter actually does with its inputs.
			if deps.Registry != nil {
				if memberScorer, ok := deps.Registry.Get(member); ok {
					if p := memberScorer.Policy(); p != vendor.Policy {
						issues = append(issues, fmt.Errorf(
							"config: adapter %q's reported policy (%+v) does not match vendors.yaml's recorded policy (%+v)",
							member, p, vendor.Policy))
					}
				}
			}
		}

		if !caps.Calibrated && !deps.calibrated(r.Name, r.Scorer) {
			issues = append(issues, fmt.Errorf("config: rule %q uses uncalibrated scorer %q with no recorded calibration", r.Name, r.Scorer))
		}
	}

	if len(issues) > 0 {
		return nil, errors.Join(issues...)
	}

	return &Config{
		Tiers:                       Tiers{Medium: raw.Tiers.Medium, High: raw.Tiers.High},
		MinScoredAdvise:             minScoredAdvise,
		TextRulesNeedFeatureSupport: textRulesNeedFeatureSupport,
		Rules:                       rules,
		scorers:                     scorers,
	}, nil
}

// validateTiers checks the global tier cut points (S3): both finite (a
// `.nan` in YAML round-trips into a real NaN and previously slipped past
// every existing check, since a NaN comparison is always false) and in
// (0,1], with high strictly greater than medium.
func validateTiers(medium, high float64) []error {
	var issues []error
	mediumOK, highOK := true, true

	if math.IsNaN(medium) || math.IsInf(medium, 0) {
		issues = append(issues, errors.New("config: tiers.medium must be a finite number"))
		mediumOK = false
	} else if medium <= 0 || medium > 1 {
		issues = append(issues, fmt.Errorf("config: tiers.medium (%v) must be in (0,1]", medium))
		mediumOK = false
	}
	if math.IsNaN(high) || math.IsInf(high, 0) {
		issues = append(issues, errors.New("config: tiers.high must be a finite number"))
		highOK = false
	} else if high <= 0 || high > 1 {
		issues = append(issues, fmt.Errorf("config: tiers.high (%v) must be in (0,1]", high))
		highOK = false
	}
	if mediumOK && highOK && high <= medium {
		issues = append(issues, fmt.Errorf("config: tiers.high (%v) must be greater than tiers.medium (%v)", high, medium))
	}
	return issues
}

// validateNoDuplicatesOrEmpty checks a rule's `labels` or `inputs` list
// (S3) for an empty string or a repeated entry — either is a config
// mistake (a copy-paste error, or a label typo'd into two spellings) that
// silently produces a rule with fewer effective entries than the author
// intended.
func validateNoDuplicatesOrEmpty(ruleName, kind string, values []string) []error {
	var issues []error
	seen := make(map[string]bool, len(values))
	for _, v := range values {
		if v == "" {
			issues = append(issues, fmt.Errorf("config: rule %q has an empty %s", ruleName, kind))
			continue
		}
		if seen[v] {
			issues = append(issues, fmt.Errorf("config: rule %q has duplicate %s %q", ruleName, kind, v))
		}
		seen[v] = true
	}
	return issues
}

// knownStageKeys enumerates the stage conditions internal/core
// understands (see core.Plan). Listing them here, not there, keeps the
// "reject an unrecognized stage key at load time" behavior next to the
// rest of config validation.
var knownStageKeys = map[string]bool{
	"min_local_risk":    true, // gate on the max risk so far of any rule scored by "local"
	"max_subject_age_h": true, // gate on features["subject_age_h"] <= value
	"min_subject_age_h": true, // gate on features["subject_age_h"] >= value
}

func validateStage(r Rule) error {
	for k, v := range r.Stage {
		if !knownStageKeys[k] {
			keys := make([]string, 0, len(knownStageKeys))
			for k := range knownStageKeys {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			return fmt.Errorf("unknown stage condition %q (known: %s)", k, strings.Join(keys, ", "))
		}
		// S3: a `.nan`/infinite stage value previously slipped past every
		// comparison in core.Plan's stageSkip (a NaN comparison is always
		// false), silently making the condition a no-op instead of a
		// load-time error.
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("stage condition %q must be a finite number, got %v", k, v)
		}
		switch k {
		case "min_local_risk":
			if v < 0 || v > 1 {
				return fmt.Errorf("stage condition %q must be in [0,1], got %v", k, v)
			}
		case "max_subject_age_h", "min_subject_age_h":
			if v < 0 {
				return fmt.Errorf("stage condition %q must be >= 0 hours, got %v", k, v)
			}
		}
	}
	// S3: a rule scored by "local" cannot stage itself on min_local_risk —
	// core.Plan's stageSkip gates on "the max risk so far among rules
	// scored by local", and a rule using its own not-yet-computed risk to
	// decide whether to compute itself is circular.
	if _, ok := r.Stage["min_local_risk"]; ok && r.Scorer == "local" {
		return errors.New("cannot stage min_local_risk against itself (it is itself scored by \"local\")")
	}
	return nil
}

// resolveScorer turns a rule's `scorer:` string into a constructed
// model.Scorer plus the list of base adapter names it is built from (for
// vendor-allowlist checking).
//
// S1 (design's v0 slice) resolves only a plain registered scorer name.
// The design's `vote(a,b,...)` combinator (§4.6) is deliberately NOT
// parsed here — S1's own review found its semantics deviate from the
// design (it averaged member scorers' raw probabilities; the design
// calls for a mean of already-CALIBRATED risks, which only makes sense
// after Combine's calibration step, i.e. living in internal/core rather
// than internal/model). TODO(design §4.6): reintroduce `vote(...)` in a
// later slice, implemented in core over calibrated risks, with its own
// "members must share a label set" load-time check (model.LabelMode.Equal
// already exists for that).
func resolveScorer(expr string, reg *model.Registry) (model.Scorer, []string, error) {
	if reg == nil {
		return nil, nil, fmt.Errorf("no scorer registry configured")
	}
	s, ok := reg.Get(expr)
	if !ok {
		return nil, nil, fmt.Errorf("unknown scorer %q; registered: %v", expr, reg.Names())
	}
	return s, []string{expr}, nil
}

// resolveInputs converts raw YAML rules into Rules with `inputs` fully
// resolved: either a literal list, or a `{same_as: <rule>}` reference
// substituted with that rule's own (already-resolved) inputs. Resolution
// runs to a fixed point so `same_as` can point at another `same_as` rule,
// and reports a config error for an unknown target or a genuine cycle
// rather than looping forever.
func resolveInputs(raw []rawRule) ([]Rule, []error) {
	rules := make([]Rule, len(raw))
	sameAs := make([]string, len(raw)) // "" once resolved or never same_as
	byName := make(map[string]int, len(raw))
	var issues []error

	for i, rr := range raw {
		var threshold float64
		if rr.Threshold != nil {
			threshold = *rr.Threshold
		}
		rules[i] = Rule{
			Name:        rr.Name,
			Mode:        Mode(rr.Mode),
			Scorer:      rr.Scorer,
			Text:        rr.Text,
			Labels:      rr.Labels,
			BenignLabel: rr.BenignLabel,
			Threshold:   threshold, // presence/range validated in Load, from the raw *float64
			Stage:       rr.Stage,
		}
		if rr.Name != "" {
			byName[rr.Name] = i
		}

		switch rr.Inputs.Kind {
		case 0: // not present
		case yaml.SequenceNode:
			var inputs []string
			if err := rr.Inputs.Decode(&inputs); err != nil {
				issues = append(issues, fmt.Errorf("config: rule %q has an `inputs` list that isn't strings: %w", rr.Name, err))
				continue
			}
			rules[i].Inputs = inputs
		case yaml.MappingNode:
			// R11 (round 2): checked directly against the raw mapping
			// Content (a flat [key0, value0, key1, value1, ...] slice)
			// instead of decoding into the sameAsRef struct — yaml.Node.
			// Decode does NOT go through the top-level Decoder's
			// KnownFields(true), so an extra key alongside same_as was
			// silently ignored despite the rest of Load being strict.
			// Content length 2 means exactly one key-value pair.
			if len(rr.Inputs.Content) != 2 || rr.Inputs.Content[0].Value != "same_as" || rr.Inputs.Content[1].Value == "" {
				issues = append(issues, fmt.Errorf("config: rule %q has an `inputs` mapping that isn't exactly {same_as: ...}", rr.Name))
				continue
			}
			sameAs[i] = rr.Inputs.Content[1].Value
		default:
			issues = append(issues, fmt.Errorf("config: rule %q has an `inputs` field that is neither a list nor {same_as: ...}", rr.Name))
		}
	}

	// Fixed-point resolution of same_as chains.
	for pass := 0; pass < len(rules)+1; pass++ {
		progress := false
		for i := range rules {
			if sameAs[i] == "" {
				continue
			}
			target, ok := byName[sameAs[i]]
			if !ok {
				continue // reported after the loop if still unresolved
			}
			if sameAs[target] != "" {
				continue // target itself unresolved this pass; try again next pass
			}
			rules[i].Inputs = append([]string(nil), rules[target].Inputs...)
			sameAs[i] = ""
			progress = true
		}
		if !progress {
			break
		}
	}
	for i := range rules {
		if sameAs[i] != "" {
			if _, ok := byName[sameAs[i]]; !ok {
				issues = append(issues, fmt.Errorf("config: rule %q has inputs.same_as referencing unknown rule %q", rules[i].Name, sameAs[i]))
			} else {
				issues = append(issues, fmt.Errorf("config: rule %q has an inputs.same_as cycle through %q", rules[i].Name, sameAs[i]))
			}
		}
	}

	return rules, issues
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
