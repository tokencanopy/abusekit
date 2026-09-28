package serve_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"
)

func postEvents(t *testing.T, ts *testServer, at time.Time, events ...map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"events": events})
	resp := httpDo(t, signedRequest(t, ts.TS, "POST", "/v1/events", body, ts.Keys.Producer, at))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("postEvents: status = %d, body: %s", resp.StatusCode, b)
	}
}

func TestGetSubject_NeverSeenIs404(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)

	req := signedRequest(t, ts.TS, "GET", "/v1/subjects/never_seen", nil, ts.Keys.Operator, now)
	resp := httpDo(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestGetSubject_SeenButUnscoredIs200Unknown(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)
	postEvents(t, ts, now, eventJSON("evt-unscored", "acct_unscored", "subject.created", now.Format(time.RFC3339), nil))

	req := signedRequest(t, ts.TS, "GET", "/v1/subjects/acct_unscored", nil, ts.Keys.Operator, now)
	resp := httpDo(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out struct {
		Tier string `json:"tier"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if out.Tier != "unknown" {
		t.Fatalf("tier = %q, want unknown", out.Tier)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("expected Cache-Control: no-store")
	}
	if resp.Header.Get("ETag") == "" {
		t.Fatalf("expected an ETag header")
	}
}

func TestGetSubject_ETagAndIfNoneMatch(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)
	postEvents(t, ts, now, eventJSON("evt-etag", "acct_etag", "subject.created", now.Format(time.RFC3339), nil))

	req1 := signedRequest(t, ts.TS, "GET", "/v1/subjects/acct_etag", nil, ts.Keys.Operator, now)
	resp1 := httpDo(t, req1)
	etag := resp1.Header.Get("ETag")
	resp1.Body.Close()
	if etag == "" {
		t.Fatalf("expected an ETag")
	}

	req2 := signedRequest(t, ts.TS, "GET", "/v1/subjects/acct_etag", nil, ts.Keys.Operator, now.Add(time.Second))
	req2.Header.Set("If-None-Match", etag)
	resp2 := httpDo(t, req2)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotModified {
		t.Fatalf("status = %d, want 304 with a matching If-None-Match", resp2.StatusCode)
	}
}

// TestGetSubject_ResponseShape pins the exact score-response fields design
// §4.4 specifies, so an accidental field rename/removal is caught.
func TestGetSubject_ResponseShape(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)
	postEvents(t, ts, now, eventJSON("evt-shape", "acct_shape", "subject.created", now.Format(time.RFC3339), nil))

	req := signedRequest(t, ts.TS, "GET", "/v1/subjects/acct_shape", nil, ts.Keys.Operator, now)
	resp := httpDo(t, req)
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, field := range []string{"subject", "score", "tier", "degraded", "stale", "events_since_score", "signals"} {
		if _, ok := out[field]; !ok {
			t.Errorf("response missing field %q: %+v", field, out)
		}
	}
}

// TestEvaluate_ReachesHighAndPersists drives POST .../evaluate over real
// HTTP against eval/fixtures/fast.jsonl's setup events (S3's contract-test
// analogue of internal/worker's own
// TestEvaluateSubject_FastFixtureReachesHighBeforeFirstSend, reusing the
// SAME already-calibrated fixture rather than a hand-rolled event set that
// could silently drift from what config/local_weights.yaml was actually
// tuned against) and confirms the committed verdict is also visible via a
// subsequent GET.
func TestEvaluate_ReachesHighAndPersists(t *testing.T) {
	all := loadFixtureLines(t, "fast.jsonl")
	var setup []map[string]any
	for _, line := range all {
		if fixtureIsExternalSend(line) {
			break
		}
		setup = append(setup, line)
	}
	if len(setup) == 0 || len(setup) == len(all) {
		t.Fatalf("fast.jsonl fixture shape assumption broken: got %d setup lines of %d total", len(setup), len(all))
	}
	lastSetupAt := fixtureEventAt(t, setup[len(setup)-1])
	now := lastSetupAt.Add(time.Second)

	ts := newTestServer(t, now)
	postEvents(t, ts, now, setup...)

	req := signedRequest(t, ts.TS, "POST", "/v1/subjects/acct_example_fast_1/evaluate", nil, ts.Keys.Operator, now)
	start := time.Now()
	resp := httpDo(t, req)
	elapsed := time.Since(start)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, b)
	}
	var out struct {
		Tier         string `json:"tier"`
		EvaluatedNow bool   `json:"evaluated_now"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if out.Tier != "high" {
		t.Fatalf("tier = %q, want high", out.Tier)
	}
	if !out.EvaluatedNow {
		t.Fatalf("expected evaluated_now:true")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("evaluate took %v, want well under the 3s deadline ceiling", elapsed)
	}
	t.Logf("HTTP evaluate round-trip latency (fast.jsonl): %v", elapsed)

	getReq := signedRequest(t, ts.TS, "GET", "/v1/subjects/acct_example_fast_1", nil, ts.Keys.Operator, now)
	getResp := httpDo(t, getReq)
	defer getResp.Body.Close()
	var getOut struct {
		Tier string `json:"tier"`
	}
	json.NewDecoder(getResp.Body).Decode(&getOut)
	if getOut.Tier != "high" {
		t.Fatalf("GET after evaluate tier = %q, want high", getOut.Tier)
	}
}

func TestEvaluate_NotFoundForUnseenSubject(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)

	req := signedRequest(t, ts.TS, "POST", "/v1/subjects/never_seen/evaluate", nil, ts.Keys.Operator, now)
	resp := httpDo(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestEvaluate_RateLimitedOnRapidRepeat(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)
	postEvents(t, ts, now, eventJSON("evt-rl-1", "acct_eval_rl", "subject.created", now.Format(time.RFC3339), nil))

	req1 := signedRequest(t, ts.TS, "POST", "/v1/subjects/acct_eval_rl/evaluate", nil, ts.Keys.Operator, now)
	resp1 := httpDo(t, req1)
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("first evaluate status = %d, want 200", resp1.StatusCode)
	}

	// Immediately again (the server's own clock is frozen at `now` for the
	// whole test, so the rate limiter's fixed window never advances
	// regardless): signed a whole second later purely so its signature
	// differs from req1's and isn't rejected as a replay before ever
	// reaching the rate limiter.
	req2 := signedRequest(t, ts.TS, "POST", "/v1/subjects/acct_eval_rl/evaluate", nil, ts.Keys.Operator, now.Add(1200*time.Millisecond))
	resp2 := httpDo(t, req2)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second immediate evaluate status = %d, want 429", resp2.StatusCode)
	}
	if resp2.Header.Get("Retry-After") == "" {
		t.Fatalf("expected a Retry-After header on 429")
	}
}

