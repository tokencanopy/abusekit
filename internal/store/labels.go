package store

import (
	"context"
	"fmt"
)

// Label is one operator/outcome decision to record (design §4.9). Rule ==
// "" means the label applies to the subject as a whole (benign|abusive),
// rather than to one rule's own label vocabulary.
type Label struct {
	Subject     string
	Rule        string
	Label       string
	Source      string // "operator" | "outcome"
	Actor       string
	Note        string // <= 500 bytes; caller's responsibility to cap (design §4.9)
	EvidenceRef string
}

// PutLabel records l for tenant and returns its assigned id. S1 only
// stores the row; turning a label into a corpus_examples row (design
// §4.9's "a label writes a corpus example...") is S3/S4 work once the
// label API and harness exist to make use of it.
func (s *Store) PutLabel(ctx context.Context, tenant string, l Label) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO labels (tenant, subject, rule, label, source, actor, note, evidence_ref)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id
	`, tenant, l.Subject, l.Rule, l.Label, l.Source, l.Actor, l.Note, l.EvidenceRef).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store: insert label for subject %s: %w", l.Subject, err)
	}
	return id, nil
}
