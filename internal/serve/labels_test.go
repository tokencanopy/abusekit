package serve_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"
)

func TestLabels_HappyPath(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)
	postEvents(t, ts, now, eventJSON("evt-label-1", "acct_label", "subject.created", now.Format(time.RFC3339), nil))

	body, _ := json.Marshal(map[string]any{
		"subject": "acct_label", "label": "abusive", "source": "operator", "actor": "ops@example.test",
	})
	req := signedRequest(t, ts.TS, "POST", "/v1/labels", body, ts.Keys.Operator, now)
	resp := httpDo(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 201; body: %s", resp.StatusCode, b)
	}
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	if _, ok := out["id"]; !ok {
		t.Fatalf("expected an \"id\" field: %+v", out)
	}
	if _, ok := out["corpus_example_id"]; !ok {
		t.Fatalf("expected a \"corpus_example_id\" field (design §4.9's corpus snapshot): %+v", out)
	}
}

func TestLabels_PerRuleVocabularyEnforced(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)
	postEvents(t, ts, now, eventJSON("evt-label-2", "acct_label_rule", "subject.created", now.Format(time.RFC3339), nil))

	// config/rules.yaml's shipped rule accepts [benign, suspicious, abusive].
	valid, _ := json.Marshal(map[string]any{
		"subject": "acct_label_rule", "rule": "new_account_velocity", "label": "suspicious", "source": "operator", "actor": "a",
	})
	resp1 := httpDo(t, signedRequest(t, ts.TS, "POST", "/v1/labels", valid, ts.Keys.Operator, now))
	defer resp1.Body.Close()
	if resp1.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp1.Body)
		t.Fatalf("valid rule label status = %d; body: %s", resp1.StatusCode, b)
	}

	invalid, _ := json.Marshal(map[string]any{
		"subject": "acct_label_rule", "rule": "new_account_velocity", "label": "not_a_real_label", "source": "operator", "actor": "a",
	})
	resp2 := httpDo(t, signedRequest(t, ts.TS, "POST", "/v1/labels", invalid, ts.Keys.Operator, now.Add(time.Second)))
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid label status = %d, want 400", resp2.StatusCode)
	}
}

func TestLabels_UnknownRuleRejected(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)
	postEvents(t, ts, now, eventJSON("evt-label-3", "acct_label_unknown_rule", "subject.created", now.Format(time.RFC3339), nil))

	body, _ := json.Marshal(map[string]any{
		"subject": "acct_label_unknown_rule", "rule": "no_such_rule", "label": "abusive", "source": "operator", "actor": "a",
	})
	req := signedRequest(t, ts.TS, "POST", "/v1/labels", body, ts.Keys.Operator, now)
	resp := httpDo(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an unknown rule", resp.StatusCode)
	}
}

func TestLabels_SubjectLevelVocabularyEnforced(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)
	postEvents(t, ts, now, eventJSON("evt-label-4", "acct_label_subjectlevel", "subject.created", now.Format(time.RFC3339), nil))

	body, _ := json.Marshal(map[string]any{
		"subject": "acct_label_subjectlevel", "label": "suspicious", "source": "operator", "actor": "a",
	})
	req := signedRequest(t, ts.TS, "POST", "/v1/labels", body, ts.Keys.Operator, now)
	resp := httpDo(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (subject-level label must be benign|abusive)", resp.StatusCode)
	}
}

func TestLabels_ScopeDenial(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)

	body, _ := json.Marshal(map[string]any{
		"subject": "acct_x", "label": "benign", "source": "operator", "actor": "a",
	})
	// The producer key has only the `events` scope.
	req := signedRequest(t, ts.TS, "POST", "/v1/labels", body, ts.Keys.Producer, now)
	resp := httpDo(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestLabels_SourceValidation(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)
	postEvents(t, ts, now, eventJSON("evt-label-5", "acct_label_source", "subject.created", now.Format(time.RFC3339), nil))

	body, _ := json.Marshal(map[string]any{
		"subject": "acct_label_source", "label": "benign", "source": "not_a_real_source", "actor": "a",
	})
	req := signedRequest(t, ts.TS, "POST", "/v1/labels", body, ts.Keys.Operator, now)
	resp := httpDo(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an invalid source", resp.StatusCode)
	}
}

func TestLabels_NotFoundForUnseenSubject(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)

	body, _ := json.Marshal(map[string]any{
		"subject": "never_seen_label_subject", "label": "benign", "source": "operator", "actor": "a",
	})
	req := signedRequest(t, ts.TS, "POST", "/v1/labels", body, ts.Keys.Operator, now)
	resp := httpDo(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestLabels_AbusiveMarksNeighborDirty(t *testing.T) {
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := newTestServer(t, now)

	sharedEmail := "e1e2e3e4e5e6e7e8e9e0e1e2e3e4e5e6e7e8e9e0e1e2e3e4e5e6e7e8e9e0e1e2"
	postEvents(t, ts, now,
		map[string]any{"id": "evt-n-1", "subject": "acct_neighbor_a", "type": "subject.created", "at": now.Format(time.RFC3339), "links": map[string]any{"email_hash": sharedEmail}},
		map[string]any{"id": "evt-n-2", "subject": "acct_neighbor_b", "type": "subject.created", "at": now.Format(time.RFC3339), "links": map[string]any{"email_hash": sharedEmail}},
	)

	body, _ := json.Marshal(map[string]any{
		"subject": "acct_neighbor_a", "label": "abusive", "source": "operator", "actor": "a",
	})
	resp := httpDo(t, signedRequest(t, ts.TS, "POST", "/v1/labels", body, ts.Keys.Operator, now))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("label status = %d, want 201", resp.StatusCode)
	}

	// acct_neighbor_b shares the email hash; its dirty_seq must have been
	// bumped by the propagation this endpoint triggers via PutLabel.
	dirty, err := ts.Store.ClaimDirtySubjects(context.Background(), now, 0)
	if err != nil {
		t.Fatalf("ClaimDirtySubjects: %v", err)
	}
	found := false
	for _, d := range dirty {
		if d.Subject == "acct_neighbor_b" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected acct_neighbor_b to be dirty after labelling its neighbour abusive, claimed: %+v", dirty)
	}
}
