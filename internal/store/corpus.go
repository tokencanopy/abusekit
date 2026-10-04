package store

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/tokencanopy/abusekit/internal/feature/registry"
	"time"
)

// CorpusExample is one label's feature/event snapshot (design §4.9: "a
// label writes a corpus example that stores the redacted event slice up
// to decision_at... plus the features as extracted at that time").
type CorpusExample struct {
	Subject    string
	LabelID    int64
	DecisionAt time.Time
	// EventSlice is the already-redacted event history up to DecisionAt,
	// as a JSON-encodable value (internal/serve builds this from
	// StoredEvent; store only marshals and stores it — corpus_examples has
	// no opinion on its internal shape).
	EventSlice any
	Features   map[string]float64
	// Split is "train" or "test" (design §4.9: "split by link cluster
	// (fallback subject) hashed 80/20" — internal/serve computes the
	// split; store just records it).
	Split string
}

// InsertCorpusExample writes one corpus_examples row for tenant (design
// §4.9), returning its assigned id. gated always starts false — a label
// only enters the harness's GATE corpus once a second source agrees
// (design: "A label enters the gate corpus only after a second source...
// agrees"), which is harness territory (S4), not this insert.
func (s *Store) InsertCorpusExample(ctx context.Context, tenant string, ex CorpusExample) (int64, error) {
	if err := registry.ValidateKeys(ex.Features); err != nil {
		return 0, err
	}
	eventSliceJSON, err := json.Marshal(ex.EventSlice)
	if err != nil {
		return 0, fmt.Errorf("store: marshal corpus event slice for %s: %w", ex.Subject, err)
	}
	featuresJSON, err := json.Marshal(ex.Features)
	if err != nil {
		return 0, fmt.Errorf("store: marshal corpus features for %s: %w", ex.Subject, err)
	}

	var id int64
	err = s.pool.QueryRow(ctx, `
		INSERT INTO corpus_examples (tenant, subject, label_id, decision_at, event_slice, features, split, gated, feature_key_space)
		VALUES ($1, $2, $3, $4, $5, $6, $7, false, $8)
		RETURNING id
	`, tenant, ex.Subject, ex.LabelID, ex.DecisionAt, eventSliceJSON, featuresJSON, ex.Split, registry.KeySpace).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store: insert corpus example for %s: %w", ex.Subject, err)
	}
	return id, nil
}

// CorpusExportRow is one corpus_examples row joined with its label
// (design §4.9's "a label writes a corpus example"; §4.10's
// `abusekit corpus export`). EventSlice/Features are left as raw JSON —
// the harness (eval package), not this package, knows how to turn an
// event slice back into text-field values, and re-decoding Features into
// a Go map here would just be thrown away again the moment it's
// re-marshaled for corpus-v2's `input.features`.
type CorpusExportRow struct {
	FeatureKeySpace string
	ID              int64
	Subject         string
	DecisionAt      time.Time
	EventSlice      json.RawMessage
	Features        json.RawMessage
	Split           string
	Label           string
	LabelSource     string
	Rule            string
	Gated           bool
}

// ListCorpusExamples returns every corpus_examples row for tenant,
// optionally restricted to one split ("" or "all" means every split),
// joined with the labels row that produced it, ordered by id (insertion
// order) for a deterministic, reproducible export.
//
// design §4.9: "A label enters the gate corpus only after a second
// source ... agrees" — nothing yet SETS corpus_examples.gated true (no
// writer exists for it anywhere in this repo; see cmd/abusekit/
// corpus_cmd.go's own doc comment on the "second source agreement" gap
// this leaves open). ListCorpusExamples still surfaces the column as-is
// (CorpusExportRow.Gated) rather than filtering on it, so a caller can
// see and reason about the gap instead of silently exporting nothing.
func (s *Store) ListCorpusExamples(ctx context.Context, tenant, split string) ([]CorpusExportRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT c.id, c.subject, c.decision_at, c.event_slice, c.features, c.split, c.gated, c.feature_key_space,
		       l.label, l.source, l.rule
		FROM corpus_examples c
		JOIN labels l ON l.id = c.label_id AND l.tenant = c.tenant
		WHERE c.tenant = $1 AND ($2 = '' OR $2 = 'all' OR c.split = $2)
		ORDER BY c.id ASC
	`, tenant, split)
	if err != nil {
		return nil, fmt.Errorf("store: query corpus examples: %w", err)
	}
	defer rows.Close()

	var out []CorpusExportRow
	for rows.Next() {
		var r CorpusExportRow
		if err := rows.Scan(&r.ID, &r.Subject, &r.DecisionAt, &r.EventSlice, &r.Features, &r.Split, &r.Gated, &r.FeatureKeySpace,
			&r.Label, &r.LabelSource, &r.Rule); err != nil {
			return nil, fmt.Errorf("store: scan corpus example row: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate corpus examples: %w", err)
	}
	return out, nil
}
