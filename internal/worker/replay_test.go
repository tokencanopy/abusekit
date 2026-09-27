package worker

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tokencanopy/abusekit/internal/event"
	"github.com/tokencanopy/abusekit/internal/feature"
	"github.com/tokencanopy/abusekit/internal/store"
)

// loadFixture parses a JSONL replay fixture (eval/fixtures/*.jsonl) into
// event.Event values — each line's JSON shape matches design §4.3's wire
// event exactly (event.Event's own json tags), so no separate wire type is
// needed here.
func loadFixture(t *testing.T, path string) []event.Event {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open fixture %s: %v", path, err)
	}
	defer f.Close()

	var events []event.Event
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var e event.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("parse fixture line %q: %v", line, err)
		}
		events = append(events, e)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan fixture %s: %v", path, err)
	}
	if len(events) == 0 {
		t.Fatalf("fixture %s parsed to zero events", path)
	}
	return events
}

// ingestFixture runs events through the real ingest path (Validate then
// Redact, exactly what a producer's POST /v1/events would do) and appends
// them via a single AppendEvents call, matching how the events arrive
// together as one batch in production.
//
// Each event is Validated against ITS OWN `at` as the reference "now" —
// legitimate for a replay of already-known-good historical/fixture data
// (as opposed to a live event whose skew against the REAL wall clock is
// exactly what Validate's check exists to catch), and it sidesteps a
// synthetic fixture's fictional year (2031, this repo's public-data-
// boundary convention for fixture timestamps) being nowhere near
// Validate's ±24h window around any real wall-clock "now".
func ingestFixture(t *testing.T, ctx context.Context, s *store.Store, events []event.Event) {
	t.Helper()
	for i := range events {
		e := &events[i]
		if err := e.Validate(event.ValidateOptions{Now: e.At}); err != nil {
			t.Fatalf("fixture event %s failed Validate: %v", e.ID, err)
		}
		if err := e.Redact(); err != nil {
			t.Fatalf("fixture event %s failed Redact: %v", e.ID, err)
		}
	}
	if _, err := s.AppendEvents(ctx, testTenant, "abusekit-replay-test", events); err != nil {
		t.Fatalf("append fixture events: %v", err)
	}
}

// runReplay ingests the fixture at path, ticks the worker once with Now
// fixed at the last fixture event's timestamp plus a short buffer
// (simulating the worker picking the dirty subject up shortly after
// ingest — design §1 criterion 2's own "within 15s of the last ... event"
// framing, generalized here to "shortly after"), and returns the
// resulting SubjectView for subject.
func runReplay(t *testing.T, fixtureFile, subject string) *store.SubjectView {
	t.Helper()
	s := newTestStore(t)
	ctx := context.Background()
	cfg := loadShippedConfig(t)

	events := loadFixture(t, filepath.Join(repoRoot(t), "eval", "fixtures", fixtureFile))
	ingestFixture(t, ctx, s, events)

	lastAt := events[0].At
	for _, e := range events {
		if e.At.After(lastAt) {
			lastAt = e.At
		}
	}
	now := lastAt.Add(time.Minute)

	w, err := New(Deps{Store: s, Config: cfg, Neighbors: feature.NoNeighbors, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result, err := w.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if result.Scored != 1 || len(result.Errors) != 0 {
		t.Fatalf("Tick result = %+v, want exactly one subject scored with no errors", result)
	}

	view, err := s.SubjectView(ctx, testTenant, subject, nil)
	if err != nil {
		t.Fatalf("SubjectView: %v", err)
	}
	return view
}

// TestReplay_ReferenceOperatorReachesHigh replays eval/fixtures/
// reference_operator.jsonl — a synthetic composite of design §1's
// campaign narrative (fraud-declined attempts before a prepaid success, a
// quick upgrade, a burst of agent/key creation, self-send rehearsal before
// an external fan-out blast) — through the real worker+core pipeline with
// the shipped local scorer and config, and asserts it reaches tier "high"
// (design §1 success criterion 2 / plan.md's S2 row).
func TestReplay_ReferenceOperatorReachesHigh(t *testing.T) {
	view := runReplay(t, "reference_operator.jsonl", "acct_example_operator_1")
	if view.Tier != "high" {
		t.Errorf("reference_operator: tier = %q (score %v), want high\nsignals: %+v", view.Tier, view.Score, view.Signals)
	}
	if view.Degraded {
		t.Errorf("reference_operator: degraded = true, want false (the local rule should always answer)")
	}
}

// TestReplay_BenignTransactionalStaysLow replays eval/fixtures/
// benign_transactional.jsonl — an ordinary customer account with a slow
// upgrade, no self-send rehearsal, no brand-like names, and sends outside
// the first day — and asserts it stays at tier "low".
func TestReplay_BenignTransactionalStaysLow(t *testing.T) {
	view := runReplay(t, "benign_transactional.jsonl", "acct_example_benign_1")
	if view.Tier != "low" {
		t.Errorf("benign_transactional: tier = %q (score %v), want low\nsignals: %+v", view.Tier, view.Score, view.Signals)
	}
}