func TestEvaluate_DeadlineValidation(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)
	postEvents(t, ts, now, eventJSON("evt-dl-1", "acct_eval_deadline", "subject.created", now.Format(time.RFC3339), nil))

	body := []byte(`{"deadline_ms": 5000}`)
	req := signedRequest(t, ts.TS, "POST", "/v1/subjects/acct_eval_deadline/evaluate", body, ts.Keys.Operator, now)
	req.Header.Set("Content-Type", "application/json")
	resp := httpDo(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("deadline_ms=5000 status = %d, want 400", resp.StatusCode)
	}
}

// TestEvaluate_UnknownFieldRejected is S4: evaluate's body decode rejects
// an unknown field, matching events/labels.
func TestEvaluate_UnknownFieldRejected(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)
	postEvents(t, ts, now, eventJSON("evt-uf-1", "acct_eval_unknown_field", "subject.created", now.Format(time.RFC3339), nil))

	body := []byte(`{"deadline_ms": 1000, "totally_unknown_field": true}`)
	req := signedRequest(t, ts.TS, "POST", "/v1/subjects/acct_eval_unknown_field/evaluate", body, ts.Keys.Operator, now)
	req.Header.Set("Content-Type", "application/json")
	resp := httpDo(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an unknown field", resp.StatusCode)
	}
}

