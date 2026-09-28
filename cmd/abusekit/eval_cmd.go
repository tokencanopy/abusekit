package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tokencanopy/abusekit/eval"
	"github.com/tokencanopy/abusekit/internal/config"
	"github.com/tokencanopy/abusekit/internal/feature"
	"github.com/tokencanopy/abusekit/internal/model"
)

// evalFlags is `abusekit eval`'s full flag set (design §4.10 / task
// brief). Every path flag reuses `serve`'s own env-var-defaulted
// convention (envOr) so a deployment's existing ABUSEKIT_RULES_CONFIG
// etc. work unchanged for the harness too.
type evalFlags struct {
	ruleConfigPaths
	dataset       string
	labels        string
	rule          string
	scorer        string
	slice         string
	out           string
	cassettesDir  string
	record        bool
	floorsPath    string
	promptVersion string
}

func parseEvalFlags(args []string) (evalFlags, error) {
	fs := flag.NewFlagSet("eval", flag.ContinueOnError)
	var f evalFlags
	fs.StringVar(&f.rulesPath, "rules", envOr("ABUSEKIT_RULES_CONFIG", "config/rules.yaml"), "path to rules.yaml")
	fs.StringVar(&f.vendorsPath, "vendors", envOr("ABUSEKIT_VENDORS_CONFIG", "config/vendors.yaml"), "path to vendors.yaml")
	fs.StringVar(&f.weightsPath, "weights", envOr("ABUSEKIT_LOCAL_WEIGHTS", "config/local_weights.yaml"), "path to the local scorer's weights YAML")
	fs.StringVar(&f.brandsPath, "brands", envOr("ABUSEKIT_BRANDS_CONFIG", "config/brands.yaml"), "path to brands.yaml")
	fs.StringVar(&f.dataset, "dataset", "", "path to a label-snapshot corpus JSONL (design §4.6), or — with --labels also set — an event-replay events JSONL (required)")
	fs.StringVar(&f.labels, "labels", "", "path to an event-replay labels JSONL; when set, --dataset is read as the matching events file (design §4.6's second corpus shape)")
	fs.StringVar(&f.rule, "rule", "", "the rule name (from rules.yaml) to score against (required)")
	fs.StringVar(&f.scorer, "scorer", "", "a registered scorer name, or \"all\" to run every registered scorer side by side (required)")
	fs.StringVar(&f.slice, "slice", "full", "which decision-point slice to score: first_send, early_15m, or full")
	fs.StringVar(&f.out, "out", "", "path to write run.json to (default: stdout)")
	fs.StringVar(&f.cassettesDir, "cassettes", "", "directory of per-scorer cassette files (design §4.10); local never needs one")
	fs.BoolVar(&f.record, "record", false, "record a live scorer's answers into its cassette instead of replaying (never used in CI)")
	fs.StringVar(&f.floorsPath, "floors", "", "path to eval/floors.yaml; when set, exits 1 if any configured floor is violated")
	fs.StringVar(&f.promptVersion, "prompt-version", "v1", "render/prompt version recorded on the manifest and in the cassette key")
	if err := fs.Parse(args); err != nil {
		return evalFlags{}, exitCode2(err)
	}
	if f.dataset == "" {
		return evalFlags{}, exitCode2(fmt.Errorf("--dataset is required"))
	}
	if f.rule == "" {
		return evalFlags{}, exitCode2(fmt.Errorf("--rule is required"))
	}
	if f.scorer == "" {
		return evalFlags{}, exitCode2(fmt.Errorf("--scorer is required (a registered scorer name, or \"all\")"))
	}
	if !eval.ValidSliceFlag(f.slice) {
		return evalFlags{}, exitCode2(fmt.Errorf("--slice %q must be one of first_send, early_15m, full", f.slice))
	}
	return f, nil
}

