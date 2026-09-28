package serve_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"
)

func eventJSON(id, subject, typ, at string, data map[string]any) map[string]any {
	m := map[string]any{"id": id, "subject": subject, "type": typ, "at": at}
	if data != nil {
		m["data"] = data
	}
	return m
}

func TestEvents_HappyPath(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)

	body, _ := json.Marshal(map[string]any{"events": []map[string]any{
		eventJSON("evt-1", "acct_1", "subject.created", now.Format(time.RFC3339), map[string]any{"channel": "signup"}),
	}})
	req := signedRequest(t, ts.TS, "POST", "/v1/events", body, ts.Keys.Producer, now)
	resp := httpDo(t, req)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 202; body: %s", resp.StatusCode, b)
	}
	var out struct {
		Accepted   []string `json:"accepted"`
		Duplicates []string `json:"duplicates"`
		Rejected   []any    `json:"rejected"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Accepted) != 1 || out.Accepted[0] != "evt-1" {
		t.Fatalf("accepted = %v, want [evt-1]", out.Accepted)
	}
	if len(out.Duplicates) != 0 || len(out.Rejected) != 0 {
		t.Fatalf("unexpected duplicates/rejected: %+v", out)
	}
	if resp.Header.Get("X-Request-Id") == "" {
		t.Fatalf("expected X-Request-Id on the response")
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("expected X-Content-Type-Options: nosniff")
	}

	// The event landed for real: GET must see it.
	view, err := ts.Store.SubjectView(context.Background(), testTenant, "acct_1", nil)
	if err != nil {
		t.Fatalf("SubjectView: %v", err)
	}
	if view.Subject != "acct_1" {
		t.Fatalf("unexpected view: %+v", view)
	}
}

func TestEvents_PartialAcceptance(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)

	body, _ := json.Marshal(map[string]any{"events": []map[string]any{
		eventJSON("evt-ok", "acct_partial", "subject.created", now.Format(time.RFC3339), map[string]any{"channel": "signup"}),
		eventJSON("evt-bad-type", "acct_partial", "NOT A VALID TYPE", now.Format(time.RFC3339), nil),
	}})
	req := signedRequest(t, ts.TS, "POST", "/v1/events", body, ts.Keys.Producer, now)
	resp := httpDo(t, req)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 202; body: %s", resp.StatusCode, b)
	}
	var out struct {
		Accepted []string `json:"accepted"`
		Rejected []struct {
			Index int    `json:"index"`
			Code  string `json:"code"`
		} `json:"rejected"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Accepted) != 1 || out.Accepted[0] != "evt-ok" {
		t.Fatalf("accepted = %v, want [evt-ok]", out.Accepted)
	}
	if len(out.Rejected) != 1 || out.Rejected[0].Index != 1 || out.Rejected[0].Code != "bad_type" {
		t.Fatalf("rejected = %+v, want index=1 code=bad_type", out.Rejected)
	}
}

func TestEvents_ConflictVsDuplicate(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)

	first, _ := json.Marshal(map[string]any{"events": []map[string]any{
		eventJSON("evt-replay", "acct_replay", "subject.created", now.Format(time.RFC3339), map[string]any{"channel": "signup"}),
	}})
	resp1 := httpDo(t, signedRequest(t, ts.TS, "POST", "/v1/events", first, ts.Keys.Producer, now))
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusAccepted {
		t.Fatalf("first status = %d", resp1.StatusCode)
	}

	// Identical body, same id, a moment later: duplicate.
	later := now.Add(time.Second)
	resp2 := httpDo(t, signedRequest(t, ts.TS, "POST", "/v1/events", first, ts.Keys.Producer, later))
	defer resp2.Body.Close()
	var out2 struct {
		Duplicates []string `json:"duplicates"`
	}
	json.NewDecoder(resp2.Body).Decode(&out2)
	if len(out2.Duplicates) != 1 || out2.Duplicates[0] != "evt-replay" {
		t.Fatalf("expected duplicate evt-replay, got %+v", out2)
	}

	// Same id, DIFFERENT body: conflict.
	conflictBody, _ := json.Marshal(map[string]any{"events": []map[string]any{
		eventJSON("evt-replay", "acct_replay", "subject.created", now.Format(time.RFC3339), map[string]any{"channel": "referral"}),
	}})
	resp3 := httpDo(t, signedRequest(t, ts.TS, "POST", "/v1/events", conflictBody, ts.Keys.Producer, later.Add(time.Second)))
	defer resp3.Body.Close()
	var out3 struct {
		Rejected []struct {
			Code string `json:"code"`
		} `json:"rejected"`
	}
	json.NewDecoder(resp3.Body).Decode(&out3)
	if len(out3.Rejected) != 1 || out3.Rejected[0].Code != "conflict" {
		t.Fatalf("expected a conflict rejection, got %+v", out3)
	}
}

