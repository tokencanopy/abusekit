package eval

import (
	"bytes"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// FloorEntry is one (rule, scorer, slice)'s gate floors (design §4.10:
// "eval/floors.yaml defines per-rule/scorer floors ... against the
// committed synthetic corpus"). A nil field means "no floor configured
// for this metric" — MinPrecision/MinRecall/MaxECE are *float64 (not
// float64) for the same reason internal/config.rawRule.Threshold is: so
// Check can tell "not configured" from "configured as exactly 0".
type FloorEntry struct {
	Rule         string   `yaml:"rule"`
	Scorer       string   `yaml:"scorer"`
	Slice        string   `yaml:"slice"`
	MinPrecision *float64 `yaml:"min_precision"`
	MinRecall    *float64 `yaml:"min_recall"`
	MaxECE       *float64 `yaml:"max_ece"`
}

func (e FloorEntry) key() string { return e.Rule + "/" + e.Scorer + "/" + e.Slice }

// Floors is a loaded eval/floors.yaml.
type Floors struct {
	Entries []FloorEntry
}

type rawFloorsFile struct {
	Floors []FloorEntry `yaml:"floors"`
}

// LoadFloorsFile reads and parses an eval/floors.yaml-shaped file.
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
	Metric string // "precision" | "recall" | "ece"
	Floor  float64
	Got    float64
}

func (v Violation) String() string {
	if v.Metric == "ece" {
		return fmt.Sprintf("%s: got %.4f, want <= %.4f", v.Metric, v.Got, v.Floor)
	}
	return fmt.Sprintf("%s: got %.4f, want >= %.4f", v.Metric, v.Got, v.Floor)
}

// Check compares m against e's configured floors, returning one
// Violation per failed metric (a metric with no configured floor is
// never checked and never appears). Precision/recall are floors (got
// must be >= floor); ECE is a ceiling (got must be <= floor) — design:
// "fails ... below eval/floors.yaml (floor = lower interval bound of the
// reference run) or above the ECE bound".
func (e FloorEntry) Check(m Metrics) []Violation {
	var violations []Violation
	if e.MinPrecision != nil && m.Threshold.Precision.Value < *e.MinPrecision {
		violations = append(violations, Violation{Metric: "precision", Floor: *e.MinPrecision, Got: m.Threshold.Precision.Value})
	}
	if e.MinRecall != nil && m.Threshold.Recall.Value < *e.MinRecall {
		violations = append(violations, Violation{Metric: "recall", Floor: *e.MinRecall, Got: m.Threshold.Recall.Value})
	}
	if e.MaxECE != nil && m.ECE.Value > *e.MaxECE {
		violations = append(violations, Violation{Metric: "ece", Floor: *e.MaxECE, Got: m.ECE.Value})
	}
	return violations
}