// TestEvaluate_TrailingDataRejected is S4: `{"deadline_ms":1000}garbage`
// must be rejected, not silently decode the first value and ignore the
// rest.
func TestEvaluate_TrailingDataRejected(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)
	postEvents(t, ts, now, eventJSON("evt-td-1", "acct_eval_trailing", "subject.created", now.Format(time.RFC3339), nil))

	body := []byte(`{"deadline_ms": 1000}{"deadline_ms": 2000}`)
	req := signedRequest(t, ts.TS, "POST", "/v1/subjects/acct_eval_trailing/evaluate", body, ts.Keys.Operator, now)
	req.Header.Set("Content-Type", "application/json")
	resp := httpDo(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for trailing data after the JSON value", resp.StatusCode)
	}
}

// TestEvaluate_TrailingCloseBracketRejected is R6 (round 2 fix round):
// `{"deadline_ms":1000}}` — a stray closing brace tacked on after an
// otherwise-complete, otherwise-valid value — must be rejected too.
// json.Decoder.More() (the OLD check) does NOT catch this: More()'s own
// documented job is "is there another element in the array/object
// currently being parsed", and its implementation treats a bare `}` or `]`
// as "the enclosing structure just ended", not as "there is more input" —
// so this exact shape sailed through undetected before this fix.
func TestEvaluate_TrailingCloseBracketRejected(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)
	postEvents(t, ts, now, eventJSON("evt-td-2", "acct_eval_trailing_brace", "subject.created", now.Format(time.RFC3339), nil))

	body := []byte(`{"deadline_ms": 1000}}`)
	req := signedRequest(t, ts.TS, "POST", "/v1/subjects/acct_eval_trailing_brace/evaluate", body, ts.Keys.Operator, now)
	req.Header.Set("Content-Type", "application/json")
	resp := httpDo(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a trailing `}` after an otherwise-valid JSON value", resp.StatusCode)
	}
}

// TestEvaluate_WrongContentTypeWhenBodyPresent is S4: a body actually
// present with a non-JSON Content-Type is rejected, matching events/
// labels — but a request with NO body at all needs no Content-Type
// (covered by TestEvaluate_NotFoundForUnseenSubject and others, which
// never set one).
func TestEvaluate_WrongContentTypeWhenBodyPresent(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)
	postEvents(t, ts, now, eventJSON("evt-ct-1", "acct_eval_ct", "subject.created", now.Format(time.RFC3339), nil))

	body := []byte(`{"deadline_ms": 1000}`)
	req := signedRequest(t, ts.TS, "POST", "/v1/subjects/acct_eval_ct/evaluate", body, ts.Keys.Operator, now)
	req.Header.Set("Content-Type", "text/plain")
	resp := httpDo(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415", resp.StatusCode)
	}
}

// TestGetSubject_ControlCharSubjectIs400NotInternalError is S5: a subject
// path segment containing a NUL or other control byte (percent-decoded by
// net/http before PathValue sees it) must be a clean 400, never an
// unhandled Postgres driver error surfacing as a 500.
func TestGetSubject_ControlCharSubjectIs400NotInternalError(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)

	for _, encoded := range []string{"%00", "%ff", "acct%00null"} {
		req := signedRequest(t, ts.TS, "GET", "/v1/subjects/"+encoded, nil, ts.Keys.Operator, now)
		resp := httpDo(t, req)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("subject=%q: status = %d, want 400", encoded, resp.StatusCode)
		}
	}
}

