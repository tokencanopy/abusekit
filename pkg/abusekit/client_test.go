package abusekit_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tokencanopy/abusekit/pkg/abusekit"
)

// newTestClient builds a client whose clock advances by one second on
// every call it makes (starting at now) rather than staying fixed: two
// signed requests with an IDENTICAL timestamp would produce an identical
// signature, which internal/serve's replay cache (correctly) rejects as a
// replay on the second one — a real client's wall clock never does this,
// and this test harness shouldn't either. All calls stay trivially within
// the ±5min skew window against the harness's own fixed server clock
// (`now`, used only for feature-window computation, not auth) regardless.
func newTestClient(h *testHarness, now time.Time) *abusekit.Client {
	n := 0
	clock := func() time.Time {
		t := now.Add(time.Duration(n) * time.Second)
		n++
		return t
	}
	return abusekit.New(h.TS.URL, h.Key.ID, h.Key.Secret, abusekit.WithClock(clock))
}

func TestClient_SendEventsHappyPath(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	h := newTestHarness(t, now)
	c := newTestClient(h, now)

	receipt, err := c.SendEvents(context.Background(), []abusekit.Event{
		{ID: "evt-1", Subject: "acct_1", Type: "subject.created", At: now, Data: map[string]any{"channel": "signup"}},
	})
	if err != nil {
		t.Fatalf("SendEvents: %v", err)
	}
	if len(receipt.Accepted) != 1 || receipt.Accepted[0] != "evt-1" {
		t.Fatalf("accepted = %v, want [evt-1]", receipt.Accepted)
	}
	if len(receipt.Duplicates) != 0 || len(receipt.Rejected) != 0 {
		t.Fatalf("unexpected duplicates/rejected: %+v", receipt)
	}
}

func TestClient_SendEventsPartialRejection(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	h := newTestHarness(t, now)
	c := newTestClient(h, now)

	receipt, err := c.SendEvents(context.Background(), []abusekit.Event{
		{ID: "evt-ok", Subject: "acct_partial", Type: "subject.created", At: now},
		{ID: "evt-bad", Subject: "acct_partial", Type: "NOT VALID", At: now},
	})
	if err != nil {
		t.Fatalf("SendEvents: %v", err)
	}
	if len(receipt.Accepted) != 1 || receipt.Accepted[0] != "evt-ok" {
		t.Fatalf("accepted = %v", receipt.Accepted)
	}
	if len(receipt.Rejected) != 1 || receipt.Rejected[0].Index != 1 || receipt.Rejected[0].Code != "bad_type" {
		t.Fatalf("rejected = %+v", receipt.Rejected)
	}
}

func TestClient_SendEventsValidatesBatchSize(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	h := newTestHarness(t, now)
	c := newTestClient(h, now)

	if _, err := c.SendEvents(context.Background(), nil); err == nil {
		t.Fatalf("expected an error for an empty batch")
	}
	tooMany := make([]abusekit.Event, 101)
	for i := range tooMany {
		tooMany[i] = abusekit.Event{ID: "e", Subject: "s", Type: "subject.created", At: now}
	}
	if _, err := c.SendEvents(context.Background(), tooMany); err == nil {
		t.Fatalf("expected an error for a 101-item batch")
	}
}

func TestClient_SubjectRoundTrip(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	h := newTestHarness(t, now)
	c := newTestClient(h, now)

	if _, err := c.SendEvents(context.Background(), []abusekit.Event{
		{ID: "evt-2", Subject: "acct_2", Type: "subject.created", At: now},
	}); err != nil {
		t.Fatalf("SendEvents: %v", err)
	}

	subj, err := c.Subject(context.Background(), "acct_2")
	if err != nil {
		t.Fatalf("Subject: %v", err)
	}
	if subj.Subject != "acct_2" || subj.Tier != "unknown" {
		t.Fatalf("unexpected subject: %+v", subj)
	}
}

