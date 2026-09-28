package abusekit_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tokencanopy/abusekit/pkg/abusekit"
)

// flakyServer answers with a 503 the first N times, then succeeds — a
// deterministic stand-in for a transient server-side failure, independent
// of internal/serve entirely (this test is about the CLIENT's retry
// policy, not the real handler's behavior — that's covered elsewhere).
func flakyServer(t *testing.T, failTimes int32, body string) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n <= failTimes {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"code":"internal","message":"flaky"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	return ts, &calls
}

func TestClient_RetriesIdempotentSubjectCallOn503(t *testing.T) {
	body, _ := json.Marshal(map[string]any{"subject": "acct_x", "score": 0, "tier": "unknown", "signals": []any{}})
	ts, calls := flakyServer(t, 2, string(body))

	c := abusekit.New(ts.URL, "k", "s", abusekit.WithMaxRetries(3))
	subj, err := c.Subject(context.Background(), "acct_x")
	if err != nil {
		t.Fatalf("Subject: %v", err)
	}
	if subj.Subject != "acct_x" {
		t.Fatalf("unexpected subject: %+v", subj)
	}
	if got := atomic.LoadInt32(calls); got != 3 {
		t.Fatalf("expected exactly 3 calls (2 failures + 1 success), got %d", got)
	}
}

func TestClient_DoesNotRetryLabel(t *testing.T) {
	ts, calls := flakyServer(t, 5, `{"id":1}`)

	c := abusekit.New(ts.URL, "k", "s", abusekit.WithMaxRetries(3))
	_, err := c.Label(context.Background(), abusekit.LabelRequest{Subject: "acct_x", Label: "benign", Source: "operator", Actor: "a"})
	if err == nil {
		t.Fatalf("expected an error (the server never succeeds within this test's failTimes)")
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("Label must NOT be retried: expected exactly 1 call, got %d", got)
	}
}

func TestClient_DoesNotRetryEvaluate(t *testing.T) {
	ts, calls := flakyServer(t, 5, `{"subject":"acct_x","tier":"high","signals":[],"evaluated_now":true}`)

	c := abusekit.New(ts.URL, "k", "s", abusekit.WithMaxRetries(3))
	_, err := c.Evaluate(context.Background(), "acct_x", time.Second)
	if err == nil {
		t.Fatalf("expected an error (the server never succeeds within this test's failTimes)")
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("Evaluate must NOT be retried: expected exactly 1 call, got %d", got)
	}
}

func TestClient_ExhaustsRetriesAndReturnsError(t *testing.T) {
	ts, calls := flakyServer(t, 100, `{}`)

	c := abusekit.New(ts.URL, "k", "s", abusekit.WithMaxRetries(2))
	_, err := c.Subject(context.Background(), "acct_x")
	if err == nil {
		t.Fatalf("expected an error once retries are exhausted")
	}
	var apiErr *abusekit.APIError
	if !asAPIError(t, err, &apiErr) {
		t.Fatalf("expected *abusekit.APIError, got %v (%T)", err, err)
	}
	if apiErr.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", apiErr.StatusCode)
	}
	if got := atomic.LoadInt32(calls); got != 3 {
		t.Fatalf("expected exactly 3 calls (1 + 2 retries), got %d", got)
	}
}

func asAPIError(t *testing.T, err error, target **abusekit.APIError) bool {
	t.Helper()
	ae, ok := err.(*abusekit.APIError)
	if ok {
		*target = ae
	}
	return ok
}
