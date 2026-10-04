package local

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOldWeightsFailWithReplacement(t *testing.T) {
	p := filepath.Join(t.TempDir(), "weights.yaml")
	if err := os.WriteFile(p, []byte("version: v1\nbenign_label: benign\nbias: -2\nweights:\n  key_total: 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWeightsFile(p); err == nil || !strings.Contains(err.Error(), "feature_renamed: key_total is now core.credential_total") {
		t.Fatalf("old weights must fail before scoring: %v", err)
	}
}

func TestMixedWeightsFailWithReplacement(t *testing.T) {
	_, err := New(Weights{BenignLabel: "benign", Weight: map[string]float64{"core.credential_total": 1, "key_total": 2}})
	if err == nil || !strings.Contains(err.Error(), "feature_renamed") {
		t.Fatalf("mixed key spaces accepted: %v", err)
	}
}
