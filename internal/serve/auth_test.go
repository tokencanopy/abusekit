package serve_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tokencanopy/abusekit/internal/config"
	"github.com/tokencanopy/abusekit/internal/feature"
	"github.com/tokencanopy/abusekit/internal/serve"
	"github.com/tokencanopy/abusekit/internal/worker"
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
	nonce := mustNonce(t)

	req1 := signedRequestWithNonce(t, ts.TS, "GET", "/v1/subjects/never_seen", nil, ts.Keys.Operator, now, nonce)
	resp1 := httpDo(t, req1)
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusNotFound {
		t.Fatalf("first request status = %d, want 404", resp1.StatusCode)
	}

	// Byte-for-byte identical request (same signature, same timestamp,
	// SAME nonce) — a replay within the skew window must be rejected even
	// though the timestamp itself is still fresh.
	req2 := signedRequestWithNonce(t, ts.TS, "GET", "/v1/subjects/never_seen", nil, ts.Keys.Operator, now, nonce)
	resp2 := httpDo(t, req2)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("replay status = %d, want 401", resp2.StatusCode)
	}
}

// TestAuth_SameSecondDifferentNoncesBothSucceed is B1 fix round: two
// legitimate requests signed within the SAME wall-clock second (so an
// RFC3339-second-precision timestamp is identical) must NOT collide as a
// false replay — each has its own nonce.
func TestAuth_SameSecondDifferentNoncesBothSucceed(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)

	req1 := signedRequest(t, ts.TS, "GET", "/v1/subjects/never_seen", nil, ts.Keys.Operator, now)
	resp1 := httpDo(t, req1)
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusNotFound {
		t.Fatalf("first request status = %d, want 404", resp1.StatusCode)
	}

	req2 := signedRequest(t, ts.TS, "GET", "/v1/subjects/never_seen", nil, ts.Keys.Operator, now)
	resp2 := httpDo(t, req2)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("second (different nonce, same second) request status = %d, want 404, not treated as a replay", resp2.StatusCode)
	}
}

// TestAuth_FutureTimestampBeyondSkewRejected is the boundary regression
// check for B1's replayExpiry fix: a timestamp 6 minutes in the future is
// already rejected by the ordinary skew check on its VERY FIRST attempt —
// this never even reaches the replay cache.
func TestAuth_FutureTimestampBeyondSkewRejected(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)

	req := signedRequest(t, ts.TS, "GET", "/v1/subjects/never_seen", nil, ts.Keys.Operator, now.Add(6*time.Minute))
	resp := httpDo(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for a timestamp 6 minutes in the future", resp.StatusCode)
	}
}

// TestAuth_FutureTimestampReplayBlockedPastOldExpiry is B1's actual proven
// bypass: a request signed 4 minutes in the future (still within the ±5min
// skew window) must NOT become replayable again once real wall-clock time
// passes the ORIGINAL now+5min mark — replayExpiry anchors to
// max(now, timestamp)+skew specifically to prevent this.
func TestAuth_FutureTimestampReplayBlockedPastOldExpiry(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	future := now.Add(4 * time.Minute)
	nonce := mustNonce(t)

	// The server's clock ADVANCES across the two calls (unlike most tests'
	// fixed clock) so this test can observe real elapsed time crossing the
	// old (buggy) now+skew expiry boundary.
	var serverNow time.Time
	s := newTestStore(t)
	cfg := loadShippedConfig(t)
	nowFn := func() time.Time { return serverNow }
	w, err := worker.New(worker.Deps{Store: s, Config: cfg, Neighbors: feature.NoNeighbors, Brands: loadShippedBrands(t), Now: nowFn})
	if err != nil {
		t.Fatalf("worker.New: %v", err)
	}
	key := fixedTestKeys().Operator
	srv, err := serve.New(serve.Deps{Store: s, Worker: w, Config: cfg, Keys: map[string]config.Key{key.ID: key}, Neighbors: feature.NoNeighbors, Now: nowFn})
	if err != nil {
		t.Fatalf("serve.New: %v", err)
	}
	t.Cleanup(srv.Close)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	serverNow = now
	req1 := signedRequestWithNonce(t, ts, "GET", "/v1/subjects/never_seen", nil, key, future, nonce)
	resp1 := httpDo(t, req1)
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusNotFound {
		t.Fatalf("first request status = %d, want 404", resp1.StatusCode)
	}

	// Real time advances past the OLD (buggy) expiry of now+5min, but the
	// signed timestamp (future = now+4min) is STILL within ±5min of this
	// new server now (now+5min01s - (now+4min) = 1min01s) — the ordinary
	// skew check alone would accept a replay here.
	serverNow = now.Add(5*time.Minute + time.Second)
	req2 := signedRequestWithNonce(t, ts, "GET", "/v1/subjects/never_seen", nil, key, future, nonce)
	resp2 := httpDo(t, req2)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("replay status = %d, want 401 (replayExpiry must still cover this instant)", resp2.StatusCode)
	}
}

