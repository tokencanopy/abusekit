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

// PutLabel records l for tenant and returns its assigned id.
//
// It also bumps l.Subject's OWN dirty_seq (S3): a label is new evidence
// about the subject itself (design §4.9), so the worker revisits it even
// though no v0 feature currently reads a label directly — this keeps
// "labelling a subject reschedules it" true regardless of which future
// feature ends up caring, rather than silently depending on the subject's
// next unrelated event to ever trigger a fresh round. A subject row that
// doesn't exist yet (a label posted before any event ever arrived for it)
// simply matches zero rows here, the same tolerant no-op
// PropagateToNeighbors already relies on for a subject with no
// neighbours.
//
// An "abusive" label ALSO propagates to l.Subject's same-tenant
// neighbours (S2 fix round, PropagateToNeighbors): their
// linked_labelled_abusive_n feature just became stale the instant this
// label landed, and would otherwise sit wrong until something else
// happened to touch them. The label itself is already committed by the
// time either bump runs, so a failure is returned alongside the
// now-valid id rather than silently swallowed — the label write did
// succeed; the caller decides whether a failed dirty-bump (which the
// subject's own next real event would correct anyway) is worth
// surfacing or just logging.
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
	if _, err := s.pool.Exec(ctx, `
		UPDATE subjects SET dirty_seq = dirty_seq + 1 WHERE tenant = $1 AND subject = $2
	`, tenant, l.Subject); err != nil {
		return id, fmt.Errorf("store: label %d recorded, but bump dirty_seq for %s: %w", id, l.Subject, err)
	}
	if l.Label == "abusive" {
		if perr := s.PropagateToNeighbors(ctx, tenant, l.Subject); perr != nil {
			return id, fmt.Errorf("store: label %d recorded, but propagate to neighbors of %s: %w", id, l.Subject, perr)
		}
	}
	return id, nil
}
