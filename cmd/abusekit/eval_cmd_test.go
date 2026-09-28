package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tokencanopy/abusekit/eval"
)

// syntheticCorpusArgs returns the --dataset/--labels/--rules/... flags
// pointing at this repo's own shipped config and the committed synthetic
// corpus (eval/fixtures/synthetic/) — the exact inputs `make gate` uses.
func syntheticCorpusArgs(t *testing.T, extra ...string) []string {
	t.Helper()
	root := repoRoot(t)
	args := []string{
		"--dataset", filepath.Join(root, "eval", "fixtures", "synthetic", "events.jsonl"),
		"--labels", filepath.Join(root, "eval", "fixtures", "synthetic", "labels.jsonl"),
		"--rules", filepath.Join(root, "config", "rules.yaml"),
		"--vendors", filepath.Join(root, "config", "vendors.yaml"),
		"--weights", filepath.Join(root, "config", "local_weights.yaml"),
		"--brands", filepath.Join(root, "config", "brands.yaml"),
		"--rule", "new_account_velocity",
		"--scorer", "local",
		"--slice", "full",
	}
	return append(args, extra...)
}

func TestRunEval_PassesAgainstSyntheticCorpusWithShippedFloors(t *testing.T) {
	out := filepath.Join(t.TempDir(), "run.json")
	root := repoRoot(t)
	args := syntheticCorpusArgs(t, "--floors", filepath.Join(root, "eval", "floors.yaml"), "--out", out)
	if err := runEval(args); err != nil {
		t.Fatalf("runEval: %v", err)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("run.json was not written: %v", err)
	}
}

func TestRunEval_FloorViolationExitsCode1(t *testing.T) {
	floorsPath := filepath.Join(t.TempDir(), "floors.yaml")
	if err := os.WriteFile(floorsPath, []byte(`
floors:
  - rule: new_account_velocity
    scorer: local
    slice: full
    min_precision: 0.999
`), 0o644); err != nil {
		t.Fatalf("write floors: %v", err)
	}
	args := syntheticCorpusArgs(t, "--floors", floorsPath, "--out", filepath.Join(t.TempDir(), "run.json"))
	err := runEval(args)
	if err == nil {
		t.Fatalf("expected a floor-violation error, got nil")
	}
	var ec *exitError
	if !errors.As(err, &ec) {
		t.Fatalf("error %v is not an *exitError", err)
	}
	if ec.code != 1 {
		t.Fatalf("exit code = %d, want 1 (a floor violation)", ec.code)
	}
}

// TestRunEval_MutatedWeightsBreaksGate is fix round S2's own required
// cmd-level test: zeroing a real, load-bearing weight in a MUTATED COPY
// of the shipped config/local_weights.yaml (never the committed file
// itself — AGENTS.md/this fix round: "do not tune weights") and running
// `abusekit eval` through the CLI's own flag parsing and config loading
// (runEval, not eval.Run directly — see eval/floors_test.go's
// TestGate_WeightRegressionFailsFloors for the equivalent in-process
// check) against the real eval/floors.yaml must fail the gate (exit 1).
// resource_velocity_1h is one of 9 (of 18) weights the PR body's own
// weight-zeroing sweep found DOES break the gate; the other 9 pass when
// zeroed, each with a documented reason in the PR body (redundant with
// an already-gated feature, genuinely small/secondary by design, or
// simply never exercised by this corpus).
func TestRunEval_MutatedWeightsBreaksGate(t *testing.T) {
	root := repoRoot(t)
	shipped, err := os.ReadFile(filepath.Join(root, "config", "local_weights.yaml"))
	if err != nil {
		t.Fatalf("read shipped weights: %v", err)
	}
	mutated := strings.Replace(string(shipped), "resource_velocity_1h: 0.35", "resource_velocity_1h: 0.0", 1)
	if mutated == string(shipped) {
		t.Fatalf("mutation did not match any line in config/local_weights.yaml — has it been reformatted?")
	}
	weightsPath := filepath.Join(t.TempDir(), "mutated_weights.yaml")
	if err := os.WriteFile(weightsPath, []byte(mutated), 0o644); err != nil {
		t.Fatalf("write mutated weights: %v", err)
	}

	args := syntheticCorpusArgs(t, "--weights", weightsPath, "--floors", filepath.Join(root, "eval", "floors.yaml"), "--out", filepath.Join(t.TempDir(), "run.json"))
	err = runEval(args)
	if err == nil {
		t.Fatalf("zeroing resource_velocity_1h did not break the gate")
	}
	var ec *exitError
	if !errors.As(err, &ec) || ec.code != 1 {
		t.Fatalf("runEval(mutated weights) = %v, want an exitError with code 1", err)
	}
}