// TestAuth_BackfillReplayBlockedWellPastSkewWindow is B1: a backfill-scoped
// key's replay protection must not depend on TimestampSkew at all (that
// check is skipped entirely for backfill) — a replay attempt long after
// the ordinary 5-minute window (here, 5m01s later) must still be rejected.
func TestAuth_BackfillReplayBlockedWellPastSkewWindow(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	nonce := mustNonce(t)

	var serverNow time.Time
	s := newTestStore(t)
	cfg := loadShippedConfig(t)
	nowFn := func() time.Time { return serverNow }
	w, err := worker.New(worker.Deps{Store: s, Config: cfg, Neighbors: feature.NoNeighbors, Brands: loadShippedBrands(t), Now: nowFn})
	if err != nil {
		t.Fatalf("worker.New: %v", err)
	}
	key := fixedTestKeys().Backfill
	srv, err := serve.New(serve.Deps{Store: s, Worker: w, Config: cfg, Keys: map[string]config.Key{key.ID: key}, Neighbors: feature.NoNeighbors, Now: nowFn})
	if err != nil {
		t.Fatalf("serve.New: %v", err)
	}
	t.Cleanup(srv.Close)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// The backfill test key only has events+backfill scopes (no `read`) —
	// use POST /v1/events, which the replay check runs identically ahead
	// of (authenticate happens before the body is even parsed).
	body := []byte(`{"events":[{"id":"evt-backfill-replay","subject":"acct_x","type":"subject.created","at":"2030-01-01T00:00:00Z"}]}`)

	serverNow = now
	oldTimestamp := now.Add(-72 * time.Hour) // arbitrary, well outside ±5min -- fine, backfill skips that check
	req1 := signedRequestWithNonce(t, ts, "POST", "/v1/events", body, key, oldTimestamp, nonce)
	resp1 := httpDo(t, req1)
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusAccepted {
		t.Fatalf("first request status = %d, want 202", resp1.StatusCode)
	}

	serverNow = now.Add(5*time.Minute + time.Second)
	req2 := signedRequestWithNonce(t, ts, "POST", "/v1/events", body, key, oldTimestamp, nonce)
	resp2 := httpDo(t, req2)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("replay status = %d, want 401 (backfill replay must be blocked well past 5min)", resp2.StatusCode)
	}
}

// TestClient_RetryUsesFreshNonce proves the auth-seam half of B1's client
// retry requirement: a "retry" signed with its OWN fresh nonce (as
// pkg/abusekit's client does per attempt — see its own retry tests) is
// never rejected as a replay, even for the identical logical request.
func TestClient_RetryUsesFreshNonce(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)

	body := []byte(`{"events":[{"id":"evt-retry-nonce","subject":"acct_1","type":"subject.created","at":"2031-01-01T00:00:00Z"}]}`)
	req1 := signedRequest(t, ts.TS, "POST", "/v1/events", body, ts.Keys.Producer, now)
	resp1 := httpDo(t, req1)
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusAccepted {
		t.Fatalf("first attempt status = %d, want 202", resp1.StatusCode)
	}

	// A "retry" of the identical logical request, but with its OWN fresh
	// nonce — must succeed (idempotent: the SAME event id, so it lands as
	// a duplicate) rather than being rejected as a replay.
	req2 := signedRequest(t, ts.TS, "POST", "/v1/events", body, ts.Keys.Producer, now)
	resp2 := httpDo(t, req2)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusAccepted {
		t.Fatalf("retry (fresh nonce) status = %d, want 202", resp2.StatusCode)
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
