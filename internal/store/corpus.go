package store

import (
	"context"
	"encoding/json"
	"fmt"
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
		INSERT INTO corpus_examples (tenant, subject, label_id, decision_at, event_slice, features, split, gated)
		VALUES ($1, $2, $3, $4, $5, $6, $7, false)
		RETURNING id
	`, tenant, ex.Subject, ex.LabelID, ex.DecisionAt, eventSliceJSON, featuresJSON, ex.Split).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store: insert corpus example for %s: %w", ex.Subject, err)
	}
	return id, nil
}
