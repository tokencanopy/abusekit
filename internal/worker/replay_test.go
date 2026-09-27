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

// lastEventAt returns the latest At among events.
func lastEventAt(events []event.Event) time.Time {
	last := events[0].At
	for _, e := range events {
		if e.At.After(last) {
			last = e.At
		}
	}
	return last
}

// runReplayAt ingests events into a fresh throwaway store and ticks the
// worker once with Now fixed at now, returning the resulting SubjectView
// for subject plus the Tick's own result (so a caller can assert exactly
// one subject was scored with no errors, or inspect it further).
func runReplayAt(t *testing.T, events []event.Event, subject string, now time.Time) (*store.SubjectView, TickResult) {
	t.Helper()
	s := newTestStore(t)
	ctx := context.Background()
	cfg := loadShippedConfig(t)

	ingestFixture(t, ctx, s, events)

	w, err := New(Deps{Store: s, Config: cfg, Neighbors: feature.NoNeighbors, Brands: loadShippedBrands(t), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result, err := w.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}

	view, err := s.SubjectView(ctx, testTenant, subject, nil)
	if err != nil {
		t.Fatalf("SubjectView: %v", err)
	}
	return view, result
}

// runReplay ingests the fixture at path, ticks the worker once with Now
// fixed at the last fixture event's timestamp plus a short buffer
// (simulating the worker picking the dirty subject up shortly after
// ingest — design §1 criterion 2's own "within 15s of the last ... event"
// framing, generalized here to "shortly after"), and returns the
// resulting SubjectView for subject.
func runReplay(t *testing.T, fixtureFile, subject string) *store.SubjectView {
	t.Helper()
	events := loadFixture(t, filepath.Join(repoRoot(t), "eval", "fixtures", fixtureFile))
	now := lastEventAt(events).Add(time.Minute)
	view, result := runReplayAt(t, events, subject, now)
	if result.Scored != 1 || len(result.Errors) != 0 {
		t.Fatalf("Tick result = %+v, want exactly one subject scored with no errors", result)
	}
	return view
}

// assertBand fails the test unless got is within [min, max] — B1 fix
// round: "assert score bands not just tiers", so a fixture's score can't
// silently drift to the opposite edge of a tier and still pass.
func assertBand(t *testing.T, name string, got, min, max float64) {
	t.Helper()
	if got < min || got > max {
		t.Errorf("%s: score = %v, want in [%v, %v]", name, got, min, max)
	}
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
	assertBand(t, "reference_operator", view.Score, 0.99, 1.0)
}

// TestReplay_BenignTransactionalStaysLow replays eval/fixtures/
// benign_transactional.jsonl — an ordinary customer account with a slow
// (but real — B5 fix round: `upgraded` must not fire risk on its own)
// upgrade, no self-send rehearsal, no brand-like names, and sends outside
// the first day — and asserts it stays at tier "low".
func TestReplay_BenignTransactionalStaysLow(t *testing.T) {
	view := runReplay(t, "benign_transactional.jsonl", "acct_example_benign_1")
	if view.Tier != "low" {
		t.Errorf("benign_transactional: tier = %q (score %v), want low\nsignals: %+v", view.Tier, view.Score, view.Signals)
	}
	assertBand(t, "benign_transactional", view.Score, 0.0, 0.05)
}

// TestReplay_BurstReachesHighBeforeFirstSend is design §1 success
// criterion 2(a) / plan.md's S2 row: replays ONLY eval/fixtures/burst.jsonl's
// setup events (signup through the last key/agent creation — everything
// before its first content.sent) and asserts tier "high" at that last
// setup event + 15s, proving the local rule reaches high from onboarding
// signals alone, strictly before the fixture's first send.
func TestReplay_BurstReachesHighBeforeFirstSend(t *testing.T) {
	all := loadFixture(t, filepath.Join(repoRoot(t), "eval", "fixtures", "burst.jsonl"))
	var setup []event.Event
	for _, e := range all {
		if e.Type == "content.sent" {
			break
		}
		setup = append(setup, e)
	}
	if len(setup) == 0 || len(setup) == len(all) {
		t.Fatalf("burst.jsonl fixture shape assumption broken: got %d setup events of %d total", len(setup), len(all))
	}
	lastSetupAt := lastEventAt(setup)
	firstSendAt := lastEventAt(all) // not exactly right in general, but see the explicit check below
	for _, e := range all {
		if e.Type == "content.sent" {
			firstSendAt = e.At
			break
		}
	}
	now := lastSetupAt.Add(15 * time.Second)
	if !now.Before(firstSendAt) {
		t.Fatalf("fixture timing assumption broken: last-setup-event+15s (%v) is not before the first send (%v)", now, firstSendAt)
	}

	view, result := runReplayAt(t, setup, "acct_example_burst_1", now)
	if result.Scored != 1 || len(result.Errors) != 0 {
		t.Fatalf("Tick result = %+v, want exactly one subject scored with no errors", result)
	}
	if view.Tier != "high" {
		t.Errorf("burst (before first send): tier = %q (score %v), want high\nsignals: %+v", view.Tier, view.Score, view.Signals)
	}
	assertBand(t, "burst (before first send)", view.Score, 0.9, 1.0)
}

// TestReplay_BurstFinalTierHigh replays the FULL burst.jsonl fixture
// (setup plus the self-send-rehearsal-then-external-blast) and asserts the
// final tier is still "high".
func TestReplay_BurstFinalTierHigh(t *testing.T) {
	view := runReplay(t, "burst.jsonl", "acct_example_burst_1")
	if view.Tier != "high" {
		t.Errorf("burst (final): tier = %q (score %v), want high\nsignals: %+v", view.Tier, view.Score, view.Signals)
	}
	assertBand(t, "burst (final)", view.Score, 0.9, 1.0)
}

// TestReplay_BenignFastOnboardingStaysBelowHigh replays eval/fixtures/
// benign_fast_onboarding.jsonl (B1 fix round, proven: this exact shape —
// 3 agents + 1 key in 10 minutes, one self-send, one external send, no
// payment at all — previously scored 0.925, tier high) and asserts it now
// stays below high.
func TestReplay_BenignFastOnboardingStaysBelowHigh(t *testing.T) {
	view := runReplay(t, "benign_fast_onboarding.jsonl", "acct_example_fast_onboarding_1")
	if view.Tier == "high" {
		t.Errorf("benign_fast_onboarding: tier = high (score %v), want low or medium\nsignals: %+v", view.Score, view.Signals)
	}
	assertBand(t, "benign_fast_onboarding", view.Score, 0.15, 0.4)
}

// TestReplay_BenignIntegrationHeavyStaysBelowHigh replays eval/fixtures/
// benign_integration_heavy.jsonl (B1 fix round, proven: 5 agents + 1 key
// named after real SaaS integrations in the first hour, no payment,
// previously scored 0.912, tier high) and asserts it now stays below high
// — in particular that none of "Stripe Webhook Relay"/"Google Calendar
// Sync"/"Microsoft Teams Relay" trip name_brand_match (S3 fix round: those
// generic single-word brands are excluded from config/brands.yaml).
func TestReplay_BenignIntegrationHeavyStaysBelowHigh(t *testing.T) {
	view := runReplay(t, "benign_integration_heavy.jsonl", "acct_example_integration_heavy_1")
	if view.Tier == "high" {
		t.Errorf("benign_integration_heavy: tier = high (score %v), want low or medium\nsignals: %+v", view.Score, view.Signals)
	}
	assertBand(t, "benign_integration_heavy", view.Score, 0.05, 0.35)
}

// TestReplay_DormantThenBlastReachesHigh replays eval/fixtures/
// dormant_then_blast.jsonl — a week-old account, no upgrade ever, that
// suddenly creates 10 resources (one brand-impersonating) and sends to 20
// distinct external domains within an hour — and asserts it reaches
// "high" (B1 fix round, proven: this exact shape previously scored 0.000,
// tier low, since every "just signed up" feature this account doesn't
// have — payment/upgrade signals — carried most of S1's placeholder
// weights). Note (fixture-sizing interpretation): the review's own
// description named "300 external domains"; this fixture uses 20, since
// no v0 feature (first_day_distinct_domains doesn't apply — these sends
// land 7 days after signup, well past its first-day window;
// burst_ratio_24h_vs_lifetime only cares that recent activity dominates
// lifetime activity, not the exact count) actually distinguishes 20 from
// 300 distinct post-first-day domains.
func TestReplay_DormantThenBlastReachesHigh(t *testing.T) {
	view := runReplay(t, "dormant_then_blast.jsonl", "acct_example_dormant_blast_1")
	if view.Tier != "high" {
		t.Errorf("dormant_then_blast: tier = %q (score %v), want high\nsignals: %+v", view.Tier, view.Score, view.Signals)
	}
	assertBand(t, "dormant_then_blast", view.Score, 0.8, 0.98)
}
