package serve_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestErase_PurgesNonAbusiveSubject(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)
	postEvents(t, ts, now, eventJSON("evt-erase-1", "acct_erase_purge", "subject.created", now.Format(time.RFC3339), nil))

	req := signedRequest(t, ts.TS, "DELETE", "/v1/subjects/acct_erase_purge", nil, ts.Keys.Operator, now)
	resp := httpDo(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	if out["mode"] != "purged" {
		t.Fatalf("mode = %v, want purged", out["mode"])
	}

	getReq := signedRequest(t, ts.TS, "GET", "/v1/subjects/acct_erase_purge", nil, ts.Keys.Operator, now.Add(time.Second))
	getResp := httpDo(t, getReq)
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET after purge status = %d, want 404", getResp.StatusCode)
	}
}

func TestErase_TombstonesAbusiveLabelledSubject(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)
	postEvents(t, ts, now, eventJSON("evt-erase-2", "acct_erase_tombstone", "subject.created", now.Format(time.RFC3339), nil))

	labelBody, _ := json.Marshal(map[string]any{"subject": "acct_erase_tombstone", "label": "abusive", "source": "operator", "actor": "a"})
	labelResp := httpDo(t, signedRequest(t, ts.TS, "POST", "/v1/labels", labelBody, ts.Keys.Operator, now))
	labelResp.Body.Close()
	if labelResp.StatusCode != http.StatusCreated {
		t.Fatalf("label setup status = %d", labelResp.StatusCode)
	}

	req := signedRequest(t, ts.TS, "DELETE", "/v1/subjects/acct_erase_tombstone", nil, ts.Keys.Operator, now.Add(time.Second))
	resp := httpDo(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	if out["mode"] != "tombstoned" {
		t.Fatalf("mode = %v, want tombstoned", out["mode"])
	}

	// The subject still exists (numeric evidence retained) — GET is a 200,
	// not a 404, unlike the purge path.
	getReq := signedRequest(t, ts.TS, "GET", "/v1/subjects/acct_erase_tombstone", nil, ts.Keys.Operator, now.Add(2*time.Second))
	getResp := httpDo(t, getReq)
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET after tombstone status = %d, want 200", getResp.StatusCode)
	}

	// A repeat DELETE is idempotent.
	req2 := signedRequest(t, ts.TS, "DELETE", "/v1/subjects/acct_erase_tombstone", nil, ts.Keys.Operator, now.Add(3*time.Second))
	resp2 := httpDo(t, req2)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("repeat DELETE status = %d, want 200", resp2.StatusCode)
	}
	var out2 map[string]any
	json.NewDecoder(resp2.Body).Decode(&out2)
	if out2["already_erased"] != true {
		t.Fatalf("expected already_erased:true on repeat, got %+v", out2)
	}
}

func TestErase_NotFoundForUnseenSubject(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)

	req := signedRequest(t, ts.TS, "DELETE", "/v1/subjects/never_seen_erase", nil, ts.Keys.Operator, now)
	resp := httpDo(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestErase_ScopeDenial(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)
	postEvents(t, ts, now, eventJSON("evt-erase-3", "acct_erase_scope", "subject.created", now.Format(time.RFC3339), nil))

	// ts.Keys.Other has `read` only, not `erase`.
	req := signedRequest(t, ts.TS, "DELETE", "/v1/subjects/acct_erase_scope", nil, ts.Keys.Other, now)
	resp := httpDo(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}
