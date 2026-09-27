package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tokencanopy/abusekit/internal/event"
)

// RejectedEvent is one item AppendEvents could not accept, mirroring the
// `rejected: [{index, code, message}]` shape from design §4.3. Callers at
// the HTTP layer (S3) map Code directly onto the wire.
type RejectedEvent struct {
	Index   int
	ID      string
	Code    string
	Message string
}

// AppendResult is AppendEvents' outcome for one batch.
type AppendResult struct {
	Accepted   []string // event ids newly inserted
	Duplicates []string // event ids that were exact replays (same id, identical body)
	Rejected   []RejectedEvent
}

// AppendEvents inserts a batch of already-validated-and-redacted events
// (callers run event.Event.Validate + Redact before this — AppendEvents
// only handles what a store, not an ingest layer, can decide: identity
// and idempotency) for one (tenant, producer), applying design §4.3's
// idempotency rule: the same id with an identical body is `duplicate`;
// the same id with a different body is `conflict` (returned in Rejected
// with that code, never as an error — a conflict is an expected, per-item
// outcome the caller reports back to the producer, not a store failure).
//
// Every accepted event also, in the same transaction:
//   - bumps subjects.dirty_seq for e.Subject (creating the subject row on
//     first sight), which is what makes the S2 worker's queue selection
//     ("dirty_seq > scored_seq") pick it up;
//   - updates subjects.class when the event is `subject.class` and
//     e.Data["class"] is a string (design §4.3: "internal/synthetic
//     subjects are stored but never scored" — the worker, S2, is what
//     actually honours class; this method only records it);
//   - upserts one links row per present (kind, hash) on e.Links.
//
// The whole batch commits or rolls back together: a caller that wants
// per-event durability independent of a batch failure should call this
// once per event, at the cost of losing the (small) efficiency of a
// shared transaction.
//
// AppendEvents never returns a non-empty AppendResult alongside a non-nil
// error (design §4.2/§4.3: per-item rejection is how a partial batch is
// reported — a batch-level error means the whole transaction rolled back,
// so any result accumulated so far never actually committed and must not
// be handed back as if it had).
func (s *Store) AppendEvents(ctx context.Context, tenant, producer string, events []event.Event) (AppendResult, error) {
	if len(events) == 0 {
		return AppendResult{}, nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AppendResult{}, fmt.Errorf("store: begin append transaction: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	var result AppendResult
	for i, e := range events {
		bodyHash, err := e.BodyHash()
		if err != nil {
			result.Rejected = append(result.Rejected, RejectedEvent{
				Index: i, ID: e.ID, Code: string(event.CodeRedactionFailed), Message: err.Error(),
			})
			continue
		}
		linksJSON, err := json.Marshal(e.Links)
		if err != nil {
			return AppendResult{}, fmt.Errorf("store: marshal links for event %s: %w", e.ID, err)
		}
		dataJSON, err := json.Marshal(e.Data)
		if err != nil {
			return AppendResult{}, fmt.Errorf("store: marshal data for event %s: %w", e.ID, err)
		}

		var seq int64
		err = tx.QueryRow(ctx, `
			INSERT INTO events (tenant, producer, id, subject, type, at, links, data, body_hash, redaction_version)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			ON CONFLICT (tenant, producer, id) DO NOTHING
			RETURNING seq
		`, tenant, producer, e.ID, e.Subject, e.Type, e.At, linksJSON, dataJSON, bodyHash, event.RedactionSchemaVersion).Scan(&seq)

		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				return AppendResult{}, fmt.Errorf("store: insert event %s: %w", e.ID, err)
			}
			// ON CONFLICT DO NOTHING means no row was returned: this
			// (tenant, producer, id) already exists. Decide
			// duplicate vs conflict by comparing body hashes.
			var existingHash string
			lookupErr := tx.QueryRow(ctx,
				`SELECT body_hash FROM events WHERE tenant = $1 AND producer = $2 AND id = $3`,
				tenant, producer, e.ID,
			).Scan(&existingHash)
			if lookupErr != nil {
				return AppendResult{}, fmt.Errorf("store: look up existing event %s: %w", e.ID, lookupErr)
			}
			if existingHash == bodyHash {
				result.Duplicates = append(result.Duplicates, e.ID)
			} else {
				result.Rejected = append(result.Rejected, RejectedEvent{
					Index: i, ID: e.ID, Code: string(event.CodeConflict),
					Message: "event id already used with a different body",
				})
			}
			continue
		}

		if err := touchSubjectTx(ctx, tx, tenant, e.Subject, e.At); err != nil {
			return AppendResult{}, err
		}
		if e.Type == "subject.class" {
			if class, ok := e.Data["class"].(string); ok && class != "" {
				if _, err := tx.Exec(ctx,
					`UPDATE subjects SET class = $1 WHERE tenant = $2 AND subject = $3`,
					class, tenant, e.Subject,
				); err != nil {
					return AppendResult{}, fmt.Errorf("store: update subject class for %s: %w", e.Subject, err)
				}
			}
		}
		for _, kh := range e.Links.Kinds() {
			if err := upsertLinkTx(ctx, tx, tenant, kh.Kind, kh.Hash, e.Subject, e.At); err != nil {
				return AppendResult{}, err
			}
		}

		result.Accepted = append(result.Accepted, e.ID)
	}

	if err := tx.Commit(ctx); err != nil {
		return AppendResult{}, fmt.Errorf("store: commit append transaction: %w", err)
	}
	return result, nil
}

