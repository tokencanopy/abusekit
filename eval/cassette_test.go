package eval

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/tokencanopy/abusekit/internal/model"
)

// stubScorer is a minimal model.Scorer for cassette tests — eval already
// has enough of its own scoring logic under test elsewhere; this type
// only needs to prove CassetteScorer's own record/replay/miss behavior.
type stubScorer struct {
	name  string
	calls int
}

func (s *stubScorer) Name() string { return s.name }
func (s *stubScorer) Capabilities() model.Capabilities {
	return model.Capabilities{LabelMode: model.OpenLabelMode(), AcceptsFeatures: true, Calibrated: true}
}
func (s *stubScorer) Policy() model.DataPolicy { return model.DataPolicy{} }
func (s *stubScorer) Version() string          { return "stub-v1" }
func (s *stubScorer) Score(_ context.Context, req model.ScoreRequest) (model.ScoreResult, error) {
	s.calls++
	return model.ScoreResult{Probs: map[string]float64{"benign": 0.9, "abusive": 0.1}, Model: s.name, Checkpoint: "ck1"}, nil
}

// TestCassetteScorer_ReplayMissIsLoud proves a cassette miss in
// CassetteReplay mode (the mode CI/`make gate` always uses) is a loud,
// wrapped error — never a silent fall-through to the live scorer (task
// brief: "a cassette miss must be a loud failure in CI mode").
func TestCassetteScorer_ReplayMissIsLoud(t *testing.T) {
	inner := &stubScorer{name: "vendor"}
	cassette, err := LoadCassette(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err != nil {
		t.Fatalf("LoadCassette: %v", err)
	}
	cs := &CassetteScorer{Inner: inner, Cassette: cassette, Mode: CassetteReplay, PromptVersion: "v1"}

	_, err = cs.Score(context.Background(), model.ScoreRequest{Labels: []string{"benign", "abusive"}, Features: map[string]float64{"x": 1}})
	if err == nil {
		t.Fatalf("expected a cassette-miss error, got nil")
	}
	if !errors.Is(err, ErrCassetteMiss) {
		t.Errorf("error %v does not wrap ErrCassetteMiss", err)
	}
	if inner.calls != 0 {
		t.Errorf("inner.calls = %d, want 0 — CassetteReplay must never call the live scorer on a miss", inner.calls)
	}
}

// TestCassetteScorer_RecordThenReplay proves the record -> save -> load
// -> replay round trip: a CassetteRecord run calls the live scorer once
// and persists the result; a fresh CassetteReplay-mode scorer loaded
// from the saved file then answers the SAME request without calling the
// live scorer again.
func TestCassetteScorer_RecordThenReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vendor.json")
	req := model.ScoreRequest{Labels: []string{"benign", "abusive"}, Features: map[string]float64{"x": 1}, RenderVersion: "v1"}

	inner := &stubScorer{name: "vendor"}
	cassette, err := LoadCassette(path)
	if err != nil {
		t.Fatalf("LoadCassette: %v", err)
	}
	recordCS := &CassetteScorer{Inner: inner, Cassette: cassette, Mode: CassetteRecord, PromptVersion: "v1"}
	res1, err := recordCS.Score(context.Background(), req)
	if err != nil {
		t.Fatalf("record Score: %v", err)
	}
	if inner.calls != 1 {
		t.Fatalf("inner.calls = %d, want 1 after the recording call", inner.calls)
	}
	if err := cassette.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("cassette file was not written: %v", err)
	}

	// A second call with the SAME request must be served from the
	// in-memory cassette without a second live call.
	res1b, err := recordCS.Score(context.Background(), req)
	if err != nil {
		t.Fatalf("second record-mode Score: %v", err)
	}
	if inner.calls != 1 {
		t.Fatalf("inner.calls = %d after a repeat request, want still 1 (served from cassette)", inner.calls)
	}
	if res1b.Model != res1.Model || res1b.Checkpoint != res1.Checkpoint {
		t.Errorf("replayed result %+v != original %+v", res1b, res1)
	}

	// A FRESH scorer/cassette pair loaded from disk, in Replay mode, must
	// answer the same request without ever touching a live scorer.
	freshInner := &stubScorer{name: "vendor"}
	freshCassette, err := LoadCassette(path)
	if err != nil {
		t.Fatalf("LoadCassette (reload): %v", err)
	}
	replayCS := &CassetteScorer{Inner: freshInner, Cassette: freshCassette, Mode: CassetteReplay, PromptVersion: "v1"}
	res2, err := replayCS.Score(context.Background(), req)
	if err != nil {
		t.Fatalf("replay Score: %v", err)
	}
	if freshInner.calls != 0 {
		t.Fatalf("freshInner.calls = %d, want 0 — replay must never call the live scorer", freshInner.calls)
	}
	if res2.Model != res1.Model || res2.Checkpoint != res1.Checkpoint {
		t.Errorf("reloaded replay result %+v != recorded %+v", res2, res1)
	}

	// A DIFFERENT request (different features -> different input_hash)
	// is still a miss even against the same cassette file.
	otherReq := req
	otherReq.Features = map[string]float64{"x": 2}
	if _, err := replayCS.Score(context.Background(), otherReq); !errors.Is(err, ErrCassetteMiss) {
		t.Errorf("a different request should still miss, got err=%v", err)
	}
}