func TestRunEval_BadInputExitsCode2(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"missing --dataset", []string{"--rule", "x", "--scorer", "local"}},
		{"missing --rule", []string{"--dataset", "x", "--scorer", "local"}},
		{"missing --scorer", []string{"--dataset", "x", "--rule", "x"}},
		{"bad --slice", []string{"--dataset", "x", "--rule", "x", "--scorer", "local", "--slice", "bogus"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runEval(tt.args)
			if err == nil {
				t.Fatalf("expected an error")
			}
			var ec *exitError
			if !errors.As(err, &ec) {
				t.Fatalf("error %v is not an *exitError", err)
			}
			if ec.code != 2 {
				t.Fatalf("exit code = %d, want 2 (bad input)", ec.code)
			}
		})
	}
}

func TestRunEval_UnknownRuleAndScorerAreBadInput(t *testing.T) {
	root := repoRoot(t)
	base := []string{
		"--dataset", filepath.Join(root, "eval", "fixtures", "synthetic", "events.jsonl"),
		"--labels", filepath.Join(root, "eval", "fixtures", "synthetic", "labels.jsonl"),
		"--rules", filepath.Join(root, "config", "rules.yaml"),
		"--vendors", filepath.Join(root, "config", "vendors.yaml"),
		"--weights", filepath.Join(root, "config", "local_weights.yaml"),
		"--brands", filepath.Join(root, "config", "brands.yaml"),
	}
	t.Run("unknown rule", func(t *testing.T) {
		args := append(append([]string{}, base...), "--rule", "does_not_exist", "--scorer", "local")
		err := runEval(args)
		var ec *exitError
		if !errors.As(err, &ec) || ec.code != 2 {
			t.Fatalf("runEval(unknown rule) = %v, want an exitError with code 2", err)
		}
	})
	t.Run("unknown scorer", func(t *testing.T) {
		args := append(append([]string{}, base...), "--rule", "new_account_velocity", "--scorer", "does_not_exist")
		err := runEval(args)
		var ec *exitError
		if !errors.As(err, &ec) || ec.code != 2 {
			t.Fatalf("runEval(unknown scorer) = %v, want an exitError with code 2", err)
		}
	})
}

func TestScoreOneRow_MatchesEvalScoreOne(t *testing.T) {
	cfg, _, _, err := loadRuleConfig(ruleConfigPaths{
		rulesPath:   filepath.Join(repoRoot(t), "config", "rules.yaml"),
		vendorsPath: filepath.Join(repoRoot(t), "config", "vendors.yaml"),
		weightsPath: filepath.Join(repoRoot(t), "config", "local_weights.yaml"),
		brandsPath:  filepath.Join(repoRoot(t), "config", "brands.yaml"),
	})
	if err != nil {
		t.Fatalf("loadRuleConfig: %v", err)
	}
	rule, ok := ruleByName(cfg, "new_account_velocity")
	if !ok {
		t.Fatalf("rule not found")
	}
	scorer, _ := cfg.ScorerFor(rule)

	row := scoreRow{ID: "x"}
	row.Input.Features = map[string]float64{"subject_age_h": 0.1}
	got := scoreOneRow(context.Background(), row, rule, scorer, cfg.Tiers, "v1")
	want, _, err := eval.ScoreOne(context.Background(), rule, scorer, eval.Options{Tiers: cfg.Tiers, PromptVersion: "v1"}, eval.Point{Features: row.Input.Features})
	if err != nil {
		t.Fatalf("eval.ScoreOne: %v", err)
	}
	if got.Risk != want.Risk || got.Flagged != want.Flagged || got.Tier != want.Tier {
		t.Fatalf("scoreOneRow = %+v, want risk=%v flagged=%v tier=%v", got, want.Risk, want.Flagged, want.Tier)
	}
}

func TestParseCorpusExportFlags_RequiresTenantAndDatabaseURL(t *testing.T) {
	t.Setenv("ABUSEKIT_DATABASE_URL", "")
	if _, err := parseCorpusExportFlags([]string{"--tenant", "e2a"}); err == nil {
		t.Fatalf("expected an error with no database URL")
	}
	if _, err := parseCorpusExportFlags([]string{"--database-url", "postgres://x"}); err == nil {
		t.Fatalf("expected an error with no --tenant")
	}
	f, err := parseCorpusExportFlags([]string{"--database-url", "postgres://x", "--tenant", "e2a"})
	if err != nil {
		t.Fatalf("parseCorpusExportFlags: %v", err)
	}
	if f.split != "all" {
		t.Errorf("default split = %q, want \"all\"", f.split)
	}
}

func TestRunCorpus_RequiresExportSubcommand(t *testing.T) {
	err := runCorpus(nil)
	var ec *exitError
	if !errors.As(err, &ec) || ec.code != 2 {
		t.Fatalf("runCorpus(nil) = %v, want an exitError with code 2", err)
	}
	err = runCorpus([]string{"bogus"})
	if !errors.As(err, &ec) || ec.code != 2 {
		t.Fatalf("runCorpus(bogus) = %v, want an exitError with code 2", err)
	}
}
