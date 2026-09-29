package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/tokencanopy/abusekit/eval"
	"github.com/tokencanopy/abusekit/internal/config"
	"github.com/tokencanopy/abusekit/internal/model"
)

// scoreRow is one `abusekit score --jsonl` input line (design §4.10:
// "External frameworks: abusekit score --jsonl (stdin→stdout, no
// persistence)"). Reuses the label-snapshot corpus's own input shape
// (design §4.6) minus the label/split/source/meta bookkeeping fields —
// score is a raw scoring pipe, not an evaluation.
type scoreRow struct {
	ID    string `json:"id"`
	Input struct {
		Features map[string]float64  `json:"features"`
		Text     map[string][]string `json:"text"`
		Context  string              `json:"context"`
	} `json:"input"`
}

// scoreResultRow is one `abusekit score --jsonl` output line.
type scoreResultRow struct {
	ID         string  `json:"id"`
	Risk       float64 `json:"risk"`
	Tier       string  `json:"tier"`
	Flagged    bool    `json:"flagged"`
	Model      string  `json:"model"`
	Checkpoint string  `json:"checkpoint"`
	Unscored   bool    `json:"unscored,omitempty"`
	ErrorCode  string  `json:"error_code,omitempty"`
}

type scoreFlags struct {
	ruleConfigPaths
	rule          string
	scorer        string
	promptVersion string
}

func parseScoreFlags(args []string) (scoreFlags, error) {
	fs := flag.NewFlagSet("score", flag.ContinueOnError)
	var f scoreFlags
	var jsonl bool
	fs.BoolVar(&jsonl, "jsonl", false, "read/write JSONL (currently the only supported mode; required)")
	fs.StringVar(&f.rulesPath, "rules", envOr("ABUSEKIT_RULES_CONFIG", "config/rules.yaml"), "path to rules.yaml")
	fs.StringVar(&f.vendorsPath, "vendors", envOr("ABUSEKIT_VENDORS_CONFIG", "config/vendors.yaml"), "path to vendors.yaml")
	fs.StringVar(&f.weightsPath, "weights", envOr("ABUSEKIT_LOCAL_WEIGHTS", "config/local_weights.yaml"), "path to the local scorer's weights YAML")
	fs.StringVar(&f.brandsPath, "brands", envOr("ABUSEKIT_BRANDS_CONFIG", "config/brands.yaml"), "path to brands.yaml")
	fs.StringVar(&f.rule, "rule", "", "the rule name (from rules.yaml) to score against (required)")
	fs.StringVar(&f.scorer, "scorer", "", "a registered scorer name (required)")
	fs.StringVar(&f.promptVersion, "prompt-version", "v1", "render/prompt version recorded on ScoreRequest.RenderVersion")
	if err := fs.Parse(args); err != nil {
		return scoreFlags{}, exitCode2(err)
	}
	if !jsonl {
		return scoreFlags{}, exitCode2(fmt.Errorf("--jsonl is required (score has no other mode yet)"))
	}
	if f.rule == "" {
		return scoreFlags{}, exitCode2(fmt.Errorf("--rule is required"))
	}
	if f.scorer == "" {
		return scoreFlags{}, exitCode2(fmt.Errorf("--scorer is required"))
	}
	return f, nil
}

// runScore implements `abusekit score --jsonl`: reads scoreRow JSONL from
// stdin, writes scoreResultRow JSONL to stdout, one line per input line,
// with NO persistence anywhere (design §4.10) — no store, no cassette
// (score is for an external framework driving a scorer directly, not for
// CI's cassette-gated eval path).
//
// A malformed input line is a bad-input failure (exit 2): score does not
// write any partial output before reporting it, so a caller never has to
// guess how many of its already-printed lines are trustworthy.
func runScore(args []string) error {
	f, err := parseScoreFlags(args)
	if err != nil {
		return err
	}

	cfg, registry, _, err := loadRuleConfig(f.ruleConfigPaths)
	if err != nil {
		return exitCode2(fmt.Errorf("load rule config: %w", err))
	}
	rule, ok := ruleByName(cfg, f.rule)
	if !ok {
		return exitCode2(fmt.Errorf("unknown rule %q (known: %s)", f.rule, strings.Join(ruleNames(cfg), ", ")))
	}
	scorer, ok := registry.Get(f.scorer)
	if !ok {
		return exitCode2(fmt.Errorf("unknown scorer %q (registered: %s)", f.scorer, strings.Join(registry.Names(), ", ")))
	}

	rows, err := readScoreRows(os.Stdin)
	if err != nil {
		return exitCode2(err)
	}

	ctx := context.Background()
	out := make([]scoreResultRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, scoreOneRow(ctx, row, rule, scorer, cfg.Tiers, f.promptVersion))
	}

	w := bufio.NewWriter(os.Stdout)
	enc := json.NewEncoder(w)
	for _, r := range out {
		if err := enc.Encode(r); err != nil {
			return exitCode2(fmt.Errorf("write output: %w", err))
		}
	}
	return w.Flush()
}

// scoreOneRow scores one input row via eval.ScoreOne, the same
// per-point scoring path eval.Run uses internally.
func scoreOneRow(ctx context.Context, row scoreRow, rule config.Rule, scorer model.Scorer, tiers config.Tiers, promptVersion string) scoreResultRow {
	pt := eval.Point{Features: row.Input.Features, Text: row.Input.Text, Context: row.Input.Context}
	v, res, err := eval.ScoreOne(ctx, rule, scorer, eval.Options{Tiers: tiers, PromptVersion: promptVersion}, pt)
	out := scoreResultRow{ID: row.ID, Risk: v.Risk, Tier: v.Tier, Flagged: v.Flagged, Model: res.Model, Checkpoint: res.Checkpoint}
	if err != nil {
		out.Unscored = true
		out.ErrorCode = "scorer_error"
		return out
	}
	out.Unscored = v.Unscored
	out.ErrorCode = v.ErrorCode
	return out
}

func readScoreRows(r *os.File) ([]scoreRow, error) {
	var rows []scoreRow
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	line := 0
	for scanner.Scan() {
		line++
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 {
			continue
		}
		var row scoreRow
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&row); err != nil {
			return nil, fmt.Errorf("stdin:%d: invalid JSON or unknown field: %w", line, err)
		}
		if row.ID == "" {
			return nil, fmt.Errorf("stdin:%d: id is required", line)
		}
		rows = append(rows, row)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read stdin: %w", err)
	}
	return rows, nil
}
