package serve_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestAuth_MissingHeadersUnauthenticated(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)

	req, _ := http.NewRequest("GET", ts.TS.URL+"/v1/subjects/acct_1", nil)
	resp := httpDo(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	json.NewDecoder(resp.Body).Decode(&envelope)
	if envelope.Error.Code != "unauthenticated" {
		t.Fatalf("error code = %q, want unauthenticated", envelope.Error.Code)
	}
}

func TestAuth_UnknownKeyUnauthenticated(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)

	req, _ := http.NewRequest("GET", ts.TS.URL+"/v1/subjects/acct_1", nil)
	req.Header.Set("X-Abusekit-Key", "not_a_real_key")
	req.Header.Set("X-Abusekit-Timestamp", now.Format(time.RFC3339))
	req.Header.Set("X-Abusekit-Signature", "0000")
	resp := httpDo(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestAuth_BadSignatureUnauthenticated(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)

	req := signedRequest(t, ts.TS, "GET", "/v1/subjects/acct_1", nil, ts.Keys.Operator, now)
	req.Header.Set("X-Abusekit-Signature", "deadbeef")
	resp := httpDo(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestAuth_TamperedBodyBreaksSignature(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)

	signedFor := []byte(`{"subject":"acct_x","label":"benign","source":"operator","actor":"a"}`)
	req := signedRequest(t, ts.TS, "POST", "/v1/labels", signedFor, ts.Keys.Operator, now)
	// Swap the body after signing — the signature no longer matches.
	tampered := []byte(`{"subject":"acct_x","label":"abusive","source":"operator","actor":"a"}`)
	req2, _ := http.NewRequest("POST", req.URL.String(), bytes.NewReader(tampered))
	req2.Header = req.Header.Clone()
	resp := httpDo(t, req2)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for a tampered body", resp.StatusCode)
	}
}

func TestAuth_TimestampOutsideSkewWindowUnauthenticated(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)

	tooOld := now.Add(-10 * time.Minute)
	req := signedRequest(t, ts.TS, "GET", "/v1/subjects/acct_1", nil, ts.Keys.Operator, tooOld)
	resp := httpDo(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for a stale timestamp", resp.StatusCode)
	}
}

func TestAuth_TimestampWithinSkewWindowAccepted(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)

	withinSkew := now.Add(-4 * time.Minute)
	req := signedRequest(t, ts.TS, "GET", "/v1/subjects/never_seen", nil, ts.Keys.Operator, withinSkew)
	resp := httpDo(t, req)
	defer resp.Body.Close()
	// 404 (never seen), not 401 — proves the request cleared auth.
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (authenticated, subject just doesn't exist)", resp.StatusCode)
	}
}

func TestAuth_ReplayedRequestRejected(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)

	req1 := signedRequest(t, ts.TS, "GET", "/v1/subjects/never_seen", nil, ts.Keys.Operator, now)
	resp1 := httpDo(t, req1)
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusNotFound {
		t.Fatalf("first request status = %d, want 404", resp1.StatusCode)
	}

	// Byte-for-byte identical request (same signature, same timestamp) —
	// a replay within the skew window must be rejected even though the
	// timestamp itself is still fresh.
	req2 := signedRequest(t, ts.TS, "GET", "/v1/subjects/never_seen", nil, ts.Keys.Operator, now)
	resp2 := httpDo(t, req2)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("replay status = %d, want 401", resp2.StatusCode)
	}
}

func TestAuth_ScopeDenialForbidden(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)

	// The producer key has ONLY the `events` scope — GET /v1/subjects
	// requires `read`.
	req := signedRequest(t, ts.TS, "GET", "/v1/subjects/acct_1", nil, ts.Keys.Producer, now)
	resp := httpDo(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	json.NewDecoder(resp.Body).Decode(&envelope)
	if envelope.Error.Code != "forbidden" {
		t.Fatalf("error code = %q, want forbidden", envelope.Error.Code)
	}
}

// TestAuth_CrossTenantReadIs404NotForbidden is the task's own authZ-denial
// scenario: a valid key from a DIFFERENT tenant reading a subject that
// exists in tenant e2a must see 404, not 403 — existence itself is not
// disclosed across tenants.
func TestAuth_CrossTenantReadIs404NotForbidden(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)

	body, _ := json.Marshal(map[string]any{"events": []map[string]any{
		eventJSON("evt-crosstenant", "acct_crosstenant", "subject.created", now.Format(time.RFC3339), nil),
	}})
	resp1 := httpDo(t, signedRequest(t, ts.TS, "POST", "/v1/events", body, ts.Keys.Producer, now))
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusAccepted {
		t.Fatalf("setup: status = %d", resp1.StatusCode)
	}

	// ts.Keys.Other belongs to "other-tenant" and has `read` scope.
	req := signedRequest(t, ts.TS, "GET", "/v1/subjects/acct_crosstenant", nil, ts.Keys.Other, now)
	resp := httpDo(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-tenant read status = %d, want 404 (never disclose cross-tenant existence)", resp.StatusCode)
	}

	// The SAME subject, read by its own tenant's key, is found.
	req2 := signedRequest(t, ts.TS, "GET", "/v1/subjects/acct_crosstenant", nil, ts.Keys.Operator, now)
	resp2 := httpDo(t, req2)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("same-tenant read status = %d, want 200", resp2.StatusCode)
	}
}