// TestEvaluate_ControlCharSubjectIs400 is S5's same check for evaluate.
func TestEvaluate_ControlCharSubjectIs400(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)

	req := signedRequest(t, ts.TS, "POST", "/v1/subjects/%00/evaluate", nil, ts.Keys.Operator, now)
	resp := httpDo(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// TestEvaluate_CrossTenantIs404 is S6: a valid key from a DIFFERENT tenant
// calling evaluate on a subject that exists in tenant e2a must see 404 —
// the same non-disclosure GET already proves.
func TestEvaluate_CrossTenantIs404(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)
	postEvents(t, ts, now, eventJSON("evt-eval-crosstenant", "acct_eval_crosstenant", "subject.created", now.Format(time.RFC3339), nil))

	req := signedRequest(t, ts.TS, "POST", "/v1/subjects/acct_eval_crosstenant/evaluate", nil, ts.Keys.Other, now)
	resp := httpDo(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (cross-tenant evaluate must not disclose existence)", resp.StatusCode)
	}
}

// TestLabels_CrossTenantIs404 is S6's label-endpoint case: a different
// tenant's key (WITH labels scope, so this isn't just a 403-on-scope
// short-circuit) cannot label a subject it can't see, and gets the same
// 404 a GET would.
func TestLabels_CrossTenantIs404(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)
	postEvents(t, ts, now, eventJSON("evt-label-crosstenant", "acct_label_crosstenant", "subject.created", now.Format(time.RFC3339), nil))

	body, _ := json.Marshal(map[string]any{"subject": "acct_label_crosstenant", "label": "benign", "source": "operator", "actor": "a"})
	req := signedRequest(t, ts.TS, "POST", "/v1/labels", body, ts.Keys.OtherLabels, now)
	resp := httpDo(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 404 (cross-tenant label must not disclose existence); body: %s", resp.StatusCode, b)
	}
}

// TestGetSubject_ETagChangesWithDirtyScoredSeq is S2: the ETag must change
// once a subject becomes dirty again, even though no NEW verdict has been
// written yet — hashing only verdict ids (the pre-fix-round behavior) left
// a freshly-dirtied subject's ETag identical to its pre-dirty one.
func TestGetSubject_ETagChangesWithDirtyScoredSeq(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)
	postEvents(t, ts, now, eventJSON("evt-etag-seq-1", "acct_etag_seq", "subject.created", now.Format(time.RFC3339), nil))

	req1 := signedRequest(t, ts.TS, "GET", "/v1/subjects/acct_etag_seq", nil, ts.Keys.Operator, now)
	resp1 := httpDo(t, req1)
	etag1 := resp1.Header.Get("ETag")
	resp1.Body.Close()
	if etag1 == "" {
		t.Fatalf("expected an ETag")
	}

	// A new event bumps dirty_seq without writing any new verdict — the
	// ETag must still change.
	postEvents(t, ts, now.Add(time.Second), eventJSON("evt-etag-seq-2", "acct_etag_seq", "resource.created", now.Add(time.Second).Format(time.RFC3339), map[string]any{"kind": "agent", "name": "a"}))

	req2 := signedRequest(t, ts.TS, "GET", "/v1/subjects/acct_etag_seq", nil, ts.Keys.Operator, now.Add(2*time.Second))
	resp2 := httpDo(t, req2)
	etag2 := resp2.Header.Get("ETag")
	resp2.Body.Close()
	if etag2 == "" {
		t.Fatalf("expected an ETag")
	}
	if etag1 == etag2 {
		t.Fatalf("expected the ETag to change once the subject became dirty again, both were %s", etag1)
	}
}

