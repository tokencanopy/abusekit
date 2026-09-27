package store

import (
	"context"
	"fmt"
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
	keyRows, err := s.pool.Query(ctx,
		`SELECT DISTINCT kind, hash FROM links WHERE tenant = $1 AND subject = $2 ORDER BY kind, hash`, tenant, subject)
	if err != nil {
		return nil, false, fmt.Errorf("store: query link keys for subject %s: %w", subject, err)
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
