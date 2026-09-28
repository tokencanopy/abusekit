package abusekit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Signal is one rule's contribution to a Subject's score (design §4.4).
// Risk/Flagged are nil when Status is "unscored".
type Signal struct {
	Rule        string   `json:"rule"`
	Mode        string   `json:"mode,omitempty"`
	Status      string   `json:"status"`
	Risk        *float64 `json:"risk,omitempty"`
	Flagged     *bool    `json:"flagged,omitempty"`
	Model       string   `json:"model,omitempty"`
	Checkpoint  string   `json:"checkpoint,omitempty"`
	Calibration string   `json:"calibration,omitempty"`
	Reason      string   `json:"reason,omitempty"`
	ErrorCode   string   `json:"error_code,omitempty"`
}

// Subject is the response shape of GET /v1/subjects/{subject} and
// POST /v1/subjects/{subject}/evaluate (design §4.4).
type Subject struct {
	Subject          string     `json:"subject"`
	Score            float64    `json:"score"`
	Tier             string     `json:"tier"`
	Degraded         bool       `json:"degraded"`
	Stale            bool       `json:"stale"`
	EventsSinceScore int64      `json:"events_since_score"`
	ScoredAt         *time.Time `json:"scored_at,omitempty"`
	Signals          []Signal   `json:"signals"`
	// EvaluatedNow is true only on Evaluate's response, when it produced a
	// genuinely fresh round.
	EvaluatedNow bool `json:"evaluated_now,omitempty"`
	// EvaluateNote explains why Evaluate did NOT produce a fresh round
	// (S1 fix round) even though the call itself succeeded (200, not an
	// error) — currently only "deadline_exceeded". Empty on GET's response
	// and on a successful (EvaluatedNow true) Evaluate response.
	EvaluateNote string `json:"evaluate_note,omitempty"`
}

// ErrNotFound-style checking: callers should check errors.As(err,
// *APIError) and compare Code == "not_found" rather than a sentinel, since
// abusekit's wire codes (design §4.3) are the actual contract — see
// APIError's own doc comment.

// Subject fetches GET /v1/subjects/{subject} (design §4.4). IDEMPOTENT and
// retried on a transport error or 429/5xx.
func (c *Client) Subject(ctx context.Context, id string) (Subject, error) {
	respBody, _, err := c.doRetryable(ctx, requestSpec{method: http.MethodGet, path: "/v1/subjects/" + url.PathEscape(id)})
	if err != nil {
		return Subject{}, err
	}
	var s Subject
	if err := json.Unmarshal(respBody, &s); err != nil {
		return Subject{}, fmt.Errorf("abusekit: decode subject response: %w", err)
	}
	return s, nil
}

// DefaultEvaluateDeadline is design §4.4's evaluate deadline ceiling.
const DefaultEvaluateDeadline = 3000 * time.Millisecond

// Evaluate calls POST /v1/subjects/{subject}/evaluate (design §4.4):
// scores the subject synchronously when possible and returns the current
// view either way (S1 fix round: a subject that's class internal/
// synthetic, busy, or cut short by the deadline is still a 200 with the
// stored view and EvaluatedNow=false — see Subject.EvaluateNote — not an
// error). deadline <= 0 uses DefaultEvaluateDeadline; deadline is rounded
// to whole milliseconds and capped at DefaultEvaluateDeadline (the server
// rejects anything larger).
//
// A 409 (the subject is busy — claimed elsewhere, or in failure backoff)
// still returns as an *APIError with Code "subject_busy" and a real
// RetryAfter, since that genuinely is a "try again shortly" condition
// distinct from "here is the subject's current state."
//
// NOT retried: unlike a read, a retried evaluate call could be denied by
// the server's own 1/s per-subject rate limit (design §4.4) precisely
// because the first attempt actually succeeded — a caller that wants a
// fresh score after a transient failure should simply call Evaluate again
// itself, rather than have this client silently spend its one-per-second
// budget on a retry.
func (c *Client) Evaluate(ctx context.Context, id string, deadline time.Duration) (Subject, error) {
	if deadline <= 0 || deadline > DefaultEvaluateDeadline {
		deadline = DefaultEvaluateDeadline
	}
	body, err := json.Marshal(map[string]any{"deadline_ms": deadline.Milliseconds()})
	if err != nil {
		return Subject{}, fmt.Errorf("abusekit: marshal evaluate request: %w", err)
	}
	respBody, _, err := c.do(ctx, requestSpec{method: http.MethodPost, path: "/v1/subjects/" + url.PathEscape(id) + "/evaluate", body: body})
	if err != nil {
		return Subject{}, err
	}
	var s Subject
	if err := json.Unmarshal(respBody, &s); err != nil {
		return Subject{}, fmt.Errorf("abusekit: decode evaluate response: %w", err)
	}
	return s, nil
}
