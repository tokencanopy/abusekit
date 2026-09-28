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
//
// An "abusive" label also propagates to l.Subject's same-tenant neighbours
// (S2 fix round, PropagateToNeighbors): their linked_labelled_abusive_n
// feature just became stale the instant this label landed, and would
// otherwise sit wrong until something else happened to touch them. The
// label itself is already committed by the time propagation runs, so a
// propagation failure is returned alongside the now-valid id rather than
// silently swallowed — the label write did succeed; the caller decides
// whether a failed propagation nudge (which a neighbour's own next real
// event would correct anyway) is worth surfacing or just logging.
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
	if l.Label == "abusive" {
		if perr := s.PropagateToNeighbors(ctx, tenant, l.Subject); perr != nil {
			return id, fmt.Errorf("store: label %d recorded, but propagate to neighbors of %s: %w", id, l.Subject, perr)
		}
	}
	return id, nil
}
