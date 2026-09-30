package main

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/tokencanopy/abusekit/internal/model/local"
)

func goldenArgs(t *testing.T, extra ...string) []string {
	t.Helper()
	root := repoRoot(t)
	return append([]string{"--golden", "--dataset", filepath.Join(root, "eval", "fixtures"), "--rules", filepath.Join(root, "config", "rules.yaml"), "--vendors", filepath.Join(root, "config", "vendors.yaml"), "--weights", filepath.Join(root, "config", "local_weights.yaml"), "--brands", filepath.Join(root, "config", "brands.yaml"), "--brands-extra", filepath.Join(root, "eval", "fixtures", "test_brands.yaml"), "--webmail", filepath.Join(root, "config", "webmail.yaml")}, extra...)
}

func TestGoldenLastBitWeightMutationFails(t *testing.T) {
	root := repoRoot(t)
	path := filepath.Join(root, "config", "local_weights.yaml")
	w, err := local.LoadWeightsFile(path)
	if err != nil {
		t.Fatal(err)
	}
	old := w.Weight["resource_velocity_1h"]
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(raw), "resource_velocity_1h: "+strconv.FormatFloat(old, 'g', -1, 64), "resource_velocity_1h: "+strconv.FormatFloat(math.Float64frombits(math.Float64bits(old)^1), 'g', -1, 64), 1)
	if changed == string(raw) {
		t.Fatal("test did not mutate weight")
	}
	mutated := filepath.Join(t.TempDir(), "weights.yaml")
	os.WriteFile(mutated, []byte(changed), 0600)
	err = runEval(goldenArgs(t, "--weights", mutated, "--golden-check", filepath.Join(root, "eval", "golden", "reference-flat.jsonl")))
	var exit *exitError
	if !errors.As(err, &exit) || exit.code != 1 || !strings.Contains(err.Error(), "golden drift") {
		t.Fatalf("want golden drift exit 1, got %v", err)
	}
}

func TestGoldenFlagsRejectIgnoredOptions(t *testing.T) {
	for _, args := range [][]string{{"--golden", "--skip-invalid"}, {"--golden", "--scorer", "fake"}, {"--golden-check", "x"}} {
		if _, err := parseEvalFlags(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
