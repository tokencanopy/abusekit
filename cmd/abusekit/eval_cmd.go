package main

import (
	"context"
	"encoding/json"
	"errors"
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
	brandsExtraPath string
	webmailPath     string
	dataset         string
	labels          string
	rule            string
	scorer          string
	slice           string
	split           string
	out             string
	cassettesDir    string
	record          bool
	skipInvalid     bool
	floorsPath      string
	promptVersion   string
}

// errHelp is returned by parseEvalFlags when -h/--help was given (fix
// round nit: "abusekit eval --help exits 0" — flag.ErrHelp on its own
// would otherwise be treated as any other parse failure, exit 2).
var errHelp = errors.New("eval: help requested")

func parseEvalFlags(args []string) (evalFlags, error) {
	fs := flag.NewFlagSet("eval", flag.ContinueOnError)
	var f evalFlags
	fs.StringVar(&f.rulesPath, "rules", envOr("ABUSEKIT_RULES_CONFIG", "config/rules.yaml"), "path to rules.yaml")
	fs.StringVar(&f.vendorsPath, "vendors", envOr("ABUSEKIT_VENDORS_CONFIG", "config/vendors.yaml"), "path to vendors.yaml")
	fs.StringVar(&f.weightsPath, "weights", envOr("ABUSEKIT_LOCAL_WEIGHTS", "config/local_weights.yaml"), "path to the local scorer's weights YAML")
	fs.StringVar(&f.brandsPath, "brands", envOr("ABUSEKIT_BRANDS_CONFIG", "config/brands.yaml"), "path to brands.yaml")
	// Both of these mirror `serve`'s own flags exactly (same names, same
	// env vars, same defaults — parseServeFlags in main.go) so the harness
	// scores against the SAME brand/webmail configuration a real
	// deployment runs on, not a silently different one.
	fs.StringVar(&f.brandsExtraPath, "brands-extra", os.Getenv("ABUSEKIT_BRANDS_EXTRA_CONFIG"), "optional path to a private, brands.yaml-shaped extra brand list, merged with --brands (env ABUSEKIT_BRANDS_EXTRA_CONFIG; empty disables it)")
	fs.StringVar(&f.webmailPath, "webmail", envOr("ABUSEKIT_WEBMAIL_CONFIG", "config/webmail.yaml"), "path to webmail.yaml")
	fs.StringVar(&f.dataset, "dataset", "", "path to a label-snapshot corpus JSONL (design §4.6), or — with --labels also set — an event-replay events JSONL (required)")
	fs.StringVar(&f.labels, "labels", "", "path to an event-replay labels JSONL; when set, --dataset is read as the matching events file (design §4.6's second corpus shape)")
	fs.StringVar(&f.rule, "rule", "", "the rule name (from rules.yaml) to score against (required)")
	fs.StringVar(&f.scorer, "scorer", "", "a registered scorer name, or \"all\" to run every registered scorer side by side (required)")
	fs.StringVar(&f.slice, "slice", "full", "which decision-point slice to score: first_send, early_15m, or full")
	fs.StringVar(&f.split, "split", "all", "which split to score: train, test, or all")
	fs.StringVar(&f.out, "out", "", "path to write run.json to (default: stdout)")
	fs.StringVar(&f.cassettesDir, "cassettes", "", "directory of per-scorer cassette files (design §4.10); local never needs one, but every OTHER scorer requires either this or --record")
	fs.BoolVar(&f.record, "record", false, "record a live scorer's answers into its cassette instead of replaying (never used in CI)")
	fs.BoolVar(&f.skipInvalid, "skip-invalid", false, "skip and count invalid rows (reported in run.json's skipped_rows) instead of failing the whole run (default: strict, exit 2 on any bad row)")
	fs.StringVar(&f.floorsPath, "floors", "", "path to eval/floors.yaml; when set, exits 1 if any configured floor is violated, or 2 if none matches the (rule, scorer, slice) being run")
	fs.StringVar(&f.promptVersion, "prompt-version", "v1", "render/prompt version recorded on the manifest and in the cassette key")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return evalFlags{}, errHelp
		}
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
	if f.split != "all" && f.split != "train" && f.split != "test" {
		return evalFlags{}, exitCode2(fmt.Errorf("--split %q must be one of train, test, all", f.split))
	}
	return f, nil
}

