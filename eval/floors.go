package eval

import (
	"bytes"
	"fmt"
	"os"
	"sort"

	"gopkg.in/yaml.v3"
)

// FloorEntry is one (rule, scorer, slice)'s gate floors (design §4.10:
// "eval/floors.yaml defines per-rule/scorer floors ... against the
// committed synthetic corpus"). A nil field means "no floor configured
// for this metric" — every *float64 field (rather than float64) is for
// the same reason internal/config.rawRule.Threshold is: so Check can
// tell "not configured" from "configured as exactly 0".
type FloorEntry struct {
	Rule         string   `yaml:"rule"`
	Scorer       string   `yaml:"scorer"`
	Slice        string   `yaml:"slice"`
	MinPrecision *float64 `yaml:"min_precision"`
	MinRecall    *float64 `yaml:"min_recall"`
	MaxECE       *float64 `yaml:"max_ece"`
	// MinAUROC is a floor on Metrics.AUROC (fix round S2).
	MinAUROC *float64 `yaml:"min_auroc"`
	// MinHighTierRecall is a floor on Metrics.TierCuts["high"].Recall
	// (fix round S2) — distinct from MinRecall, which is at the rule's
	// own threshold, not the (usually higher) "high" tier cut.
	MinHighTierRecall *float64 `yaml:"min_high_tier_recall"`
	// FamilyMinHighTierRecall floors specific entries of
	// Metrics.FamilyHighTierRecall (fix round S2), keyed the same way:
	// "burst", "dormant_then_blast", "churn_incarnation_ge3".
	FamilyMinHighTierRecall map[string]float64 `yaml:"family_min_high_tier_recall"`
}

// Floors is a loaded eval/floors.yaml.
type Floors struct {
	Entries []FloorEntry
}

type rawFloorsFile struct {
	Floors []FloorEntry `yaml:"floors"`
}

// unitFloats returns every *float64 field on e that Check treats as a
// probability/rate (must be validated into [0,1] — fix round S1), each
// paired with a name for the error message. MinRecall/MinPrecision/
// MinAUROC/MinHighTierRecall and every FamilyMinHighTierRecall entry are
// all rates; MaxECE is also bounded [0,1] (ECE is a mean absolute
// difference of two probabilities, so it can never legitimately exceed
// 1 either).
func (e FloorEntry) unitFloats() map[string]float64 {
	out := map[string]float64{}
	add := func(name string, v *float64) {
		if v != nil {
			out[name] = *v
		}
	}
	add("min_precision", e.MinPrecision)
	add("min_recall", e.MinRecall)
	add("max_ece", e.MaxECE)
	add("min_auroc", e.MinAUROC)
	add("min_high_tier_recall", e.MinHighTierRecall)
	for fam, v := range e.FamilyMinHighTierRecall {
		out["family_min_high_tier_recall."+fam] = v
	}
	return out
}

func (e FloorEntry) key() string { return e.Rule + "/" + e.Scorer + "/" + e.Slice }