// TestGetSubject_NeverScoredSubjectsDontShareAConstantETag is S2: two
// DIFFERENT never-scored subjects (both tier "unknown", zero signals) must
// NOT share the same ETag — the old verdict-ids-only hash gave every
// never-scored subject the identical empty-hash ETag, which is harmless
// for If-None-Match correctness (a client never has one subject's ETag to
// send for another) but is still a surprising/leaky constant worth
// avoiding: each subject's own dirty_seq (bumped by its own creation
// event) already makes their hashes diverge.
func TestGetSubject_NeverScoredSubjectsDontShareAConstantETag(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)
	postEvents(t, ts, now, eventJSON("evt-etag-a", "acct_etag_a", "subject.created", now.Format(time.RFC3339), nil))
	postEvents(t, ts, now, eventJSON("evt-etag-b1", "acct_etag_b", "subject.created", now.Format(time.RFC3339), nil))
	postEvents(t, ts, now, eventJSON("evt-etag-b2", "acct_etag_b", "resource.created", now.Format(time.RFC3339), map[string]any{"kind": "agent", "name": "a"}))

	getETag := func(subject string) string {
		req := signedRequest(t, ts.TS, "GET", "/v1/subjects/"+subject, nil, ts.Keys.Operator, now)
		resp := httpDo(t, req)
		defer resp.Body.Close()
		return resp.Header.Get("ETag")
	}
	etagA := getETag("acct_etag_a")
	etagB := getETag("acct_etag_b")
	if etagA == "" || etagB == "" {
		t.Fatalf("expected non-empty ETags: a=%q b=%q", etagA, etagB)
	}
	if etagA == etagB {
		t.Fatalf("expected two never-scored subjects with different dirty_seq to have different ETags, both were %s", etagA)
	}
}

// TestEvaluate_SyntheticSubjectReturnsStoredView is S1's HTTP-level proof:
// e2a's own prober accounts are synthetic — evaluate on one must be a
// normal 200 with the stored view (tier "unknown", evaluated_now:false),
// never a 500.
func TestEvaluate_SyntheticSubjectReturnsStoredView(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)
	postEvents(t, ts, now,
		eventJSON("evt-synth-1", "mon-a", "subject.created", now.Format(time.RFC3339), nil),
		eventJSON("evt-synth-2", "mon-a", "subject.class", now.Add(time.Second).Format(time.RFC3339), map[string]any{"class": "synthetic"}),
	)

	req := signedRequest(t, ts.TS, "POST", "/v1/subjects/mon-a/evaluate", nil, ts.Keys.Operator, now.Add(2*time.Second))
	resp := httpDo(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, b)
	}
	var out struct {
		Tier         string `json:"tier"`
		EvaluatedNow bool   `json:"evaluated_now"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if out.Tier != "unknown" {
		t.Fatalf("tier = %q, want unknown", out.Tier)
	}
	if out.EvaluatedNow {
		t.Fatalf("expected evaluated_now=false for a synthetic (never-scored) subject")
	}
}

// TestEvaluate_BusySubjectReturns409WithRetryAfter is S1: a subject the
// worker's own Tick has already claimed must 409 subject_busy (not 429
// rate_limited) with a real Retry-After, when evaluate races it.
func TestEvaluate_BusySubjectReturns409WithRetryAfter(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)
	postEvents(t, ts, now, eventJSON("evt-busy-1", "acct_eval_busy", "subject.created", now.Format(time.RFC3339), nil))

	if _, err := ts.Store.ClaimDirtySubjects(context.Background(), now, 0); err != nil {
		t.Fatalf("ClaimDirtySubjects: %v", err)
	}

	req := signedRequest(t, ts.TS, "POST", "/v1/subjects/acct_eval_busy/evaluate", nil, ts.Keys.Operator, now)
	resp := httpDo(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 409; body: %s", resp.StatusCode, b)
	}
	ra := resp.Header.Get("Retry-After")
	if ra == "" || ra == "0" {
		t.Fatalf("expected a non-zero Retry-After, got %q", ra)
	}
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	json.NewDecoder(resp.Body).Decode(&envelope)
	if envelope.Error.Code != "subject_busy" {
		t.Fatalf("error code = %q, want subject_busy", envelope.Error.Code)
	}
}