// runEval implements `abusekit eval` (design §4.10 / task brief).
func runEval(args []string) error {
	f, err := parseEvalFlags(args)
	if err != nil {
		if errors.Is(err, errHelp) {
			return nil // nit: --help exits 0, not 2
		}
		return err
	}

	cfg, registry, brands, err := loadRuleConfig(f.ruleConfigPaths)
	if err != nil {
		return exitCode2(fmt.Errorf("load rule config: %w", err))
	}
	// Mirrors boot's own sequence in main.go: brands-extra is optional (an
	// empty path merges in nothing), webmail has a default the same as
	// --brands does.
	if f.brandsExtraPath != "" {
		extra, err := feature.LoadBrandsFile(f.brandsExtraPath)
		if err != nil {
			return exitCode2(fmt.Errorf("load brands-extra config: %w", err))
		}
		brands = feature.MergeBrandSets(brands, extra)
	}
	webmail, err := feature.LoadWebmailFile(f.webmailPath)
	if err != nil {
		return exitCode2(fmt.Errorf("load webmail config: %w", err))
	}
	rule, ok := ruleByName(cfg, f.rule)
	if !ok {
		return exitCode2(fmt.Errorf("unknown rule %q (known: %s)", f.rule, strings.Join(ruleNames(cfg), ", ")))
	}

	scorerNames, err := resolveScorerNames(registry, f.scorer)
	if err != nil {
		return exitCode2(err)
	}

	dataset, datasetSHA, labelsSHA, skippedRows, err := loadEvalDataset(f.dataset, f.labels, rule.BenignLabel, brands, webmail, f.skipInvalid)
	if err != nil {
		return exitCode2(err)
	}
	dataset = eval.FilterSplit(dataset, f.split)

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
	var unmatchedFloors []string

	for _, name := range scorerNames {
		scorer, ok := registry.Get(name)
		if !ok {
			return exitCode2(fmt.Errorf("scorer %q is not registered", name))
		}
		// Fix round B3: any scorer other than local needs a cassette (to
		// replay from) or --record (to build one) — never silently fall
		// through to a live, unrecorded call.
		if scorer.Name() != "local" && f.cassettesDir == "" && !f.record {
			return exitCode2(fmt.Errorf("scorer %q is not \"local\" and needs --cassettes (to replay recorded answers) or --record (to make some)", name))
		}
		scorer, err := maybeWrapCassette(scorer, f.cassettesDir, f.record, f.promptVersion)
		if err != nil {
			return exitCode2(err)
		}
		if cs, ok := scorer.(*eval.CassetteScorer); ok {
			cs.Cassette.SetDatasetSHA(datasetSHA) // fix round S8
			if err := cs.Cassette.VerifyDatasetSHA(datasetSHA); err != nil {
				return exitCode2(err)
			}
		}

		res, err := eval.Run(ctx, dataset, rule, scorer, eval.Options{
			Slice:         eval.Slice(f.slice),
			Tiers:         cfg.Tiers,
			PromptVersion: f.promptVersion,
			DatasetSHA:    datasetSHA,
			LabelsSHA:     labelsSHA,
			SplitFilter:   f.split,
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

		if f.floorsPath != "" {
			entry, ok := floors.For(rule.Name, name, f.slice)
			if !ok {
				// Fix round S1: a requested floors file that doesn't
				// cover this (rule, scorer, slice) at all is bad input,
				// not a silently-ungated pass.
				unmatchedFloors = append(unmatchedFloors, fmt.Sprintf("(%s, %s, %s)", rule.Name, name, f.slice))
			} else {
				for _, v := range entry.Check(res.Metrics) {
					violations = append(violations, fmt.Sprintf("(%s, %s, %s) %s", rule.Name, name, f.slice, v))
				}
			}
		}
	}
	if len(unmatchedFloors) > 0 {
		return exitCode2(fmt.Errorf("--floors %s has no entry for: %s", f.floorsPath, strings.Join(unmatchedFloors, ", ")))
	}

	if len(scorerNames) > 1 {
		printScorerTable(os.Stdout, scorerNames, results)
		if f.out != "" {
			if err := writeEvalOutput(f.out, scorerNames, results, skippedRows); err != nil {
				return exitCode2(err)
			}
		}
	} else if err := writeEvalOutput(f.out, scorerNames, results, skippedRows); err != nil {
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
// the event-replay shape (benignLabel threads the scored rule's own
// BenignLabel into neighbour-evidence resolution — fix round B1); unset
// selects the label-snapshot corpus shape (design §4.6's two corpus
// shapes). skipInvalid (fix round P2) makes a schema error non-fatal:
// the partial Dataset (every row that DID parse) is returned alongside
// the row errors instead of an error, for the caller to report as
// `skipped_rows`.
func loadEvalDataset(datasetPath, labelsPath, benignLabel string, brands feature.BrandSet, webmail feature.WebmailSet, skipInvalid bool) (dataset eval.Dataset, datasetSHA, labelsSHA string, skipped []eval.RowError, err error) {
	datasetSHA, err = eval.SHA256File(datasetPath)
	if err != nil {
		return eval.Dataset{}, "", "", nil, fmt.Errorf("hash --dataset %s: %w", datasetPath, err)
	}

	if labelsPath == "" {
		f, err := os.Open(datasetPath)
		if err != nil {
			return eval.Dataset{}, "", "", nil, fmt.Errorf("open --dataset %s: %w", datasetPath, err)
		}
		defer f.Close()
		dataset, rowErrs, err := eval.LoadSnapshotCorpus(f)
		if err != nil {
			if !skipInvalid {
				return eval.Dataset{}, "", "", nil, schemaCLIError(rowErrs, err)
			}
			return dataset, datasetSHA, "", rowErrs, nil
		}
		return dataset, datasetSHA, "", nil, nil
	}

	labelsSHA, err = eval.SHA256File(labelsPath)
	if err != nil {
		return eval.Dataset{}, "", "", nil, fmt.Errorf("hash --labels %s: %w", labelsPath, err)
	}
	eventsF, err := os.Open(datasetPath)
	if err != nil {
		return eval.Dataset{}, "", "", nil, fmt.Errorf("open --dataset %s: %w", datasetPath, err)
	}
	defer eventsF.Close()
	labelsF, err := os.Open(labelsPath)
	if err != nil {
		return eval.Dataset{}, "", "", nil, fmt.Errorf("open --labels %s: %w", labelsPath, err)
	}
	defer labelsF.Close()

	dataset, rowErrs, err := eval.LoadReplayDataset(eval.ReplayInput{
		EventsPath: datasetPath, Events: eventsF,
		LabelsPath: labelsPath, Labels: labelsF,
	}, brands, webmail, benignLabel)
	if err != nil {
		if !skipInvalid {
			return eval.Dataset{}, "", "", nil, schemaCLIError(rowErrs, err)
		}
		return dataset, datasetSHA, labelsSHA, rowErrs, nil
	}
	return dataset, datasetSHA, labelsSHA, nil, nil
}

// schemaCLIError renders a *eval.SchemaError's per-row failures as one
// multi-line message — each RowError already names its own real source
// file and line number (fix round P1: RowError.Error()), so this no
// longer needs (and can no longer get wrong) a caller-supplied label.
// Any other error (a genuine I/O failure) is passed through unchanged.
func schemaCLIError(rowErrs []eval.RowError, err error) error {
	if len(rowErrs) == 0 {
		return err
	}
	lines := make([]string, 0, len(rowErrs))
	for _, re := range rowErrs {
		lines = append(lines, "  "+re.Error())
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

// skippedRowInfo is one --skip-invalid row (fix round P2), embedded into
// run.json as `skipped_rows` alongside whichever scorer(s) actually ran.
type skippedRowInfo struct {
	Source string `json:"source"`
	Line   int    `json:"line"`
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

func toSkippedRowInfo(rowErrs []eval.RowError) []skippedRowInfo {
	if len(rowErrs) == 0 {
		return nil
	}
	out := make([]skippedRowInfo, 0, len(rowErrs))
	for _, re := range rowErrs {
		out = append(out, skippedRowInfo{Source: re.Source, Line: re.Line, Code: re.Code, Detail: re.Err.Error()})
	}
	return out
}

// skippedByCode tallies rowErrs by Code (fix round T2's own
// `skipped_by_code`) — a quick "what kind of thing got skipped, and how
// much of it" summary alongside the full skipped_rows detail.
func skippedByCode(rowErrs []eval.RowError) map[string]int {
	if len(rowErrs) == 0 {
		return nil
	}
	out := make(map[string]int, len(rowErrs))
	for _, re := range rowErrs {
		out[re.Code]++
	}
	return out
}

// singleRunOutput is a single scorer's output shape: RunResult's own
// fields promoted to the top level, plus `skipped_rows`/`skipped_by_code`
// when --skip-invalid dropped any (fix round P2/T2).
type singleRunOutput struct {
	eval.RunResult
	SkippedRows   []skippedRowInfo `json:"skipped_rows,omitempty"`
	SkippedByCode map[string]int   `json:"skipped_by_code,omitempty"`
}

// writeEvalOutput writes run.json (a single scorer) or a
// {"<scorer>": run.json, ...} object (--scorer all) to path, or to
// stdout when path is empty.
func writeEvalOutput(path string, names []string, results map[string]eval.RunResult, skippedRows []eval.RowError) error {
	var payload any
	if len(names) == 1 {
		payload = singleRunOutput{RunResult: results[names[0]], SkippedRows: toSkippedRowInfo(skippedRows), SkippedByCode: skippedByCode(skippedRows)}
	} else {
		out := make(map[string]singleRunOutput, len(names))
		for _, n := range names {
			out[n] = singleRunOutput{RunResult: results[n], SkippedRows: toSkippedRowInfo(skippedRows), SkippedByCode: skippedByCode(skippedRows)}
		}
		payload = out
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
