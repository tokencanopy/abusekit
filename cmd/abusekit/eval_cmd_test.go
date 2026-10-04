package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tokencanopy/abusekit/eval"
	"github.com/tokencanopy/abusekit/internal/model/fake"
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
		"--webmail", filepath.Join(root, "config", "webmail.yaml"),
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
// core.resource_velocity_1h is one of 9 (of 18) weights the PR body's own
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
	mutated := strings.Replace(string(shipped), "core.resource_velocity_1h: 0.35", "core.resource_velocity_1h: 0.0", 1)
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
		t.Fatalf("zeroing core.resource_velocity_1h did not break the gate")
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
		"--webmail", filepath.Join(root, "config", "webmail.yaml"),
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
	row.Input.Features = map[string]float64{"core.subject_age_h": 0.1}
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

// withExtraFakeScorer registers a non-local fake.Scorer named "vendor_fake"
// for the duration of one test (review round 2, T1's "inject a fake
// non-local scorer into the registry" — see ruleconfig.go's
// testExtraScorers), automatically un-registering it via t.Cleanup so
// tests never leak state into each other.
func withExtraFakeScorer(t *testing.T) {
	t.Helper()
	s := fake.New()
	s.NameValue = "vendor_fake"
	testExtraScorers = nil
	testExtraScorers = append(testExtraScorers, s)
	t.Cleanup(func() { testExtraScorers = nil })
}

func TestRunEval_NonLocalScorerRequiresCassetteOrRecord(t *testing.T) {
	withExtraFakeScorer(t)
	args := syntheticCorpusArgs(t, "--scorer", "vendor_fake", "--out", filepath.Join(t.TempDir(), "run.json"))
	err := runEval(args)
	var ec *exitError
	if !errors.As(err, &ec) || ec.code != 2 {
		t.Fatalf("runEval(vendor_fake, no --cassettes/--record) = %v, want an exitError with code 2", err)
	}
}

func TestRunEval_NonLocalCassetteMissExitsNonZero(t *testing.T) {
	withExtraFakeScorer(t)
	args := syntheticCorpusArgs(t, "--scorer", "vendor_fake", "--cassettes", t.TempDir(), "--out", filepath.Join(t.TempDir(), "run.json"))
	err := runEval(args)
	if err == nil {
		t.Fatalf("runEval(vendor_fake, empty cassette dir, no --record) returned nil, want a cassette-miss error")
	}
	var ec *exitError
	if !errors.As(err, &ec) || ec.code == 0 {
		t.Fatalf("runEval error = %v, want a non-zero *exitError", err)
	}
}