func TestClient_SubjectNotFoundIsTypedAPIError(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	h := newTestHarness(t, now)
	c := newTestClient(h, now)

	_, err := c.Subject(context.Background(), "never_seen")
	var apiErr *abusekit.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *abusekit.APIError, got %v (%T)", err, err)
	}
	if apiErr.StatusCode != 404 || apiErr.Code != "not_found" {
		t.Fatalf("unexpected APIError: %+v", apiErr)
	}
}

func TestClient_EvaluateReachesHigh(t *testing.T) {
	all := loadFixtureLines(t, "fast.jsonl")
	var setup []abusekit.Event
	var lastAt time.Time
	for _, line := range all {
		typ, _ := line["type"].(string)
		data, _ := line["data"].(map[string]any)
		if typ == "content.sent" {
			own, _ := data["recipient_is_own_identity"].(bool)
			if !own {
				break
			}
		}
		id, _ := line["id"].(string)
		subject, _ := line["subject"].(string)
		atStr, _ := line["at"].(string)
		at, err := time.Parse(time.RFC3339, atStr)
		if err != nil {
			t.Fatalf("parse fixture at: %v", err)
		}
		var links abusekit.Links
		if l, ok := line["links"].(map[string]any); ok {
			if v, ok := l["email_hash"].(string); ok {
				links.EmailHash = v
			}
			if v, ok := l["card_fingerprint_hash"].(string); ok {
				links.CardFingerprintHash = v
			}
		}
		setup = append(setup, abusekit.Event{ID: id, Subject: subject, Type: typ, At: at, Links: links, Data: data})
		lastAt = at
	}
	if len(setup) == 0 {
		t.Fatalf("fast.jsonl fixture shape assumption broken: zero setup events")
	}
	now := lastAt.Add(time.Second)

	h := newTestHarness(t, now)
	c := newTestClient(h, now)

	if _, err := c.SendEvents(context.Background(), setup); err != nil {
		t.Fatalf("SendEvents: %v", err)
	}

	subj, err := c.Evaluate(context.Background(), "acct_example_fast_1", 3*time.Second)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if subj.Tier != "high" {
		t.Fatalf("tier = %q, want high\nsignals: %+v", subj.Tier, subj.Signals)
	}
	if !subj.EvaluatedNow {
		t.Fatalf("expected EvaluatedNow=true")
	}
}

// TestClient_Label covers POST /v1/labels only — Delete/ListSubjects were
// pulled from this client in the S3 fix round's scope split (X1): see
// feat/s3b-list-erasure and docs/design/notes/erasure-findings.md on that
// branch.
func TestClient_Label(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	h := newTestHarness(t, now)
	c := newTestClient(h, now)

	if _, err := c.SendEvents(context.Background(), []abusekit.Event{
		{ID: "evt-label", Subject: "acct_label", Type: "subject.created", At: now},
	}); err != nil {
		t.Fatalf("SendEvents: %v", err)
	}

	labelResult, err := c.Label(context.Background(), abusekit.LabelRequest{
		Subject: "acct_label", Label: "abusive", Source: "operator", Actor: "ops@example.test",
	})
	if err != nil {
		t.Fatalf("Label: %v", err)
	}
	if labelResult.ID == 0 {
		t.Fatalf("expected a non-zero label id")
	}
}

func TestClient_ForbiddenIsTypedAPIError(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	h := newTestHarness(t, now)
	// A client built with an unknown key: every call should 401, not 403 —
	// covered here to prove APIError decodes an auth failure identically
	// to any other error envelope.
	c := abusekit.New(h.TS.URL, "no_such_key", "wrong-secret", abusekit.WithClock(func() time.Time { return now }))

	_, err := c.Subject(context.Background(), "acct_x")
	var apiErr *abusekit.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *abusekit.APIError, got %v (%T)", err, err)
	}
	if apiErr.StatusCode != 401 || apiErr.Code != "unauthenticated" {
		t.Fatalf("unexpected APIError: %+v", apiErr)
	}
}