func TestEvents_BatchSizeValidation(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)

	empty, _ := json.Marshal(map[string]any{"events": []map[string]any{}})
	resp := httpDo(t, signedRequest(t, ts.TS, "POST", "/v1/events", empty, ts.Keys.Producer, now))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty batch status = %d, want 400", resp.StatusCode)
	}

	tooMany := make([]map[string]any, 101)
	for i := range tooMany {
		tooMany[i] = eventJSON("evt-"+string(rune('a'+i%26))+string(rune('0'+i/26)), "acct_x", "subject.created", now.Format(time.RFC3339), nil)
	}
	body, _ := json.Marshal(map[string]any{"events": tooMany})
	resp2 := httpDo(t, signedRequest(t, ts.TS, "POST", "/v1/events", body, ts.Keys.Producer, now))
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("101-item batch status = %d, want 400", resp2.StatusCode)
	}
}

func TestEvents_WrongContentType(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)

	body, _ := json.Marshal(map[string]any{"events": []map[string]any{
		eventJSON("evt-ct", "acct_ct", "subject.created", now.Format(time.RFC3339), nil),
	}})
	req := signedRequest(t, ts.TS, "POST", "/v1/events", body, ts.Keys.Producer, now)
	req.Header.Set("Content-Type", "text/plain")
	resp := httpDo(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415", resp.StatusCode)
	}
}

func TestEvents_TooLargeBody(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)

	huge := make([]byte, 2<<20) // 2 MiB > MaxEventBatchBody (1 MiB)
	for i := range huge {
		huge[i] = 'a'
	}
	req := signedRequest(t, ts.TS, "POST", "/v1/events", huge, ts.Keys.Producer, now)
	req.Header.Set("Content-Type", "application/json")
	resp := httpDo(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
}

func TestEvents_UnknownFieldRejected(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)

	body := []byte(`{"events":[{"id":"e1","subject":"s","type":"subject.created","at":"2031-01-01T00:00:00Z","totally_unknown_field":1}]}`)
	resp := httpDo(t, signedRequest(t, ts.TS, "POST", "/v1/events", body, ts.Keys.Producer, now))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an unknown top-level field", resp.StatusCode)
	}
}

func TestEvents_BackfillScopeSkipsClockSkew(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)

	old := now.Add(-72 * time.Hour) // outside the ±24h event-level window too, but backfill-scoped
	body, _ := json.Marshal(map[string]any{"events": []map[string]any{
		eventJSON("evt-old", "acct_backfill", "subject.created", old.Format(time.RFC3339), nil),
	}})
	req := signedRequest(t, ts.TS, "POST", "/v1/events", body, ts.Keys.Backfill, now)
	resp := httpDo(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 202; body: %s", resp.StatusCode, b)
	}
	var out struct {
		Accepted []string `json:"accepted"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if len(out.Accepted) != 1 {
		t.Fatalf("expected the old backfilled event accepted, got %+v", out)
	}
}

// TestEvents_NonBackfillKeyRejectsOldTimestamp is the negative control for
// TestEvents_BackfillScopeSkipsClockSkew: the SAME old event, signed by
// the ordinary producer key (no backfill scope), is rejected per-item as
// bad_timestamp rather than silently accepted.
func TestEvents_NonBackfillKeyRejectsOldTimestamp(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)

	old := now.Add(-72 * time.Hour)
	body, _ := json.Marshal(map[string]any{"events": []map[string]any{
		eventJSON("evt-old-2", "acct_backfill_2", "subject.created", old.Format(time.RFC3339), nil),
	}})
	req := signedRequest(t, ts.TS, "POST", "/v1/events", body, ts.Keys.Producer, now)
	resp := httpDo(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (per-item rejection, not a whole-request failure)", resp.StatusCode)
	}
	var out struct {
		Rejected []struct {
			Code string `json:"code"`
		} `json:"rejected"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if len(out.Rejected) != 1 || out.Rejected[0].Code != "bad_timestamp" {
		t.Fatalf("expected a bad_timestamp rejection, got %+v", out)
	}
}