// TestRunEval_SchemaErrorsReportOwnFileAndLine is fix round P1's own
// acceptance test: an events-file mistake and a labels-file mistake in
// the SAME run must each be reported against their own real path and
// their own correct line number, never a bogus concatenated path or the
// other file's line count.
func TestRunEval_SchemaErrorsReportOwnFileAndLine(t *testing.T) {
	dir := t.TempDir()
	eventsPath := filepath.Join(dir, "my-events.jsonl")
	labelsPath := filepath.Join(dir, "my-labels.jsonl")
	if err := os.WriteFile(eventsPath, []byte(
		"{\"subject\":\"acct_1\",\"type\":\"subject.created\",\"at\":\"2031-01-01T00:00:00Z\",\"data\":{}}\n"+
			"{\"subject\":\"acct_2\",\"type\":\"NOT VALID\",\"at\":\"2031-01-01T00:01:00Z\",\"data\":{}}\n"), 0o644); err != nil {
		t.Fatalf("write events: %v", err)
	}
	if err := os.WriteFile(labelsPath, []byte(
		"{\"subject\":\"acct_1\",\"label\":\"benign\",\"source\":\"operator\",\"decision_at\":{\"full\":\"2031-01-01T01:00:00Z\"}}\n"+
			"{\"subject\":\"acct_missing\",\"label\":\"abusive\",\"source\":\"operator\",\"decision_at\":{\"full\":\"2031-01-01T01:00:00Z\"}}\n"), 0o644); err != nil {
		t.Fatalf("write labels: %v", err)
	}
	root := repoRoot(t)
	args := []string{
		"--dataset", eventsPath, "--labels", labelsPath,
		"--rules", filepath.Join(root, "config", "rules.yaml"),
		"--vendors", filepath.Join(root, "config", "vendors.yaml"),
		"--weights", filepath.Join(root, "config", "local_weights.yaml"),
		"--brands", filepath.Join(root, "config", "brands.yaml"),
		"--webmail", filepath.Join(root, "config", "webmail.yaml"),
		"--rule", "new_account_velocity", "--scorer", "local",
		"--out", filepath.Join(t.TempDir(), "run.json"),
	}
	err := runEval(args)
	if err == nil {
		t.Fatalf("expected a schema error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, eventsPath+":2:") {
		t.Errorf("error %q does not name %s:2", msg, eventsPath)
	}
	if !strings.Contains(msg, labelsPath+":2:") {
		t.Errorf("error %q does not name %s:2", msg, labelsPath)
	}
}

// TestRunEval_SkipInvalidWritesSkippedRows is fix round P2's own
// acceptance test.
func TestRunEval_SkipInvalidWritesSkippedRows(t *testing.T) {
	dir := t.TempDir()
	eventsPath := filepath.Join(dir, "events.jsonl")
	labelsPath := filepath.Join(dir, "labels.jsonl")
	if err := os.WriteFile(eventsPath, []byte(
		"{\"subject\":\"acct_1\",\"type\":\"subject.created\",\"at\":\"2031-01-01T00:00:00Z\",\"data\":{}}\n"+
			"{\"subject\":\"acct_1\",\"type\":\"resource.created\",\"at\":\"2031-01-01T00:01:00Z\",\"data\":{\"kind\":\"agent\",\"name\":\"A\"}}\n"), 0o644); err != nil {
		t.Fatalf("write events: %v", err)
	}
	if err := os.WriteFile(labelsPath, []byte(
		"{\"subject\":\"acct_1\",\"label\":\"benign\",\"source\":\"operator\",\"decision_at\":{\"full\":\"2031-01-01T01:00:00Z\"}}\n"+
			"{\"subject\":\"acct_missing\",\"label\":\"abusive\",\"source\":\"operator\",\"decision_at\":{\"full\":\"2031-01-01T01:00:00Z\"}}\n"), 0o644); err != nil {
		t.Fatalf("write labels: %v", err)
	}
	out := filepath.Join(t.TempDir(), "run.json")
	root := repoRoot(t)
	args := []string{
		"--dataset", eventsPath, "--labels", labelsPath,
		"--rules", filepath.Join(root, "config", "rules.yaml"),
		"--vendors", filepath.Join(root, "config", "vendors.yaml"),
		"--weights", filepath.Join(root, "config", "local_weights.yaml"),
		"--brands", filepath.Join(root, "config", "brands.yaml"),
		"--webmail", filepath.Join(root, "config", "webmail.yaml"),
		"--rule", "new_account_velocity", "--scorer", "local", "--skip-invalid",
		"--out", out,
	}
	if err := runEval(args); err != nil {
		t.Fatalf("runEval(--skip-invalid): %v", err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read run.json: %v", err)
	}
	var got struct {
		SkippedRows   []map[string]any `json:"skipped_rows"`
		SkippedByCode map[string]int   `json:"skipped_by_code"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal run.json: %v", err)
	}
	if len(got.SkippedRows) == 0 {
		t.Fatalf("skipped_rows is empty, want at least the acct_missing row reported")
	}
	if got.SkippedByCode["zero_events"] == 0 {
		t.Fatalf("skipped_by_code = %v, want a zero_events entry (fix round T2)", got.SkippedByCode)
	}
}

// TestRunEval_NullOptionalFieldsLoadAndDontChangeVerdicts is fix round
// P3's own acceptance test: an event carrying an explicit JSON null for
// an optional field must load successfully (not the pre-fix hard
// failure) AND score IDENTICALLY to the same event with that field
// omitted entirely — "null" and "absent" are the same thing to this
// loader, not two different inputs that happen to both work.
func TestRunEval_NullOptionalFieldsLoadAndDontChangeVerdicts(t *testing.T) {
	labels := "{\"subject\":\"acct_1\",\"label\":\"benign\",\"source\":\"operator\",\"decision_at\":{\"full\":\"2031-01-01T02:00:00Z\"}}\n"

	runWith := func(t *testing.T, verdictData string) eval.Verdict {
		t.Helper()
		dir := t.TempDir()
		eventsPath := filepath.Join(dir, "events.jsonl")
		labelsPath := filepath.Join(dir, "labels.jsonl")
		events := "" +
			"{\"subject\":\"acct_1\",\"type\":\"subject.created\",\"at\":\"2031-01-01T00:00:00Z\",\"data\":{}}\n" +
			"{\"subject\":\"acct_1\",\"type\":\"resource.created\",\"at\":\"2031-01-01T00:01:00Z\",\"data\":{\"kind\":\"agent\",\"name\":\"A\"}}\n" +
			"{\"subject\":\"acct_1\",\"type\":\"content.verdict\",\"at\":\"2031-01-01T00:02:00Z\",\"data\":" + verdictData + "}\n"
		if err := os.WriteFile(eventsPath, []byte(events), 0o644); err != nil {
			t.Fatalf("write events: %v", err)
		}
		if err := os.WriteFile(labelsPath, []byte(labels), 0o644); err != nil {
			t.Fatalf("write labels: %v", err)
		}
		out := filepath.Join(t.TempDir(), "run.json")
		root := repoRoot(t)
		args := []string{
			"--dataset", eventsPath, "--labels", labelsPath,
			"--rules", filepath.Join(root, "config", "rules.yaml"),
			"--vendors", filepath.Join(root, "config", "vendors.yaml"),
			"--weights", filepath.Join(root, "config", "local_weights.yaml"),
			"--brands", filepath.Join(root, "config", "brands.yaml"),
			"--webmail", filepath.Join(root, "config", "webmail.yaml"),
			"--rule", "new_account_velocity", "--scorer", "local",
			"--out", out,
		}
		if err := runEval(args); err != nil {
			t.Fatalf("runEval(%s): %v", verdictData, err)
		}
		b, err := os.ReadFile(out)
		if err != nil {
			t.Fatalf("read run.json: %v", err)
		}
		var got struct {
			Verdicts []eval.Verdict `json:"verdicts"`
		}
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("unmarshal run.json: %v", err)
		}
		if len(got.Verdicts) != 1 {
			t.Fatalf("len(Verdicts) = %d, want 1", len(got.Verdicts))
		}
		return got.Verdicts[0]
	}

	withNull := runWith(t, `{"source":"piguard","category":null,"score":null}`)
	omitted := runWith(t, `{"source":"piguard"}`)
	if withNull.Risk != omitted.Risk || withNull.Flagged != omitted.Flagged {
		t.Fatalf("null-fields verdict %+v != omitted-fields verdict %+v", withNull, omitted)
	}
}