// runEval implements `abusekit eval` (design §4.10 / task brief).
func runEval(args []string) error {
	f, err := parseEvalFlags(args)
	if err != nil {
		return err
	}

	cfg, registry, brands, err := loadRuleConfig(f.ruleConfigPaths)
	if err != nil {
		return exitCode2(fmt.Errorf("load rule config: %w", err))
	}
	rule, ok := ruleByName(cfg, f.rule)
	if !ok {
		return exitCode2(fmt.Errorf("unknown rule %q (known: %s)", f.rule, strings.Join(ruleNames(cfg), ", ")))
	}

	scorerNames, err := resolveScorerNames(registry, f.scorer)
	if err != nil {
		return exitCode2(err)
	}

	dataset, datasetSHA, labelsSHA, err := loadEvalDataset(f.dataset, f.labels, brands)
	if err != nil {
		return exitCode2(err)
	}

	var floors eval.Floors
	if f.floorsPath != "" {
		floors, err = eval.LoadFloorsFile(f.floorsPath)
		if err != nil {
			return exitCode2(fmt.Errorf("load floors: %w", err))
		}
	}

	ctx := context.Background()
	results := make(map[string]eval.RunResult, len(scorerNames))
	var violations []string

	for _, name := range scorerNames {
		scorer, ok := registry.Get(name)
		if !ok {
			return exitCode2(fmt.Errorf("scorer %q is not registered", name))
		}
		scorer, err := maybeWrapCassette(scorer, f.cassettesDir, f.record, f.promptVersion)
		if err != nil {
			return exitCode2(err)
		}

		res, err := eval.Run(ctx, dataset, rule, scorer, eval.Options{
			Slice:         eval.Slice(f.slice),
			Tiers:         cfg.Tiers,
			PromptVersion: f.promptVersion,
			DatasetSHA:    datasetSHA,
			LabelsSHA:     labelsSHA,
		})
		if err != nil {
			return exitCode2(fmt.Errorf("eval.Run(%s): %w", name, err))
		}
		results[name] = res

		if cs, ok := scorer.(*eval.CassetteScorer); ok && f.record {
			if err := cs.Cassette.Save(); err != nil {
				return exitCode2(fmt.Errorf("save cassette for %s: %w", name, err))
			}
		}

		if entry, ok := floors.For(rule.Name, name, f.slice); ok {
			for _, v := range entry.Check(res.Metrics) {
				violations = append(violations, fmt.Sprintf("(%s, %s, %s) %s", rule.Name, name, f.slice, v))
			}
		}
	}

	if len(scorerNames) > 1 {
		printScorerTable(os.Stdout, scorerNames, results)
		if f.out != "" {
			if err := writeEvalOutput(f.out, scorerNames, results); err != nil {
				return exitCode2(err)
			}
		}
	} else if err := writeEvalOutput(f.out, scorerNames, results); err != nil {
		return exitCode2(err)
	}

	if len(violations) > 0 {
		msg := fmt.Sprintf("gate: %d floor violation(s):\n  %s", len(violations), strings.Join(violations, "\n  "))
		return exitCode1(fmt.Errorf("%s", msg))
	}
	return nil
}

// ruleByName / ruleNames look up a rule from cfg by name — config.Config
// has no such accessor of its own (only ScorerFor(rule Rule), which needs
// a Rule value already in hand).
func ruleByName(cfg *config.Config, name string) (config.Rule, bool) {
	for _, r := range cfg.Rules {
		if r.Name == name {
			return r, true
		}
	}
	return config.Rule{}, false
}

func ruleNames(cfg *config.Config) []string {
	names := make([]string, 0, len(cfg.Rules))
	for _, r := range cfg.Rules {
		names = append(names, r.Name)
	}
	sort.Strings(names)
	return names
}

// resolveScorerNames expands `--scorer all` to every registered scorer
// name (sorted, so table/output order is deterministic); any other value
// must name exactly one registered scorer.
func resolveScorerNames(registry *model.Registry, flagValue string) ([]string, error) {
	if flagValue == "all" {
		names := registry.Names()
		if len(names) == 0 {
			return nil, fmt.Errorf("no scorers are registered")
		}
		return names, nil
	}
	if _, ok := registry.Get(flagValue); !ok {
		return nil, fmt.Errorf("unknown scorer %q (registered: %s)", flagValue, strings.Join(registry.Names(), ", "))
	}
	return []string{flagValue}, nil
}

