package abusekit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Links carries the keyed-hash identity signals design §4.2 defines. Every
// field is optional.
type Links struct {
	EmailHash           string `json:"email_hash,omitempty"`
	CardFingerprintHash string `json:"card_fingerprint_hash,omitempty"`
	IP24Hash            string `json:"ip24_hash,omitempty"`
	ASN                 string `json:"asn,omitempty"`
	UAHash              string `json:"ua_hash,omitempty"`
	DeviceHash          string `json:"device_hash,omitempty"`
}

// Event is one item of a POST /v1/events batch (design §4.3).
type Event struct {
	ID      string         `json:"id"`
	Subject string         `json:"subject"`
	Type    string         `json:"type"`
	At      time.Time      `json:"at"`
	Links   Links          `json:"links,omitempty"`
	Data    map[string]any `json:"data,omitempty"`
}

// RejectedEvent is one item POST /v1/events could not accept.
type RejectedEvent struct {
	Index   int    `json:"index"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Receipt is POST /v1/events' response (design §4.3).
type Receipt struct {
	Accepted   []string        `json:"accepted"`
	Duplicates []string        `json:"duplicates"`
	Rejected   []RejectedEvent `json:"rejected"`
}

// MaxEventBatch mirrors design §4.3's "batch of 1–100" so a caller can
// self-check before calling SendEvents, without importing anything server-
// side to learn the number.
const MaxEventBatch = 100

// SendEvents posts a batch of 1..MaxEventBatch events. IDEMPOTENT and
// retried on a transport error or 429/5xx (WithMaxRetries): every event
// carries its own id, and the server dedupes on (tenant, producer, id) —
// design §4.3 — so a retried batch that partially landed the first time
// simply reports its already-accepted ids back as `duplicates` the second
// time, never double-processing anything.
func (c *Client) SendEvents(ctx context.Context, events []Event) (Receipt, error) {
	if len(events) == 0 || len(events) > MaxEventBatch {
		return Receipt{}, fmt.Errorf("abusekit: SendEvents requires 1..%d events, got %d", MaxEventBatch, len(events))
	}
	body, err := json.Marshal(map[string]any{"events": events})
	if err != nil {
		return Receipt{}, fmt.Errorf("abusekit: marshal events: %w", err)
	}
	respBody, _, err := c.doRetryable(ctx, requestSpec{method: http.MethodPost, path: "/v1/events", body: body})
	if err != nil {
		return Receipt{}, err
	}
	var receipt Receipt
	if err := json.Unmarshal(respBody, &receipt); err != nil {
		return Receipt{}, fmt.Errorf("abusekit: decode events response: %w", err)
	}
	return receipt, nil
}
