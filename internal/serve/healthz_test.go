package serve_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestHealthz_Unauthenticated(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)

	resp, err := http.Get(ts.TS.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out map[string]string
	json.NewDecoder(resp.Body).Decode(&out)
	if out["status"] != "ok" {
		t.Fatalf("body = %+v, want status:ok", out)
	}
}

func TestUnknownRoute_404(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)

	resp, err := http.Get(ts.TS.URL + "/v1/no-such-route")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}
