package abusekit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
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
	// EvaluatedNow is true only on Evaluate's response.
	EvaluatedNow bool `json:"evaluated_now,omitempty"`
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
// scores the subject synchronously and returns the fresh verdict.
// deadline <= 0 uses DefaultEvaluateDeadline; deadline is rounded to
// whole milliseconds and capped at DefaultEvaluateDeadline (the server
// rejects anything larger).
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

// SubjectListItem is one row of ListSubjects' response.
type SubjectListItem struct {
	Subject   string     `json:"subject"`
	Tier      string     `json:"tier"`
	Score     float64    `json:"score"`
	VerdictID int64      `json:"verdict_id,omitempty"`
	ScoredAt  *time.Time `json:"scored_at,omitempty"`
}

// ListOptions filters/pages a ListSubjects call (design §4.4). Every field
// is optional; Limit <= 0 uses the server's own default.
type ListOptions struct {
	Tier   string
	Class  string
	Since  time.Time
	Cursor string
	Limit  int
}

// ListResult is ListSubjects' response. NextCursor is "" when there is no
// further page.
type ListResult struct {
	Subjects   []SubjectListItem `json:"subjects"`
	NextCursor string            `json:"-"`
}

// ListSubjects calls GET /v1/subjects (design §4.4). IDEMPOTENT and
// retried on a transport error or 429/5xx.
func (c *Client) ListSubjects(ctx context.Context, opts ListOptions) (ListResult, error) {
	limit := ""
	if opts.Limit > 0 {
		limit = strconv.Itoa(opts.Limit)
	}
	since := ""
	if !opts.Since.IsZero() {
		since = opts.Since.UTC().Format(time.RFC3339)
	}
	query := buildQuery([][2]string{
		{"tier", opts.Tier},
		{"class", opts.Class},
		{"since", since},
		{"cursor", opts.Cursor},
		{"limit", limit},
	})

	respBody, _, err := c.doRetryable(ctx, requestSpec{method: http.MethodGet, path: "/v1/subjects" + query})
	if err != nil {
		return ListResult{}, err
	}
	var wire struct {
		Subjects   []SubjectListItem `json:"subjects"`
		NextCursor *string           `json:"next_cursor"`
	}
	if err := json.Unmarshal(respBody, &wire); err != nil {
		return ListResult{}, fmt.Errorf("abusekit: decode list response: %w", err)
	}
	out := ListResult{Subjects: wire.Subjects}
	if wire.NextCursor != nil {
		out.NextCursor = *wire.NextCursor
	}
	return out, nil
}

// EraseResult is DELETE /v1/subjects/{subject}'s response (design §4.4).
type EraseResult struct {
	Subject       string    `json:"subject"`
	Erased        bool      `json:"erased"`
	Mode          string    `json:"mode"`
	ErasedAt      time.Time `json:"erased_at"`
	AlreadyErased bool      `json:"already_erased"`
}

// Delete calls DELETE /v1/subjects/{subject} (design §4.4's legal erasure
// request; key scope `erase`, operator use only). IDEMPOTENT and retried
// on a transport error or 429/5xx: a repeat call against an
// already-tombstoned subject succeeds again with AlreadyErased:true, and
// a repeat call against an already-purged subject 404s both times
// (store.EraseSubject's own documented behavior) — either way, retrying
// never double-erases anything.
func (c *Client) Delete(ctx context.Context, id string) (EraseResult, error) {
	respBody, _, err := c.doRetryable(ctx, requestSpec{method: http.MethodDelete, path: "/v1/subjects/" + url.PathEscape(id)})
	if err != nil {
		return EraseResult{}, err
	}
	var r EraseResult
	if err := json.Unmarshal(respBody, &r); err != nil {
		return EraseResult{}, fmt.Errorf("abusekit: decode delete response: %w", err)
	}
	return r, nil
}
