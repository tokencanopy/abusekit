package local

import (
	"bytes"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// LoadWeightsFile reads and parses a config/local_weights.yaml-shaped file
// into a Weights value, then validates it. Kept separate from New so
// tests can build a Scorer from an in-memory Weights without touching the
// filesystem.
func LoadWeightsFile(path string) (Weights, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Weights{}, fmt.Errorf("local: read weights file %s: %w", path, err)
	}
	var w Weights
	// R5 (round 2): strict decoding (KnownFields) so a typo'd or stale
	// top-level key (e.g. "bais" instead of "bias") fails the load instead
	// of silently leaving that field at its zero value — a misspelled
	// `bias` would otherwise ship as bias=0 with no warning.
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&w); err != nil {
		return Weights{}, fmt.Errorf("local: parse weights file %s: %w", path, err)
	}
	if err := w.Validate(); err != nil {
		return Weights{}, fmt.Errorf("local: %s: %w", path, err)
	}
	return w, nil
}
