package abusekit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// LabelRequest is POST /v1/labels' body (design §4.9). Rule, Note, and
// EvidenceRef are optional; with Rule empty, Label must be "benign" or
// "abusive" (the subject-level vocabulary) — a rule-scoped label's
// vocabulary is that rule's own configured `labels`.
type LabelRequest struct {
	Subject     string `json:"subject"`
	Rule        string `json:"rule,omitempty"`
	Label       string `json:"label"`
	Source      string `json:"source"` // "operator" | "outcome"
	Actor       string `json:"actor"`
	Note        string `json:"note,omitempty"`
	EvidenceRef string `json:"evidence_ref,omitempty"`
}

// LabelResult is POST /v1/labels' response.
type LabelResult struct {
	ID int64 `json:"id"`
	// CorpusExampleID is 0 if the server couldn't snapshot a corpus
	// example (design §4.9) for this label — the label itself is still
	// recorded either way; see internal/serve's own doc comment on why
	// that failure mode doesn't fail the label write.
	CorpusExampleID int64 `json:"corpus_example_id,omitempty"`
}

// Label calls POST /v1/labels (design §4.9; key scope `labels`).
//
// NOT retried: each call creates a NEW label row (there is no
// caller-supplied idempotency key for a label the way there is for an
// event's id) — retrying a request that actually succeeded server-side
// but whose response was lost in transit would double-record the label.
// A caller that needs at-most-once semantics under retry should generate
// its own dedupe key server-side (e.g. checking GET /v1/subjects first)
// rather than relying on this client to retry safely.
func (c *Client) Label(ctx context.Context, req LabelRequest) (LabelResult, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return LabelResult{}, fmt.Errorf("abusekit: marshal label request: %w", err)
	}
	respBody, _, err := c.do(ctx, requestSpec{method: http.MethodPost, path: "/v1/labels", body: body})
	if err != nil {
		return LabelResult{}, err
	}
	var r LabelResult
	if err := json.Unmarshal(respBody, &r); err != nil {
		return LabelResult{}, fmt.Errorf("abusekit: decode label response: %w", err)
	}
	return r, nil
}
