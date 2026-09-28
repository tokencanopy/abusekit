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

	w, err := New(Deps{Store: s, Config: cfg, Neighbors: feature.NoNeighbors, Brands: loadTestBrands(t), Webmail: loadShippedWebmail(t), Now: func() time.Time { return now }})
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
// stays below high. Upper edge widened to 0.45 (D2 round 3): switching
// first_day_distinct_domains from a hard cap to a log1p curve anchored at
// n=10 (see internal/feature.firstDayDistinctDomainsLogScale) makes a
// SMALL domain count (this fixture's) contribute MORE than the old
// literal-count formula did — log1p is concave, so it sits above the
// straight line from (0,0) to (10,10) everywhere in between — which
// nudged this fixture from ~0.26 to ~0.40, still comfortably medium/low,
// but too close to the old 0.4 edge to leave a safe margin.
func TestReplay_BenignFastOnboardingStaysBelowHigh(t *testing.T) {
	view := runReplay(t, "benign_fast_onboarding.jsonl", "acct_example_fast_onboarding_1")
	if view.Tier == "high" {
		t.Errorf("benign_fast_onboarding: tier = high (score %v), want low or medium\nsignals: %+v", view.Score, view.Signals)
	}
	assertBand(t, "benign_fast_onboarding", view.Score, 0.15, 0.45)
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
// suddenly sends to 100 distinct external (non-webmail) domains within
// ten minutes — and asserts it reaches "high" (B1 fix round, proven: this
// exact shape previously scored 0.000, tier low, since every "just signed
// up" feature this account doesn't have — payment/upgrade signals —
// carried most of S1's placeholder weights).
//
// Round 2's R1 fix round: this fixture no longer has a brand-impersonating
// agent name or a resource-creation burst at all (a SINGLE neutral agent)
// — the review asked for proof that the volume/recipient signals ALONE,
// with no other evidence, still carry this shape to `high`, replacing the
// old hard 7-day age gate (proven evadable by simply waiting past it, and
// blind to whether the account had any real prior sending at all) with a
// history-relative burst_factor: this subject has NO prior sending
// history, so its 100-recipient burst reads at close to full strength
// (see internal/feature.burstFactor/priorTenMinutePeak), discounted only
// by ageDecayFactor for its 7-day age (still close to full weight; the
// floor doesn't bind until ~day 25).
func TestReplay_DormantThenBlastReachesHigh(t *testing.T) {
	view := runReplay(t, "dormant_then_blast.jsonl", "acct_example_dormant_blast_1")
	if view.Tier != "high" {
		t.Errorf("dormant_then_blast: tier = %q (score %v), want high\nsignals: %+v", view.Tier, view.Score, view.Signals)
	}
	assertBand(t, "dormant_then_blast", view.Score, 0.8, 0.98)
}

// TestReplay_SelfSendOnlyStaysBelowHigh replays eval/fixtures/
// benign_self_send_only.jsonl — R1 round 2, proven: a developer sending 8
// test emails to their own inbox, never externally, previously scored
// 0.9433 (tier high) on selfSendBeforeExternal alone; capping it at
// selfSendBeforeExternalCap brings it down to ~0.12.
func TestReplay_SelfSendOnlyStaysBelowHigh(t *testing.T) {
	view := runReplay(t, "benign_self_send_only.jsonl", "acct_example_selfsend_only_1")
	if view.Tier == "high" {
		t.Errorf("benign_self_send_only: tier = high (score %v), want low or medium\nsignals: %+v", view.Score, view.Signals)
	}
	assertBand(t, "benign_self_send_only", view.Score, 0.05, 0.35)
}

// TestReplay_ReceiptsFanoutStaysBelowHigh replays eval/fixtures/
// benign_receipts_fanout.jsonl — R1 round 2, proven: a day-1 receipts
// account (1 agent) fanning out to 30 distinct, genuinely external
// customer domains previously scored 0.9634 (tier high) on
// first_day_distinct_domains alone; R1's hard cap brought it down to
// ~0.15, and D2 round 3's log1p replacement (still anchored so n=10 gives
// the same contribution the old cap did) leaves it at ~0.34.
//
// Round 2's R1 fix round widened the upper edge of this band from 0.4 to
// 0.55: this fixture, like dormant_then_blast, has NO prior sending
// history, so sends_10m_max/sends_1h/distinct_recipients_1h's new
// burst_factor reads its 30-recipient, 1-hour fan-out at close to full
// strength too — the SAME history-relative measure that (correctly)
// carries dormant_then_blast to `high` on a much larger, more
// concentrated burst also pushes this smaller, more spread-out one from
// `low` into low `medium`. Documented trade-off, not a fixture
// regression: this fixture's own defining shape (30 recipients spread
// across a full hour, one legitimate account) keeps its sends_10m_max far
// below dormant_then_blast's (a burst concentrated into 10 minutes), so
// it still lands clearly short of `high` — the qualitative claim this
// test exists to protect.
func TestReplay_ReceiptsFanoutStaysBelowHigh(t *testing.T) {
	view := runReplay(t, "benign_receipts_fanout.jsonl", "acct_example_receipts_fanout_1")
	if view.Tier == "high" {
		t.Errorf("benign_receipts_fanout: tier = high (score %v), want low or medium\nsignals: %+v", view.Score, view.Signals)
	}
	assertBand(t, "benign_receipts_fanout", view.Score, 0.4, 0.55)
}

// TestReplay_VariantAStaysBelowHigh replays eval/fixtures/
// benign_variant_a.jsonl — R1 round 2's own regression guard, using the
// re-review's literal numbers (4 agents + 2 keys in 10 minutes, then 5
// external emails to 5 distinct domains within hour 1): the re-review
// measured this shape at 0.49 (tier medium) against the ALREADY-RETUNED
// weights from the first fix round and flagged it as a case any further
// retuning must not push into high. Upper edge widened to 0.70 (D2 round
// 3): its 5 distinct domains are well under the old hard cap of 10, but
// log1p's concave shape still gives them more weight than the old
// literal-count formula did (see TestReplay_BenignFastOnboardingStaysBelowHigh's
// comment for why), nudging this fixture from 0.49 to ~0.64 — still
// clearly medium, not high, but too close to the old 0.65 edge for a safe
// margin. The regression this guards against is unchanged: a WEIGHT
// change elsewhere (e.g. resource/key velocity) creeping it upward.
func TestReplay_VariantAStaysBelowHigh(t *testing.T) {
	view := runReplay(t, "benign_variant_a.jsonl", "acct_example_variant_a_1")
	if view.Tier == "high" {
		t.Errorf("benign_variant_a: tier = high (score %v), want medium (this is a regression guard, not a call to make it score LOW)\nsignals: %+v", view.Score, view.Signals)
	}
	assertBand(t, "benign_variant_a", view.Score, 0.35, 0.70)
}

// TestReplay_SelfSendBrandNameStaysBelowHigh replays eval/fixtures/
// benign_selfsend_brandname.jsonl — R1 round 2, proven: an agent literally
// named "PayPal integration" that only ever sends to its own domain
// (recipient_is_own_identity=true, 10 sends, no external send ever)
// previously scored 0.9880 (tier high) — from TWO compounding causes: the
// pre-R6 brand matcher had no integration-token exclusion at all (so
// "PayPal integration" tripped name_brand_match), AND
// selfSendBeforeExternal was uncapped. Both are now fixed independently
// (R6's BrandSet.Matches integration-token gate; R1's cap here) — this
// fixture proves the COMBINATION resolves too, not just either fix in
// isolation.
func TestReplay_SelfSendBrandNameStaysBelowHigh(t *testing.T) {
	view := runReplay(t, "benign_selfsend_brandname.jsonl", "acct_example_selfsend_brand_1")
	if view.Tier == "high" {
		t.Errorf("benign_selfsend_brandname: tier = high (score %v), want low or medium\nsignals: %+v", view.Score, view.Signals)
	}
	assertBand(t, "benign_selfsend_brandname", view.Score, 0.05, 0.35)
}

// TestReplay_WebmailBlastReachesAtLeastMedium replays eval/fixtures/
// webmail_blast.jsonl — S2b's B2 fixture: a brand-new account sending 100
// recipients, all on a single consumer webmail domain, within its first
// 10 minutes, with entirely neutral subject lines (no brand mentioned at
// all). Addresses a common bulk-phishing shape on volume and webmail
// concentration ALONE, with no brand signal to lean on — must reach at
// least tier "medium".
//
// Round 2's R1 fix round raised the band from medium-sized ([0.55, 0.85])
// to this: with no prior sending history at all, burst_factor now reads
// this 100-in-10-minutes burst at close to full strength on BOTH
// sends_10m_max and webmail_sends_1h at once (the account's single
// webmail domain means every one of these new weights fires together,
// unlike a fixture that splits its volume across webmail and non-webmail
// recipients) — the qualitative claim ("at least medium") still holds
// with room to spare; the fixture now clears all the way to `high`.
func TestReplay_WebmailBlastReachesAtLeastMedium(t *testing.T) {
	view := runReplay(t, "webmail_blast.jsonl", "acct_example_webmail_blast_1")
	if view.Tier == "low" {
		t.Errorf("webmail_blast: tier = low (score %v), want medium or high\nsignals: %+v", view.Score, view.Signals)
	}
	assertBand(t, "webmail_blast", view.Score, 0.9, 1.0)
}

// TestReplay_SingleBrandBlastReachesHigh replays eval/fixtures/
// single_brand_blast_45m.jsonl — S2b's B2 fixture: a brand-new account
// sending 240 recipients across consumer webmail domains over 45 minutes,
// every subject line mentioning the identical fictional brand
// ("Glowbank" — eval/fixtures/test_brands.yaml, never a real brand name).
// Combining a repeated brand mention with a sustained volume burst is a
// stronger signal than either alone (contrast webmail_blast, which has
// volume but no brand, and day0_marketplace_seller, which has a brand but
// a much smaller volume) — expected tier: "high".
func TestReplay_SingleBrandBlastReachesHigh(t *testing.T) {
	view := runReplay(t, "single_brand_blast_45m.jsonl", "acct_example_single_brand_blast_1")
	if view.Tier != "high" {
		t.Errorf("single_brand_blast_45m: tier = %q (score %v), want high\nsignals: %+v", view.Tier, view.Score, view.Signals)
	}
	assertBand(t, "single_brand_blast_45m", view.Score, 0.95, 1.0)
}

// TestReplay_EstablishedNewsletterStaysBelowMedium replays eval/fixtures/
// established_newsletter_burst.jsonl — S2b's B1 fixture: a 60-day-old,
// paid, established newsletter sender with a real history of periodic
// sends, whose most recent send happens to burst 300 recipients (all
// consumer webmail) within 10 minutes — the exact shape a lifetime-max
// volume feature with no account-age awareness would flag `high` on
// alone, and the flag would never decay for an account that keeps
// operating normally afterward. Round 2's R1 fix round: this account HAS
// a real prior sending history (its own weekly sends), so
// burstFactor(current 300-recipient burst, that real prior baseline)
// reads the burst as only moderately elevated rather than maximally
// unusual, and ageDecayFactor discounts it further at 60 days old (near
// its 0.2 floor) — so this fixture must stay below tier "medium".
func TestReplay_EstablishedNewsletterStaysBelowMedium(t *testing.T) {
	view := runReplay(t, "established_newsletter_burst.jsonl", "acct_example_established_newsletter_1")
	if view.Tier != "low" {
		t.Errorf("established_newsletter_burst: tier = %q (score %v), want low\nsignals: %+v", view.Tier, view.Score, view.Signals)
	}
	assertBand(t, "established_newsletter_burst", view.Score, 0.0, 0.2)
}

// TestReplay_Day0MarketplaceSellerStaysBelowHigh replays eval/fixtures/
// day0_marketplace_seller.jsonl — S2b's B2 fixture: a brand-new account
// whose agent is named after a fictional shop brand ("Fictashop" —
// eval/fixtures/test_brands.yaml), no integration token in that name,
// sending "Your Fictashop order has shipped" to 30 webmail buyers over
// its first hour — a plausible day-0 legitimate marketplace seller as
// much as a suspicious blast. Also exercises S2 in a realistic combined
// scenario: the agent's own name already matches "Fictashop"
// (name_brand_match=1), so subject_brand_match must NOT also credit the
// identical brand mentioned in every subject line. Must stay below tier
// "high".
func TestReplay_Day0MarketplaceSellerStaysBelowHigh(t *testing.T) {
	view := runReplay(t, "day0_marketplace_seller.jsonl", "acct_example_marketplace_seller_1")
	if view.Tier == "high" {
		t.Errorf("day0_marketplace_seller: tier = high (score %v), want low or medium\nsignals: %+v", view.Score, view.Signals)
	}
	assertBand(t, "day0_marketplace_seller", view.Score, 0.6, 0.78)
}

// TestReplay_DormantBrandedBurst8dReachesHigh replays eval/fixtures/
// dormant_branded_burst_8d.jsonl — round 2's R1(a) required outcome: an
// account that sat dormant for 8 days (one day PAST the old hard 7-day
// gate) then bursts 100 branded webmail recipients within 10 minutes.
// The old calendar gate was evadable by simply waiting past it — this
// account would have read as fully "established" under it despite never
// having sent anything before. burst_factor (no prior history at this
// subject at all) reads the burst at close to full strength, discounted
// only slightly by ageDecayFactor at 8 days (still close to 1.0 — the
// floor doesn't bind until ~day 25) — must reach at least medium, and in
// fact clears all the way to high.
func TestReplay_DormantBrandedBurst8dReachesHigh(t *testing.T) {
	view := runReplay(t, "dormant_branded_burst_8d.jsonl", "acct_example_dormant_branded_8d_1")
	if view.Tier == "low" {
		t.Errorf("dormant_branded_burst_8d: tier = low (score %v), want medium or high\nsignals: %+v", view.Score, view.Signals)
	}
	assertBand(t, "dormant_branded_burst_8d", view.Score, 0.9, 1.0)
}

// TestReplay_PaidLaunch5dStaysBelowHigh replays eval/fixtures/
// paid_launch_5d.jsonl — round 2's R1(c) required outcome: a 5-day-old
// paid SaaS account, with a real (if modest) history of prior sends,
// that pushes a launch-announcement burst of 250 webmail recipients over
// 15 minutes with an entirely neutral subject line (no brand mentioned).
// Its prior sends give burstFactor a real, non-trivial baseline to
// compare against (unlike dormant_branded_burst_8d/dormant_then_blast,
// which have none) — the burst reads as elevated but not nearly as
// extreme, and ageDecayFactor at 5 days is barely discounted yet, so this
// fixture must stay below `high` on the strength of that history alone,
// with a comfortable margin.
func TestReplay_PaidLaunch5dStaysBelowHigh(t *testing.T) {
	view := runReplay(t, "paid_launch_5d.jsonl", "acct_example_paid_launch_5d_1")
	if view.Tier == "high" {
		t.Errorf("paid_launch_5d: tier = high (score %v), want low or medium\nsignals: %+v", view.Score, view.Signals)
	}
	assertBand(t, "paid_launch_5d", view.Score, 0.0, 0.4)
}

// TestReplay_WebmailSpread1hMediumBand replays eval/fixtures/
// webmail_spread_1h.jsonl — round 2's R6: a fixture that isolates
// webmail_sends_1h from sends_10m_max specifically, so the mutation
// sweep can prove webmail_sends_1h load-bearing on its own (see
// mutation_test.go's "webmail_spread_1h" scenario). 80 webmail
// recipients spread evenly across a full hour (8-minute intervals) —
// deliberately NOT concentrated into any 10-minute window the way
// webmail_blast's burst is, so sends_10m_max stays modest (20) while
// webmail_sends_1h/sends_1h/distinct_recipients_1h (80 each) carry most
// of the signal.
func TestReplay_WebmailSpread1hMediumBand(t *testing.T) {
	view := runReplay(t, "webmail_spread_1h.jsonl", "acct_example_webmail_spread_1")
	if view.Tier == "high" {
		t.Errorf("webmail_spread_1h: tier = high (score %v), want low or medium\nsignals: %+v", view.Score, view.Signals)
	}
	assertBand(t, "webmail_spread_1h", view.Score, 0.65, 0.78)
}

// TestReplay_CommunityGroupPhotoWalkKnownGap replays eval/fixtures/
// community_group_photo_walk.jsonl — round 2's R3 required fixture: a
// day-0 community-group account (a genuine "Fictabook" fan/community
// persona, not an impersonator) posting an ordinary, non-suspicious
// update about a real-world activity ("Fictabook photo walk this
// Saturday" — deliberately no community PHRASE like "group meetup"/"fan
// club"/"community event", so R3's phrase gate correctly does not
// suppress the brand match here) to 80 webmail members within 10
// minutes.
//
// DOCUMENTED TRADE-OFF (R3: "if the fixture can't be held below high
// without losing R1's outcomes, document the trade-off and choose"):
// this fixture reaches `high`. Structurally it is close to
// indistinguishable, on THIS feature set alone, from a malicious
// day-0 brand-impersonation blast (webmail_blast.jsonl,
// single_brand_blast_45m.jsonl, dormant_branded_burst_8d.jsonl): a
// brand-new account, no prior sending history, a webmail-concentrated
// burst of comparable size, and a subject line that matches a curated
// brand. R1's blocker-level outcomes (a calendar-evadable dormant
// account must still reach `high` on volume alone, an established
// sender must not) require sends_10m_max/webmail_sends_1h to carry a
// day-0, no-history burst most of the way to `high` by themselves —
// weakening that weight to spare this fixture would also weaken
// dormant_branded_burst_8d's own required outcome. This v0 feature set
// has no signal for "a real, ongoing community persona" (that needs
// something like verified account age/ownership or content semantics
// beyond phrase-detection) — choosing to keep R1's blocker outcomes
// intact over this should-fix item's exact tier target, and recording
// the choice here rather than silently accepting either a weakened
// blocker or an undocumented regression.
func TestReplay_CommunityGroupPhotoWalkKnownGap(t *testing.T) {
	view := runReplay(t, "community_group_photo_walk.jsonl", "acct_example_community_group_1")
	t.Logf("community_group_photo_walk: tier=%q score=%v (documented known gap — see this test's own doc comment)", view.Tier, view.Score)
	assertBand(t, "community_group_photo_walk", view.Score, 0.9, 1.0)
}