func touchSubjectTx(ctx context.Context, tx pgx.Tx, tenant, subject string, at time.Time) error {
	// S7: first_seen_at uses LEAST(existing, new) rather than "whatever was
	// there at first INSERT" — events don't always arrive in `at` order
	// (a backfill, a retried batch, or plain network reordering can
	// deliver an earlier event after a later one), and first_seen_at
	// feeds subject-age features that must reflect the account's true
	// earliest known activity, not just the earliest EVENT DELIVERY.
	_, err := tx.Exec(ctx, `
		INSERT INTO subjects (tenant, subject, dirty_seq, first_seen_at, last_event_at)
		VALUES ($1, $2, 1, $3, $3)
		ON CONFLICT (tenant, subject) DO UPDATE SET
			dirty_seq     = subjects.dirty_seq + 1,
			first_seen_at = LEAST(subjects.first_seen_at, EXCLUDED.first_seen_at),
			last_event_at = GREATEST(subjects.last_event_at, EXCLUDED.last_event_at)
	`, tenant, subject, at)
	if err != nil {
		return fmt.Errorf("store: touch subject %s: %w", subject, err)
	}
	return nil
}

func upsertLinkTx(ctx context.Context, tx pgx.Tx, tenant, kind, hash, subject string, at time.Time) error {
	// S7: same LEAST(existing, new) fix as touchSubjectTx, for the same
	// out-of-order-delivery reason.
	_, err := tx.Exec(ctx, `
		INSERT INTO links (tenant, kind, hash, subject, first_seen, last_seen)
		VALUES ($1, $2, $3, $4, $5, $5)
		ON CONFLICT (tenant, kind, hash, subject) DO UPDATE SET
			first_seen = LEAST(links.first_seen, EXCLUDED.first_seen),
			last_seen  = GREATEST(links.last_seen, EXCLUDED.last_seen)
	`, tenant, kind, hash, subject, at)
	if err != nil {
		return fmt.Errorf("store: upsert link %s/%s for subject %s: %w", kind, hash, subject, err)
	}
	return nil
}

// StoredEvent pairs a decoded event.Event with the store-assigned
// ReceivedAt timestamp.
type StoredEvent struct {
	Event      event.Event
	ReceivedAt time.Time
}

// EventsForSubject returns every event stored for (tenant, subject),
// oldest first (by event time, then insertion order for same-timestamp
// events). This is the raw material internal/feature (S2) extracts
// windows from; S1 has no caller for it yet beyond tests, but the method
// belongs to the store's contract regardless of who calls it first.
func (s *Store) EventsForSubject(ctx context.Context, tenant, subject string) ([]StoredEvent, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, subject, type, at, links, data, received_at
		FROM events
		WHERE tenant = $1 AND subject = $2
		ORDER BY at ASC, seq ASC
	`, tenant, subject)
	if err != nil {
		return nil, fmt.Errorf("store: query events for subject %s: %w", subject, err)
	}
	defer rows.Close()

	var out []StoredEvent
	for rows.Next() {
		var (
			se                  StoredEvent
			linksJSON, dataJSON []byte
		)
		if err := rows.Scan(&se.Event.ID, &se.Event.Subject, &se.Event.Type, &se.Event.At,
			&linksJSON, &dataJSON, &se.ReceivedAt); err != nil {
			return nil, fmt.Errorf("store: scan event row: %w", err)
		}
		if err := json.Unmarshal(linksJSON, &se.Event.Links); err != nil {
			return nil, fmt.Errorf("store: decode links for event %s: %w", se.Event.ID, err)
		}
		if err := json.Unmarshal(dataJSON, &se.Event.Data); err != nil {
			return nil, fmt.Errorf("store: decode data for event %s: %w", se.Event.ID, err)
		}
		out = append(out, se)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate events for subject %s: %w", subject, err)
	}
	return out, nil
}
