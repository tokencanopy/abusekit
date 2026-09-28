package serve_test

import (
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

func TestListSubjects_PaginatesStably(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)
	for i := 0; i < 5; i++ {
		subj := "acct_list_" + string(rune('a'+i))
		postEvents(t, ts, now, eventJSON("evt-list-"+string(rune('a'+i)), subj, "subject.created", now.Format(time.RFC3339), nil))
	}

	seen := map[string]bool{}
	cursor := ""
	for {
		path := "/v1/subjects?limit=2"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		req := signedRequest(t, ts.TS, "GET", path, nil, ts.Keys.Operator, now)
		resp := httpDo(t, req)
		var out struct {
			Subjects []struct {
				Subject string `json:"subject"`
			} `json:"subjects"`
			NextCursor *string `json:"next_cursor"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			resp.Body.Close()
			t.Fatalf("decode: %v", err)
		}
		resp.Body.Close()
		for _, it := range out.Subjects {
			if seen[it.Subject] {
				t.Fatalf("subject %s seen twice across pages", it.Subject)
			}
			seen[it.Subject] = true
		}
		if out.NextCursor == nil {
			break
		}
		cursor = *out.NextCursor
	}
	if len(seen) != 5 {
		t.Fatalf("walked %d subjects, want 5: %v", len(seen), seen)
	}
}

func TestListSubjects_TierFilterValidation(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)

	req := signedRequest(t, ts.TS, "GET", "/v1/subjects?tier=not_a_tier", nil, ts.Keys.Operator, now)
	resp := httpDo(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an invalid tier", resp.StatusCode)
	}
}
