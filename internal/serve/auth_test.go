package serve_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

// TestAuth_BackfillReplayBlockedWellPastSkewWindow is B1 (updated by R5,
// round 2 fix round, for BackfillEventsTimestampWindow's now-bounded ±24h
// backfill window on POST /v1/events, replacing the old "skips the clock-
// skew check entirely"): a backfill-scoped key's replay protection must
// not depend on the ORDINARY TimestampSkew at all — a replay attempt long
// after the ordinary 5-minute window (here, 5m01s later), but still well
// within the wider backfill window, must still be rejected.
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
	// Well outside ±5min (the ordinary skew), but within R5's ±24h
	// BackfillEventsTimestampWindow — a realistic historical-backfill
	// timestamp, not one so old it would now be rejected outright.
	oldTimestamp := now.Add(-20 * time.Hour)
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

// TestAuth_PreAuthLimiterOnlyCountsFailedAuthentications is R4 (round 2 fix
// round)'s literal repro: a flood of FAILED-auth requests from one IP must
// never block a LATER, correctly-signed request from that same IP — the
// pre-auth bucket only ever consumes on a failure, never a success. Before
// this fix, the pre-auth limiter consumed a slot for EVERY request
// (successful or not) BEFORE authenticate ever ran, so exhausting the
// bucket with garbage also meant a real producer sharing that IP (a NAT
// gateway, a corporate proxy) got 429'd on a perfectly valid signed
// request too.
func TestAuth_PreAuthLimiterOnlyCountsFailedAuthentications(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)

	// 300 unauthenticated requests (unknown key id) — well past
	// serve.DefaultPreAuthPerIPPerSecond (200) — from the same IP
	// (httptest.Server requests are all loopback, so they naturally share
	// one clientIP). Each is expected to fail auth outright: 401 while the
	// bucket still has room, 429 once it's been exhausted by this very
	// flood — both are fine here, this loop is only building up the
	// failure count the fix must not let leak onto a real request.
	for i := 0; i < 300; i++ {
		req, _ := http.NewRequest("GET", ts.TS.URL+"/v1/subjects/acct_1", nil)
		req.Header.Set("X-Abusekit-Key", "not_a_real_key")
		req.Header.Set("X-Abusekit-Timestamp", now.Format(time.RFC3339))
		req.Header.Set("X-Abusekit-Signature", "0000")
		resp := httpDo(t, req)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("attempt %d: status = %d, want 401 or 429", i, resp.StatusCode)
		}
	}

	// A real, correctly-signed request from the SAME IP right afterward
	// must NOT be rate-limited — it authenticates and falls through to the
	// ordinary 404 (the subject just doesn't exist), never a 429.
	req := signedRequest(t, ts.TS, "GET", "/v1/subjects/never_seen_preauth", nil, ts.Keys.Operator, now)
	resp := httpDo(t, req)
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		t.Fatalf("a validly-signed request was rate-limited (429) after 300 failed attempts from the same IP")
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (authenticated, subject just doesn't exist)", resp.StatusCode)
	}
}

// TestAuth_OversizedNonceUnauthenticated is R5 (round 2 fix round): a
// nonce past MaxNonceHexLen is rejected outright, the same as one too
// short — bounding the cost of hex-validating/hashing an adversarially
// oversized header value.
func TestAuth_OversizedNonceUnauthenticated(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)

	oversized := strings.Repeat("a", serve.MaxNonceHexLen+2)
	req := signedRequestWithNonce(t, ts.TS, "GET", "/v1/subjects/acct_1", nil, ts.Keys.Operator, now, oversized)
	resp := httpDo(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for a nonce past MaxNonceHexLen", resp.StatusCode)
	}
}

// TestAuth_BackfillWidenedWindowIsEventsOnly is R5 (round 2 fix round): a
// backfill-scoped key's widened ±24h timestamp window applies ONLY to POST
// /v1/events — the SAME key, SAME old timestamp, against a DIFFERENT
// endpoint it also has scope for, must use the ordinary ±5min skew and be
// rejected.
func TestAuth_BackfillWidenedWindowIsEventsOnly(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)

	// Within BackfillEventsTimestampWindow (24h) but well outside the
	// ordinary ±5min TimestampSkew.
	old := now.Add(-20 * time.Hour)

	body, _ := json.Marshal(map[string]any{"events": []map[string]any{
		eventJSON("evt-backfill-window", "acct_backfill_window", "subject.created", now.Format(time.RFC3339), nil),
	}})
	eventsReq := signedRequest(t, ts.TS, "POST", "/v1/events", body, ts.Keys.BackfillRead, old)
	eventsResp := httpDo(t, eventsReq)
	defer eventsResp.Body.Close()
	if eventsResp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /v1/events with a %v-old backfill-scoped timestamp: status = %d, want 202 (within the widened events-only window)", now.Sub(old), eventsResp.StatusCode)
	}

	readReq := signedRequest(t, ts.TS, "GET", "/v1/subjects/acct_backfill_window", nil, ts.Keys.BackfillRead, old)
	readResp := httpDo(t, readReq)
	defer readResp.Body.Close()
	if readResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /v1/subjects with the SAME backfill key and the SAME %v-old timestamp: status = %d, want 401 (ordinary ±5min skew applies outside /v1/events)", now.Sub(old), readResp.StatusCode)
	}
}