// LoadFloorsFile reads and parses an eval/floors.yaml-shaped file,
// rejecting (fix round S1) a missing rule/scorer/slice or any floor
// value outside [0,1].
func LoadFloorsFile(path string) (Floors, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Floors{}, fmt.Errorf("eval: read floors file %s: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	var raw rawFloorsFile
	if err := dec.Decode(&raw); err != nil {
		return Floors{}, fmt.Errorf("eval: parse floors file %s: %w", path, err)
	}
	for i, e := range raw.Floors {
		if e.Rule == "" || e.Scorer == "" || e.Slice == "" {
			return Floors{}, fmt.Errorf("eval: %s: floors[%d] is missing rule/scorer/slice", path, i)
		}
		names := make([]string, 0, len(e.unitFloats()))
		for name := range e.unitFloats() {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			v := e.unitFloats()[name]
			if v < 0 || v > 1 {
				return Floors{}, fmt.Errorf("eval: %s: floors[%d] (%s): %s = %v must be in [0,1]", path, i, e.key(), name, v)
			}
		}
	}
	return Floors{Entries: raw.Floors}, nil
}

// For returns the floor entry configured for (rule, scorer, slice), if
// any.
func (f Floors) For(rule, scorer, slice string) (FloorEntry, bool) {
	for _, e := range f.Entries {
		if e.Rule == rule && e.Scorer == scorer && e.Slice == slice {
			return e, true
		}
	}
	return FloorEntry{}, false
}

// Violation is one floor a Run's Metrics failed to clear.
type Violation struct {
	Metric string // "precision" | "recall" | "ece" | "auroc" | "high_tier_recall" | "family_high_tier_recall.<family>"
	Floor  float64
	Got    float64
	// Undefined reports the metric had no evidence at all (Rate.Defined
	// == false, or an empty-set ECE — fix round B3) rather than a real
	// value below floor; Got is 0 in that case, not a genuine
	// measurement.
	Undefined bool
}

func (v Violation) String() string {
	if v.Undefined {
		return fmt.Sprintf("%s: undefined (no evidence), want <= %.4f or a defined value", v.Metric, v.Floor)
	}
	if v.Metric == "ece" {
		return fmt.Sprintf("%s: got %.4f, want <= %.4f", v.Metric, v.Got, v.Floor)
	}
	return fmt.Sprintf("%s: got %.4f, want >= %.4f", v.Metric, v.Got, v.Floor)
}

// Check compares m against e's configured floors, returning one
// Violation per failed metric (a metric with no configured floor is
// never checked and never appears). Every rate floor requires
// Rate.Defined; an undefined rate against a configured floor is always a
// violation (fail closed — design: an unscored/no-evidence metric must
// never silently pass a gate it was never actually measured against).
// ECE is a ceiling (got must be <= floor); every other floor here is a
// minimum.
func (e FloorEntry) Check(m Metrics) []Violation {
	var violations []Violation

	checkMin := func(metric string, floor *float64, rate Rate) {
		if floor == nil {
			return
		}
		if !rate.Defined {
			violations = append(violations, Violation{Metric: metric, Floor: *floor, Undefined: true})
			return
		}
		if rate.Value < *floor {
			violations = append(violations, Violation{Metric: metric, Floor: *floor, Got: rate.Value})
		}
	}

	checkMin("precision", e.MinPrecision, m.Threshold.Precision)
	checkMin("recall", e.MinRecall, m.Threshold.Recall)
	checkMin("min_auroc", e.MinAUROC, Rate{Value: m.AUROC.Value, Defined: m.AUROC.Defined})

	if e.MaxECE != nil {
		if !m.ECE.Defined {
			violations = append(violations, Violation{Metric: "ece", Floor: *e.MaxECE, Undefined: true})
		} else if m.ECE.Value > *e.MaxECE {
			violations = append(violations, Violation{Metric: "ece", Floor: *e.MaxECE, Got: m.ECE.Value})
		}
	}

	if e.MinHighTierRecall != nil {
		high, ok := m.TierCuts["high"]
		if !ok {
			violations = append(violations, Violation{Metric: "high_tier_recall", Floor: *e.MinHighTierRecall, Undefined: true})
		} else {
			checkMin("high_tier_recall", e.MinHighTierRecall, high.Recall)
		}
	}

	if len(e.FamilyMinHighTierRecall) > 0 {
		names := make([]string, 0, len(e.FamilyMinHighTierRecall))
		for fam := range e.FamilyMinHighTierRecall {
			names = append(names, fam)
		}
		sort.Strings(names) // deterministic violation order
		for _, fam := range names {
			floor := e.FamilyMinHighTierRecall[fam]
			rate, ok := m.FamilyHighTierRecall[fam]
			metric := "family_high_tier_recall." + fam
			if !ok {
				violations = append(violations, Violation{Metric: metric, Floor: floor, Undefined: true})
				continue
			}
			checkMin(metric, &floor, rate)
		}
	}

	return violations
}
