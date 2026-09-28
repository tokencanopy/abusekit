package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// DefaultNeighborCapPerKey and DefaultNeighborCapTotal are the fan-in caps
// from design §4.2 ("a fan-in cap of 50 per key and a total cap of 200").
// Neighbors uses these when capPerKey/capTotal are <= 0.
const (
	DefaultNeighborCapPerKey = 50
	DefaultNeighborCapTotal  = 200
)

// Neighbors returns every other subject sharing any link key with
// subject (design §4.2's identity graph), deduplicated across keys,
// capped at capPerKey subjects per individual (kind, hash) and capTotal
// overall. truncated is true if either cap was reached, so a caller can
// surface `neighbors_truncated` as a feature rather than silently
// under-counting evidence.
//
// capPerKey <= 0 uses DefaultNeighborCapPerKey; capTotal <= 0 uses
// DefaultNeighborCapTotal.
func (s *Store) Neighbors(ctx context.Context, tenant, subject string, capPerKey, capTotal int) (subjects []string, truncated bool, err error) {
	return s.neighbors(ctx, tenant, subject, capPerKey, capTotal, nil)
}

// NeighborsByKinds is Neighbors restricted to a specific set of link kinds
// (e.g. []string{"card_fingerprint_hash"}), rather than every kind the
// subject happens to have a link row under. internal/feature (S2) uses
// this twice: once with every kind except "asn" for the general linked_*
// evidence (design §4.2's linked_deleted_n/linked_labelled_abusive_n —
// see feature.Config.IncludeASN for why ASN is excluded by default), and
// once with only "card_fingerprint_hash" for
// fingerprint_seen_on_other_subjects, which is specifically about a
// reused payment fingerprint, not general same-tenant link-sharing.
//
// kinds must be non-empty (use Neighbors for "every kind").
func (s *Store) NeighborsByKinds(ctx context.Context, tenant, subject string, kinds []string, capPerKey, capTotal int) (subjects []string, truncated bool, err error) {
	if len(kinds) == 0 {
		return nil, false, fmt.Errorf("store: NeighborsByKinds requires at least one kind (use Neighbors for \"every kind\")")
	}
	return s.neighbors(ctx, tenant, subject, capPerKey, capTotal, kinds)
}

// neighbors is Neighbors/NeighborsByKinds' shared implementation. kinds ==
// nil means "every kind this subject has a link row under" (Neighbors'
// original, unfiltered behavior); a non-nil kinds restricts the key
// listing to exactly those link kinds.
func (s *Store) neighbors(ctx context.Context, tenant, subject string, capPerKey, capTotal int, kinds []string) (subjects []string, truncated bool, err error) {
	if capPerKey <= 0 {
		capPerKey = DefaultNeighborCapPerKey
	}
	if capTotal <= 0 {
		capTotal = DefaultNeighborCapTotal
	}

	// R4 (round 2): ORDER BY kind, hash — SELECT DISTINCT with no ORDER BY
	// has no defined row order, so which keys get processed first (and
	// therefore which of them fill up capTotal before the rest are simply
	// skipped) was not deterministic call to call against unchanged data.
	var keyRows pgx.Rows
	if kinds == nil {
		rows, err := s.pool.Query(ctx,
			`SELECT DISTINCT kind, hash FROM links WHERE tenant = $1 AND subject = $2 ORDER BY kind, hash`, tenant, subject)
		if err != nil {
			return nil, false, fmt.Errorf("store: query link keys for subject %s: %w", subject, err)
		}
		keyRows = rows
	} else {
		rows, err := s.pool.Query(ctx,
			`SELECT DISTINCT kind, hash FROM links WHERE tenant = $1 AND subject = $2 AND kind = ANY($3) ORDER BY kind, hash`,
			tenant, subject, kinds)
		if err != nil {
			return nil, false, fmt.Errorf("store: query link keys for subject %s: %w", subject, err)
		}
		keyRows = rows
	}

	type kindHash struct{ kind, hash string }
	var keys []kindHash
	for keyRows.Next() {
		var kh kindHash
		if err := keyRows.Scan(&kh.kind, &kh.hash); err != nil {
			keyRows.Close()
			return nil, false, fmt.Errorf("store: scan link key: %w", err)
		}
		keys = append(keys, kh)
	}
	keyRows.Close()
	if err := keyRows.Err(); err != nil {
		return nil, false, fmt.Errorf("store: iterate link keys: %w", err)
	}

	seen := map[string]bool{subject: true} // exclude the subject itself
	out := make([]string, 0, capTotal)

	for _, kh := range keys {
		// S13: bound the OUTER loop at capTotal too — once the total cap
		// is already reached, every further per-key query would only
		// discover more truncation, never a new neighbor, so stop issuing
		// them. (Reaching capTotal mid-key, inside the row loop below,
		// still needs its own check to mark truncated and stop reading
		// that key's remaining rows.)
		if len(out) >= capTotal {
			truncated = true
			break
		}

		// S13: ORDER BY last_seen DESC (not subject) — when a key's
		// neighbors exceed capPerKey, the ones worth keeping are the most
		// recently active, not whichever sort alphabetically first.
		// R4 (round 2): ", subject" breaks a last_seen tie deterministically
		// — several neighbors sharing an identical last_seen (routine when
		// they were all touched by the same batch/backfill) otherwise left
		// Postgres free to return them in any order, so which ones survived
		// a capPerKey truncation could vary call to call.
		rows, err := s.pool.Query(ctx, `
			SELECT subject FROM links
			WHERE tenant = $1 AND kind = $2 AND hash = $3 AND subject <> $4
			ORDER BY last_seen DESC, subject
			LIMIT $5
		`, tenant, kh.kind, kh.hash, subject, capPerKey+1)
		if err != nil {
			return nil, false, fmt.Errorf("store: query neighbors for %s/%s: %w", kh.kind, kh.hash, err)
		}

		count := 0
		for rows.Next() {
			var neighbor string
			if err := rows.Scan(&neighbor); err != nil {
				rows.Close()
				return nil, false, fmt.Errorf("store: scan neighbor: %w", err)
			}
			count++
			if count > capPerKey {
				truncated = true
				continue // drop the (capPerKey+1)-th row we only fetched to detect this
			}
			if seen[neighbor] {
				continue
			}
			if len(out) >= capTotal {
				truncated = true
				continue
			}
			seen[neighbor] = true
			out = append(out, neighbor)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, false, fmt.Errorf("store: iterate neighbors for %s/%s: %w", kh.kind, kh.hash, err)
		}
	}

	return out, truncated, nil
}