// TestAuth_ScopeDenialDoesNotBurnNonce is R5 (round 2 fix round): a request
// rejected for lacking scope (403 — a real, identified key's own mistake,
// not anonymous flood traffic) must not consume its nonce in the replay
// cache — the SAME nonce, freshly signed for an endpoint the key DOES have
// scope for, must still succeed rather than hitting "replay detected".
func TestAuth_ScopeDenialDoesNotBurnNonce(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)
	nonce := mustNonce(t)

	// ts.Keys.Operator has read+labels, NOT events — POST /v1/events must
	// be forbidden.
	body, _ := json.Marshal(map[string]any{"events": []map[string]any{
		eventJSON("evt-scope-denial", "acct_scope_denial", "subject.created", now.Format(time.RFC3339), nil),
	}})
	forbiddenReq := signedRequestWithNonce(t, ts.TS, "POST", "/v1/events", body, ts.Keys.Operator, now, nonce)
	forbiddenResp := httpDo(t, forbiddenReq)
	forbiddenResp.Body.Close()
	if forbiddenResp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (Operator key lacks events scope)", forbiddenResp.StatusCode)
	}

	// The SAME nonce, freshly signed for a DIFFERENT request (a different
	// method+path changes the signature, but the nonce header is reused
	// verbatim) that Operator DOES have scope for, must authenticate
	// normally — not "replay detected".
	readReq := signedRequestWithNonce(t, ts.TS, "GET", "/v1/subjects/never_seen_scope_denial", nil, ts.Keys.Operator, now, nonce)
	readResp := httpDo(t, readReq)
	defer readResp.Body.Close()
	if readResp.StatusCode == http.StatusUnauthorized {
		t.Fatalf("reusing a nonce from a SCOPE-DENIED (403) attempt was rejected as a replay; scope denials must not burn the nonce")
	}
	if readResp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (authenticated, subject just doesn't exist)", readResp.StatusCode)
	}
}

// TestAuth_UniformMessageForEveryPreScopeFailure is R7 (round 2 fix
// round): every failure that happens BEFORE the scope check — missing
// headers, a malformed nonce, an unknown key, a malformed timestamp, a
// timestamp outside the accepted window, and a bad signature — must return
// the IDENTICAL 401 message, not a distinct one per cause. Before this
// fix, a caller probing a request could tell how many of (key id exists,
// timestamp is well-formed, timestamp is fresh, signature matches) it got
// right from which distinct message came back.
func TestAuth_UniformMessageForEveryPreScopeFailure(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)

	const wantMessage = "invalid key or signature"
	type envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	decode := func(t *testing.T, resp *http.Response) envelope {
		t.Helper()
		var e envelope
		if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
			t.Fatalf("decode error envelope: %v", err)
		}
		return e
	}

	cases := []struct {
		name string
		req  *http.Request
	}{
		{
			name: "missing headers",
			req: func() *http.Request {
				req, _ := http.NewRequest("GET", ts.TS.URL+"/v1/subjects/acct_1", nil)
				return req
			}(),
		},
		{
			name: "malformed nonce (too short)",
			req:  signedRequestWithNonce(t, ts.TS, "GET", "/v1/subjects/acct_1", nil, ts.Keys.Operator, now, "ab"),
		},
		{
			name: "unknown key",
			req: func() *http.Request {
				req := signedRequest(t, ts.TS, "GET", "/v1/subjects/acct_1", nil, ts.Keys.Operator, now)
				req.Header.Set(serve.HeaderKey, "no_such_key")
				return req
			}(),
		},
		{
			name: "malformed timestamp",
			req: func() *http.Request {
				req := signedRequest(t, ts.TS, "GET", "/v1/subjects/acct_1", nil, ts.Keys.Operator, now)
				req.Header.Set(serve.HeaderTimestamp, "not-a-timestamp")
				return req
			}(),
		},
		{
			name: "timestamp outside accepted window",
			req:  signedRequest(t, ts.TS, "GET", "/v1/subjects/acct_1", nil, ts.Keys.Operator, now.Add(-10*time.Minute)),
		},
		{
			name: "bad signature",
			req: func() *http.Request {
				req := signedRequest(t, ts.TS, "GET", "/v1/subjects/acct_1", nil, ts.Keys.Operator, now)
				req.Header.Set(serve.HeaderSignature, "deadbeef")
				return req
			}(),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := httpDo(t, c.req)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", resp.StatusCode)
			}
			e := decode(t, resp)
			if e.Error.Code != "unauthenticated" {
				t.Fatalf("code = %q, want unauthenticated", e.Error.Code)
			}
			if e.Error.Message != wantMessage {
				t.Fatalf("message = %q, want uniform %q", e.Error.Message, wantMessage)
			}
		})
	}
}