// loadEvalDataset loads --dataset (and --labels, if set) into an
// eval.Dataset, returning the SHA-256 of each file actually read so the
// manifest reflects the real bytes on disk (eval.SHA256File), not a
// re-serialization of what was parsed out of them. --labels set selects
// the event-replay shape; unset selects the label-snapshot corpus shape
// (design §4.6's two corpus shapes).
func loadEvalDataset(datasetPath, labelsPath string, brands feature.BrandSet) (dataset eval.Dataset, datasetSHA, labelsSHA string, err error) {
	datasetSHA, err = eval.SHA256File(datasetPath)
	if err != nil {
		return eval.Dataset{}, "", "", fmt.Errorf("hash --dataset %s: %w", datasetPath, err)
	}

	if labelsPath == "" {
		f, err := os.Open(datasetPath)
		if err != nil {
			return eval.Dataset{}, "", "", fmt.Errorf("open --dataset %s: %w", datasetPath, err)
		}
		defer f.Close()
		dataset, rowErrs, err := eval.LoadSnapshotCorpus(f)
		if err != nil {
			return eval.Dataset{}, "", "", schemaCLIError(datasetPath, rowErrs, err)
		}
		return dataset, datasetSHA, "", nil
	}

	labelsSHA, err = eval.SHA256File(labelsPath)
	if err != nil {
		return eval.Dataset{}, "", "", fmt.Errorf("hash --labels %s: %w", labelsPath, err)
	}
	eventsF, err := os.Open(datasetPath)
	if err != nil {
		return eval.Dataset{}, "", "", fmt.Errorf("open --dataset %s: %w", datasetPath, err)
	}
	defer eventsF.Close()
	labelsF, err := os.Open(labelsPath)
	if err != nil {
		return eval.Dataset{}, "", "", fmt.Errorf("open --labels %s: %w", labelsPath, err)
	}
	defer labelsF.Close()

	dataset, rowErrs, err := eval.LoadReplayDataset(eventsF, labelsF, brands)
	if err != nil {
		return eval.Dataset{}, "", "", schemaCLIError(datasetPath+"/"+labelsPath, rowErrs, err)
	}
	return dataset, datasetSHA, labelsSHA, nil
}

// schemaCLIError renders a *eval.SchemaError's per-row failures as one
// multi-line message naming the file they came from; any other error
// (a genuine I/O failure) is passed through unchanged.
func schemaCLIError(sourceLabel string, rowErrs []eval.RowError, err error) error {
	if len(rowErrs) == 0 {
		return err
	}
	lines := make([]string, 0, len(rowErrs))
	for _, re := range rowErrs {
		lines = append(lines, fmt.Sprintf("  %s:%d: %v", sourceLabel, re.Line, re.Err))
	}
	return fmt.Errorf("%d row(s) failed schema validation:\n%s", len(rowErrs), strings.Join(lines, "\n"))
}

// maybeWrapCassette wraps scorer in an eval.CassetteScorer when
// cassettesDir is set and scorer isn't the local scorer (task brief:
// "Local needs no cassette"). The cassette file lives at
// <cassettesDir>/<scorer-name>.json.
func maybeWrapCassette(scorer model.Scorer, cassettesDir string, record bool, promptVersion string) (model.Scorer, error) {
	if cassettesDir == "" || scorer.Name() == "local" {
		return scorer, nil
	}
	path := filepath.Join(cassettesDir, scorer.Name()+".json")
	cassette, err := eval.LoadCassette(path)
	if err != nil {
		return nil, fmt.Errorf("load cassette %s: %w", path, err)
	}
	mode := eval.CassetteReplay
	if record {
		mode = eval.CassetteRecord
	}
	return &eval.CassetteScorer{Inner: scorer, Cassette: cassette, Mode: mode, PromptVersion: promptVersion}, nil
}

// printScorerTable prints `--scorer all`'s side-by-side comparison
// (design §4.10: "--scorer all prints a side-by-side table").
func printScorerTable(w *os.File, names []string, results map[string]eval.RunResult) {
	fmt.Fprintf(w, "%-20s %10s %10s %10s %10s %10s %10s\n", "scorer", "precision", "recall", "f1", "ece", "auroc", "unscored")
	for _, name := range names {
		m := results[name].Metrics
		auroc := "n/a"
		if m.AUROC.Defined {
			auroc = fmt.Sprintf("%.4f", m.AUROC.Value)
		}
		fmt.Fprintf(w, "%-20s %10.4f %10.4f %10.4f %10.4f %10s %10d\n",
			name, m.Threshold.Precision.Value, m.Threshold.Recall.Value, m.Threshold.F1, m.ECE.Value, auroc, m.UnscoredCount)
	}
}

// writeEvalOutput writes run.json (a single scorer) or a
// {"<scorer>": run.json, ...} object (--scorer all) to path, or to
// stdout when path is empty.
func writeEvalOutput(path string, names []string, results map[string]eval.RunResult) error {
	var payload any
	if len(names) == 1 {
		payload = results[names[0]]
	} else {
		payload = results
	}
	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal run.json: %w", err)
	}
	b = append(b, '\n')
	if path == "" {
		_, err := os.Stdout.Write(b)
		return err
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, b, 0o644)
}