// PropagateToNeighbors bumps dirty_seq for every same-tenant subject
// sharing any link key with subject (capped at DefaultNeighborCapTotal),
// so their linked_deleted_n/linked_labelled_abusive_n features — computed
// from evidence that just changed for THIS subject — get rescored rather
// than sitting stale until their own next unrelated event (S2 fix round).
// Called after a subject is labelled "abusive" (PutLabel) or permanently
// deleted (AppendEvents' subject.deleted handling), the two events that
// change what a neighbour's linked_* features should read.
//
// Every link kind is considered here, deliberately more inclusive than
// internal/feature.Config's default candidate set: over-propagating just
// means a neighbour gets rescored and its features come out unchanged (a
// wasted but harmless round), whereas under-propagating would leave a
// neighbour's evidence stale until something else happens to touch it.
func (s *Store) PropagateToNeighbors(ctx context.Context, tenant, subject string) error {
	neighbors, _, err := s.Neighbors(ctx, tenant, subject, 0, DefaultNeighborCapTotal)
	if err != nil {
		return fmt.Errorf("store: resolve neighbors to propagate to for %s: %w", subject, err)
	}
	if len(neighbors) == 0 {
		return nil
	}
	// Only dirty_seq is bumped — last_event_at deliberately untouched, so a
	// propagated recompute never masquerades as real subject activity in
	// ClaimDirtySubjects' "oldest dirty" priority ordering.
	if _, err := s.pool.Exec(ctx, `
		UPDATE subjects SET dirty_seq = dirty_seq + 1
		WHERE tenant = $1 AND subject = ANY($2)
	`, tenant, neighbors); err != nil {
		return fmt.Errorf("store: bump dirty_seq for neighbors of %s: %w", subject, err)
	}
	return nil
}

// NeighborOutcomes reports, among the given same-tenant subjects (typically
// a Neighbors/NeighborsByKinds result), how many have ever emitted a
// PERMANENT subject.deleted event and how many carry at least one
// "abusive" label — the two counts internal/feature's linked_deleted_n and
// linked_labelled_abusive_n features need (design §4.2). Both are computed
// with one query each rather than round-tripping once per neighbor.
//
// Only mode="permanent" counts toward deletedCount (N3 fix round): a
// trash-mode deletion is reversible within e2a's own retention window (its
// own soft-deletion design explicitly supports restoring one), so treating
// it as permanent abandonment evidence forever would misjudge any
// restored account for good with no way to un-flag it — there is no
// subject.restored event in the current vocabulary to correct the record
// if we did. Trash-mode deletions are still stored (nothing here erases
// them) for whenever that event exists; this is deliberately the more
// conservative of the two fixes the review offered.
//
// A label row's rule is deliberately not restricted here: a per-rule
// "abusive" verdict from a rule whose own label vocabulary includes it is
// just as much evidence of a labelled-abusive neighbor as a subject-level
// one (rule == "").
//
// subjects empty returns (0, 0, nil) without querying.
func (s *Store) NeighborOutcomes(ctx context.Context, tenant string, subjects []string) (deletedCount, labelledAbusiveCount int, err error) {
	if len(subjects) == 0 {
		return 0, 0, nil
	}
	if err := s.pool.QueryRow(ctx, `
		SELECT COUNT(DISTINCT subject) FROM events
		WHERE tenant = $1 AND subject = ANY($2) AND type = 'subject.deleted'
		  AND data ->> 'mode' = 'permanent'
	`, tenant, subjects).Scan(&deletedCount); err != nil {
		return 0, 0, fmt.Errorf("store: count deleted neighbors: %w", err)
	}
	if err := s.pool.QueryRow(ctx, `
		SELECT COUNT(DISTINCT subject) FROM labels
		WHERE tenant = $1 AND subject = ANY($2) AND label = 'abusive'
	`, tenant, subjects).Scan(&labelledAbusiveCount); err != nil {
		return 0, 0, fmt.Errorf("store: count labelled-abusive neighbors: %w", err)
	}
	return deletedCount, labelledAbusiveCount, nil
}
