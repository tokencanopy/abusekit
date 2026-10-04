package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/tokencanopy/abusekit/internal/feature/registry"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tokencanopy/abusekit/internal/store"
)

// corpusExportRow is `abusekit corpus export`'s output shape: the
// label-snapshot corpus-v2 schema (design §4.6, eval/schema/
// corpus-v2.schema.json) — this is the SAME shape eval.LoadSnapshotCorpus
// reads, so `abusekit corpus export ... > corpus.jsonl` output is always
// a valid `abusekit eval --dataset corpus.jsonl` input.
type corpusExportRow struct {
	FeatureKeySpace string `json:"feature_key_space"`
	ID              string `json:"id"`
	Input           struct {
		Features map[string]float64  `json:"features"`
		Text     map[string][]string `json:"text,omitempty"`
		Context  string              `json:"context,omitempty"`
	} `json:"input"`
	Label  string         `json:"label"`
	Split  string         `json:"split,omitempty"`
	Source string         `json:"source,omitempty"`
	Meta   map[string]any `json:"meta,omitempty"`
}

// wireEventSlice mirrors internal/serve's own (unexported) shape for one
// corpus_examples.event_slice entry — duplicated here (rather than
// exported from internal/serve, which has no reason to expose a wire
// type this command is the only other reader of) since it's a small,
// stable JSON shape internal/serve's snapshotCorpusExample already
// commits to on disk.
type wireEventSlice struct {
	ID   string         `json:"id"`
	Type string         `json:"type"`
	At   string         `json:"at"`
	Data map[string]any `json:"data"`
}

type corpusFlags struct {
	databaseURL string
	split       string
	tenant      string
}

func parseCorpusExportFlags(args []string) (corpusFlags, error) {
	fs := flag.NewFlagSet("corpus export", flag.ContinueOnError)
	var f corpusFlags
	fs.StringVar(&f.databaseURL, "database-url", os.Getenv("ABUSEKIT_DATABASE_URL"), "Postgres connection string (env ABUSEKIT_DATABASE_URL)")
	fs.StringVar(&f.split, "split", "all", "\"all\", \"train\", or \"test\"")
	fs.StringVar(&f.tenant, "tenant", "", "tenant to export (required — corpus_examples has no cross-tenant default)")
	if err := fs.Parse(args); err != nil {
		return corpusFlags{}, exitCode2(err)
	}
	if f.databaseURL == "" {
		return corpusFlags{}, exitCode2(fmt.Errorf("a database URL is required: pass --database-url or set ABUSEKIT_DATABASE_URL"))
	}
	if f.tenant == "" {
		return corpusFlags{}, exitCode2(fmt.Errorf("--tenant is required"))
	}
	if f.split != "all" && f.split != "train" && f.split != "test" {
		return corpusFlags{}, exitCode2(fmt.Errorf("--split %q must be \"all\", \"train\", or \"test\"", f.split))
	}
	return f, nil
}

// runCorpus dispatches `abusekit corpus <subcommand>`. `export` is the
// only one S4 implements — design §4.9's `abusekit corpus export --split
// all|train|test --schema corpus-v2.json > corpus.jsonl`.
func runCorpus(args []string) error {
	if len(args) == 0 || args[0] != "export" {
		return exitCode2(fmt.Errorf("usage: abusekit corpus export [flags] (only \"export\" is implemented)"))
	}
	return runCorpusExport(args[1:])
}

// runCorpusExport reads every corpus_examples row for --tenant (design
// §4.9's `corpus_examples`, written by internal/serve's POST /v1/labels
// handler) and writes it to stdout as one corpus-v2 JSONL line per row.
//
// Known, deliberate gap (S4 scope decision — not fixed here): design
// §4.9 says "A label enters the gate corpus only after a second source
// (a second operator, or an outcome label) agrees" — nothing in this
// repo yet WRITES corpus_examples.gated=true (no reconciliation pass
// exists to compare a subject's labels across sources and flip it).
// This command exports every row regardless of `gated`, surfacing the
// column in each row's `meta.gated` so a caller can filter or reason
// about it downstream, rather than silently either (a) implementing an
// under-specified agreement policy here or (b) exporting nothing at all
// because no row is ever gated. plan.md's S4 row scopes the harness/gate,
// not this reconciliation pass; a future slice that actually needs
// gate-corpus semantics (S9's incident evaluation, or a real `abusekit
// promote`) should design and implement it there.
func runCorpusExport(args []string) error {
	f, err := parseCorpusExportFlags(args)
	if err != nil {
		return err
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, f.databaseURL)
	if err != nil {
		return exitCode2(fmt.Errorf("connect to database: %w", err))
	}
	defer pool.Close()
	s := store.New(pool)

	rows, err := s.ListCorpusExamples(ctx, f.tenant, f.split)
	if err != nil {
		return exitCode2(fmt.Errorf("list corpus examples: %w", err))
	}

	w := bufio.NewWriter(os.Stdout)
	enc := json.NewEncoder(w)
	for _, row := range rows {
		out, err := corpusRowToExportRow(row)
		if err != nil {
			return exitCode2(fmt.Errorf("corpus example %d (subject %s): %w", row.ID, row.Subject, err))
		}
		if err := enc.Encode(out); err != nil {
			return exitCode2(fmt.Errorf("write output: %w", err))
		}
	}
	return w.Flush()
}

func corpusRowToExportRow(row store.CorpusExportRow) (corpusExportRow, error) {
	if row.FeatureKeySpace != registry.KeySpace {
		return corpusExportRow{}, fmt.Errorf("feature_key_space: cannot export %q as %s", row.FeatureKeySpace, registry.KeySpace)
	}
	var out corpusExportRow
	out.FeatureKeySpace = registry.KeySpace
	out.ID = fmt.Sprintf("corpus_%d", row.ID)
	out.Label = row.Label
	out.Split = row.Split
	out.Source = row.LabelSource
	out.Meta = map[string]any{
		"subject":     row.Subject,
		"decision_at": row.DecisionAt.UTC().Format("2006-01-02T15:04:05.999999999Z"),
		"rule":        row.Rule,
		"gated":       row.Gated,
	}

	if err := json.Unmarshal(row.Features, &out.Input.Features); err != nil {
		return corpusExportRow{}, fmt.Errorf("decode features: %w", err)
	}

	var slice []wireEventSlice
	if err := json.Unmarshal(row.EventSlice, &slice); err != nil {
		return corpusExportRow{}, fmt.Errorf("decode event_slice: %w", err)
	}
	out.Input.Text = textFieldsFromEventSlice(slice)

	return out, nil
}

// textFieldsFromEventSlice mirrors eval.extractTextFields (unexported,
// operates on []event.Event rather than this command's own decoded
// wireEventSlice) — the same suffix convention
// (internal/event's "_skeleton" fields, plus "first_link_host").
func textFieldsFromEventSlice(slice []wireEventSlice) map[string][]string {
	var out map[string][]string
	for _, e := range slice {
		for k, v := range e.Data {
			if !strings.HasSuffix(k, "_skeleton") && k != "first_link_host" {
				continue
			}
			s, ok := v.(string)
			if !ok || s == "" {
				continue
			}
			if out == nil {
				out = map[string][]string{}
			}
			out[k] = append(out[k], s)
		}
	}
	return out
}
