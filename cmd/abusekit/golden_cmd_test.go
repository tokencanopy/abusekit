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

	"github.com/tokencanopy/abusekit/eval"
)

func goldenArgs(t *testing.T, extra ...string) []string {
	t.Helper()
	root := repoRoot(t)
	return append([]string{"--golden", "--dataset", filepath.Join(root, "eval", "fixtures"), "--rules", filepath.Join(root, "config", "rules.yaml"), "--vendors", filepath.Join(root, "config", "vendors.yaml"), "--weights", filepath.Join(root, "config", "local_weights.yaml"), "--brands", filepath.Join(root, "config", "brands.yaml"), "--brands-extra", filepath.Join(root, "eval", "fixtures", "test_brands.yaml"), "--webmail", filepath.Join(root, "config", "webmail.yaml")}, extra...)
}

func TestGoldenLastBitWeightMutationFails(t *testing.T) {
	root := repoRoot(t)
	referenceName := "reference-flat.jsonl"
	if profile := eval.GoldenProfile(); profile != "amd64-fma" {
		referenceName = "reference-flat-" + profile + ".jsonl"
	}
	reference := filepath.Join(root, "eval", "golden", referenceName)
	if err := runEval(goldenArgs(t, "--golden-check", reference)); err != nil {
		t.Fatalf("unmodified weights must pass first: %v", err)
	}
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
	err = runEval(goldenArgs(t, "--weights", mutated, "--golden-check", reference))
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

func TestGoldenOutputCannotOverwriteSources(t *testing.T) {
	for _, kind := range []string{"directory-fixture", "symlink", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			fixtures := filepath.Join(dir, "fixtures")
			os.MkdirAll(filepath.Join(fixtures, "synthetic"), 0755)
			src := filepath.Join(fixtures, "case.jsonl")
			data := []byte(`{"id":"x","subject":"acct_x","type":"subject.created","at":"2031-01-01T00:00:00Z","data":{}}` + "\n")
			os.WriteFile(src, data, 0600)
			os.WriteFile(filepath.Join(fixtures, "synthetic", "events.jsonl"), data, 0600)
			dataset, output := fixtures, src
			if kind != "directory-fixture" {
				dataset = src
				output = filepath.Join(dir, "alias.jsonl")
				var err error
				if kind == "symlink" {
					err = os.Symlink(src, output)
				} else {
					err = os.Link(src, output)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			err := runEval(goldenArgs(t, "--dataset", dataset, "--out", output))
			if err == nil {
				t.Fatal("accepted output alias of an input")
			}
			got, _ := os.ReadFile(src)
			if string(got) != string(data) {
				t.Fatal("input was modified")
			}
		})
	}
}
