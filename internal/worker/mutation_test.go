package worker

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/tokencanopy/abusekit/internal/event"
	"github.com/tokencanopy/abusekit/internal/feature"
	"github.com/tokencanopy/abusekit/internal/model"
	"github.com/tokencanopy/abusekit/internal/model/local"
)

// goldenWeightSigns is R2 round 2's golden table: every feature
// config/rules.yaml's new_account_velocity rule reads must have a
// NON-ZERO weight in config/local_weights.yaml, with this EXACT sign
// (+1: increases risk; -1: decreases risk). Unlike a score-band
// assertion against a fixture, this check has no numerical fragility at
// all — a weight set to exactly zero, dropped from the file entirely, or
// flipped to the wrong sign fails immediately, regardless of how small
// its magnitude is deliberately kept (core.upgrade_delay_min's -0.0005 is easy
// to miss in a fixture-band check, impossible to miss here).
//
// core.subject_age_h and core.upgrade_delay_min are the only two negative entries:
// an OLDER account, or one that has gone a long time with no PAID
// upgrade (design's clamp ceiling), is LESS likely to be a fresh
// throwaway — see local_weights.yaml's own comments for the full
// rationale on each.
var goldenWeightSigns = map[string]int{
	"core.subject_age_h":                      -1,
	"core.resource_velocity_1h":               1,
	"core.resource_total":                     1,
	"core.credential_velocity_1h":             1,
	"core.credential_total":                   1,
	"core.upgrade_delay_min":                  -1,
	"core.upgraded":                           1,
	"core.declines_before_first_success":      1,
	"core.first_funding_prepaid":              1,
	"brand.name_match":                        1,
	"brand.name_has_at":                       1,
	"email.first_day_distinct_domains":        1,
	"email.self_send_before_external":         1,
	"core.linked_deleted_n":                   1,
	"core.linked_labelled_abusive_n":          1,
	"core.fingerprint_seen_on_other_subjects": 1,
	"core.neighbors_truncated":                1,
	"core.burst_ratio_24h_vs_lifetime":        1,
	"email.sends_10m_max":                     1,
	"email.sends_1h":                          1,
	"email.sends_first_day":                   1,
	"email.webmail_recipient_share":           1,
	"email.webmail_sends_1h":                  1,
	"email.distinct_recipients_1h":            1,
	"email.subject_brand_match":               1,
}

// TestLocalWeights_GoldenSignsAndNonZero is R2 round 2's static half of
// "the mutation check must be able to fail for every weight": rather than
// relying on SOME fixture's score happening to be sensitive enough to
// notice a change, this checks config/local_weights.yaml directly against
// goldenWeightSigns, for every feature config/rules.yaml's rules actually
// read (not a hand-copied duplicate of that list — a rule referencing a
// feature this table doesn't know about, or vice versa, fails loudly
// rather than silently skipping it).
func TestLocalWeights_GoldenSignsAndNonZero(t *testing.T) {
	weights, err := local.LoadWeightsFile(filepath.Join(repoRoot(t), "config", "local_weights.yaml"))
	if err != nil {
		t.Fatalf("LoadWeightsFile: %v", err)
	}
	cfg := loadShippedConfig(t)

	ruleInputs := map[string]bool{}
	for _, r := range cfg.Rules {
		if r.Scorer != "local" {
			continue
		}
		for _, in := range r.Inputs {
			ruleInputs[in] = true
		}
	}

	for name := range ruleInputs {
		if _, ok := goldenWeightSigns[name]; !ok {
			t.Errorf("config/rules.yaml's local-scored rule(s) read feature %q, which goldenWeightSigns doesn't know about — add it", name)
		}
	}
	for name := range goldenWeightSigns {
		if !ruleInputs[name] {
			t.Errorf("goldenWeightSigns lists %q, but no local-scored rule in config/rules.yaml actually reads it — remove it or add it to the rule's inputs", name)
		}
	}

	for name, wantSign := range goldenWeightSigns {
		w, ok := weights.Weight[name]
		if !ok {
			t.Errorf("config/local_weights.yaml is missing a weight for %q", name)
			continue
		}
		if w == 0 {
			t.Errorf("config/local_weights.yaml has weight[%q] = 0, want non-zero (sign %+d)", name, wantSign)
			continue
		}
		gotSign := 1
		if w < 0 {
			gotSign = -1
		}
		if gotSign != wantSign {
			t.Errorf("config/local_weights.yaml has weight[%q] = %v (sign %+d), want sign %+d", name, w, gotSign, wantSign)
		}
	}
}

// mutationScenario is one committed replay fixture's feature vector, band,
// and the outcome it must reach — mirroring one of replay_test.go's
// DB-backed assertions, but computed directly via feature.Extract (with a
// canned Neighbors reproducing exactly what the real store-backed replay
// test observes) so TestWeightMutation can score it many times over
// without a database.
type mutationScenario struct {
	name     string
	features map[string]float64
	min, max float64
}

// fakeNeighborsWith returns a feature.Neighbors reporting exactly ev,
// regardless of which subject is asked about.
type fakeNeighborsWith struct{ ev feature.NeighborEvidence }

func (f fakeNeighborsWith) Evidence(context.Context, string, string) (feature.NeighborEvidence, error) {
	return f.ev, nil
}

// extractFixture loads fixtureFile, runs it through feature.Extract as of
// lastEventAt(events)+after (or, for burst's before-first-send case, only
// the events up to and including the last one before the first
// content.sent), and returns the resulting feature map. ev is the
// same-tenant linking evidence to report (feature.NeighborEvidence{} for
// every fixture except churn's).
func extractFixture(t *testing.T, brands feature.BrandSet, webmail feature.WebmailSet, fixtureFile string, after time.Duration, stopBeforeContentSent bool, ev feature.NeighborEvidence) map[string]float64 {
	t.Helper()
	events := loadFixture(t, filepath.Join(repoRoot(t), "eval", "fixtures", fixtureFile))
	if stopBeforeContentSent {
		var setup []event.Event
		for _, e := range events {
			if e.Type == "content.sent" {
				break
			}
			setup = append(setup, e)
		}
		events = setup
	}
	now := lastEventAt(events).Add(after)
	res, err := feature.Extract(context.Background(), testTenant, "subject", events, fakeNeighborsWith{ev}, feature.DefaultWindows(now), brands, webmail)
	if err != nil {
		t.Fatalf("feature.Extract(%s): %v", fixtureFile, err)
	}
	return res.Features.Map()
}

// loadShippedWebmail loads the real config/webmail.yaml this repo ships —
// the S2b analogue of loadShippedBrands.
func loadShippedWebmail(t *testing.T) feature.WebmailSet {
	t.Helper()
	w, err := feature.LoadWebmailFile(filepath.Join(repoRoot(t), "config", "webmail.yaml"))
	if err != nil {
		t.Fatalf("load webmail.yaml: %v", err)
	}
	return w
}

// loadTestBrands merges the real, public config/brands.yaml with
// eval/fixtures/test_brands.yaml's entirely fictional entries (S2b's
// hygiene rule: a replay fixture never references a real brand name) —
// used wherever a mutation scenario or fixture needs brand.name_match/
// email.subject_brand_match to fire against a brand a fixture actually mentions.
func loadTestBrands(t *testing.T) feature.BrandSet {
	t.Helper()
	shipped := loadShippedBrands(t)
	extra, err := feature.LoadBrandsFile(filepath.Join(repoRoot(t), "eval", "fixtures", "test_brands.yaml"))
	if err != nil {
		t.Fatalf("load eval/fixtures/test_brands.yaml: %v", err)
	}
	return feature.MergeBrandSets(shipped, extra)
}

// mutationScenarios returns every committed replay fixture's scenario,
// with the identical bands replay_test.go/replay_churn_test.go assert
// through the full DB-backed worker+store pipeline — see those tests for
// why each band is what it is. Kept in sync with them deliberately (not
// derived from them mechanically): this file exists specifically so a
// weight mutation can be checked against many scenarios fast, without
// needing those same assertions to also be data-driven from here.
func mutationScenarios(t *testing.T) []mutationScenario {
	t.Helper()
	brands := loadTestBrands(t)
	webmail := loadShippedWebmail(t)

	churnEvents := loadFixture(t, filepath.Join(repoRoot(t), "eval", "fixtures", "churn.jsonl"))
	const eventsPerSubject = 5
	subject3Onboarding := churnEvents[2*eventsPerSubject : 2*eventsPerSubject+4]
	now3 := lastEventAt(subject3Onboarding).Add(time.Second)
	res3, err := feature.Extract(context.Background(), testTenant, "acct_example_churn_3", subject3Onboarding, fakeNeighborsWith{feature.NeighborEvidence{DeletedCount: 2, FingerprintShared: true}}, feature.DefaultWindows(now3), brands, webmail)
	if err != nil {
		t.Fatalf("feature.Extract(churn subject 3): %v", err)
	}
	subject6Onboarding := churnEvents[5*eventsPerSubject : 5*eventsPerSubject+4]
	now6 := lastEventAt(subject6Onboarding).Add(time.Second)
	res6, err := feature.Extract(context.Background(), testTenant, "acct_example_churn_6", subject6Onboarding, fakeNeighborsWith{feature.NeighborEvidence{DeletedCount: 5, FingerprintShared: true}}, feature.DefaultWindows(now6), brands, webmail)
	if err != nil {
		t.Fatalf("feature.Extract(churn subject 6): %v", err)
	}

	scenarios := []mutationScenario{
		{"reference_operator", extractFixture(t, brands, webmail, "reference_operator.jsonl", time.Minute, false, feature.NeighborEvidence{}), 0.99, 1.0},
		{"benign_transactional", extractFixture(t, brands, webmail, "benign_transactional.jsonl", time.Minute, false, feature.NeighborEvidence{}), 0.0, 0.05},
		{"burst_before_send", extractFixture(t, brands, webmail, "burst.jsonl", 15*time.Second, true, feature.NeighborEvidence{}), 0.9, 1.0},
		{"burst_final", extractFixture(t, brands, webmail, "burst.jsonl", time.Minute, false, feature.NeighborEvidence{}), 0.9, 1.0},
		{"benign_fast_onboarding", extractFixture(t, brands, webmail, "benign_fast_onboarding.jsonl", time.Minute, false, feature.NeighborEvidence{}), 0.15, 0.45}, // upper edge widened, D2 round 3 — see replay_test.go's TestReplay_BenignFastOnboardingStaysBelowHigh
		{"benign_integration_heavy", extractFixture(t, brands, webmail, "benign_integration_heavy.jsonl", time.Minute, false, feature.NeighborEvidence{}), 0.05, 0.35},
		{"dormant_then_blast", extractFixture(t, brands, webmail, "dormant_then_blast.jsonl", time.Minute, false, feature.NeighborEvidence{}), 0.8, 0.98},
		{"churn_subject_3", res3.Features.Map(), 0.8, 0.95},
		{"churn_subject_saturated", res6.Features.Map(), 0.9, 1.0},

		// --- S2b fixtures: send volume, webmail, recipient hashing,
		// subject-brand matching. Bands measured against the shipped
		// weights with a deliberate margin on both sides (never pinned to
		// the exact computed value) — see the PR body's "which fixture
		// bounds which weight" table and the sensitivity windows recorded
		// there.
		{"webmail_blast", extractFixture(t, brands, webmail, "webmail_blast.jsonl", time.Minute, false, feature.NeighborEvidence{}), 0.9, 1.0}, // round 2, R1: was [0.55, 0.85] — see replay_test.go's own comment
		{"single_brand_blast_45m", extractFixture(t, brands, webmail, "single_brand_blast_45m.jsonl", time.Minute, false, feature.NeighborEvidence{}), 0.95, 1.0},
		{"established_newsletter_burst", extractFixture(t, brands, webmail, "established_newsletter_burst.jsonl", time.Minute, false, feature.NeighborEvidence{}), 0.0, 0.2},
		{"day0_marketplace_seller", extractFixture(t, brands, webmail, "day0_marketplace_seller.jsonl", time.Minute, false, feature.NeighborEvidence{}), 0.6, 0.78},
		{"benign_receipts_fanout", extractFixture(t, brands, webmail, "benign_receipts_fanout.jsonl", time.Minute, false, feature.NeighborEvidence{}), 0.4, 0.55}, // round 2, R1: was [0.05, 0.4] — see replay_test.go's own comment

		// --- Round 2, R1 fixtures: history-relative volume signal, no
		// hard calendar-age gate.
		{"dormant_branded_burst_8d", extractFixture(t, brands, webmail, "dormant_branded_burst_8d.jsonl", time.Minute, false, feature.NeighborEvidence{}), 0.9, 1.0},
		{"paid_launch_5d", extractFixture(t, brands, webmail, "paid_launch_5d.jsonl", time.Minute, false, feature.NeighborEvidence{}), 0.0, 0.4},
		{"webmail_spread_1h", extractFixture(t, brands, webmail, "webmail_spread_1h.jsonl", time.Minute, false, feature.NeighborEvidence{}), 0.65, 0.78},

		// --- Round 2, R6: real replay fixtures bounding email.sends_1h,
		// email.sends_first_day, email.distinct_recipients_1h and email.subject_brand_match
		// (replacing the isolated synthetic scenarios these four used to
		// need — see isolatedWeightScenarios' own comment). Three
		// DISTINCT fixtures, each isolating one feature from its
		// siblings, so swapping which weight applies to which feature
		// would fail a test:
		{"moderate_volume_single_brand", extractFixture(t, brands, webmail, "moderate_volume_single_brand.jsonl", time.Minute, false, feature.NeighborEvidence{}), 0.6, 0.8},
		{"repeat_recipient_resend", extractFixture(t, brands, webmail, "repeat_recipient_resend.jsonl", time.Minute, false, feature.NeighborEvidence{}), 0.855, 0.875},
		firstDayBurstThenQuietScenario(t, brands, webmail),
	}
	return append(scenarios, isolatedWeightScenarios()...)
}

// firstDayBurstThenQuietScenario is round 2's R6 fixture bounding
// email.sends_first_day specifically: first_day_burst_then_quiet.jsonl's burst
// happens entirely within the subject's first day, but — unlike
// extractFixture's usual "lastEventAt + a short buffer" evaluation
// instant — this scenario evaluates 26 hours after the subject's FIRST
// event, well past currentBurstWindow (24h): email.sends_10m_max/email.sends_1h/
// email.distinct_recipients_1h/email.webmail_sends_1h all read 0 (the burst has aged
// out of the CURRENT window), isolating email.sends_first_day (a permanent
// fact, unaffected by how long ago its own window closed) as the only
// one of the four with a non-zero value here.
func firstDayBurstThenQuietScenario(t *testing.T, brands feature.BrandSet, webmail feature.WebmailSet) mutationScenario {
	t.Helper()
	events := loadFixture(t, filepath.Join(repoRoot(t), "eval", "fixtures", "first_day_burst_then_quiet.jsonl"))
	// events[0] is subject.created, the fixture's earliest event (the
	// file is written in chronological order — see loadFixture's own
	// contract).
	now := events[0].At.Add(26 * time.Hour)
	res, err := feature.Extract(context.Background(), testTenant, "acct_example_first_day_quiet_1", events, feature.NoNeighbors, feature.DefaultWindows(now), brands, webmail)
	if err != nil {
		t.Fatalf("feature.Extract(first_day_burst_then_quiet.jsonl): %v", err)
	}
	return mutationScenario{"first_day_burst_then_quiet", res.Features.Map(), 0.56, 0.62}
}

// isolatedWeightScenarios is R2 round 2: eight of the eighteen weights
// (core.resource_total/core.credential_total — minor companions to their velocity
// counterparts; core.upgrade_delay_min — deliberately shrunk ~20x by B5 so it
// can't swamp the model; core.subject_age_h, brand.name_has_at,
// core.linked_labelled_abusive_n, core.neighbors_truncated,
// core.burst_ratio_24h_vs_lifetime) never move any of the WIDE, realistic
// fixture bands above by enough to cross an edge, even though each one
// measurably moves the score (see TestAblation_EveryWeightedFeatureMovesTheScore).
// TestLocalWeights_GoldenSignsAndNonZero already proves each is
// configured, non-zero, and correctly signed; these scenarios additionally
// prove each one's OWN weight is what a SCORE actually depends on, isolated
// from the fixtures' other signals: a shared, moderate backdrop (roughly
// half of fullFeatureVector's values, landing baseline risk in the
// sigmoid's sensitive middle rather than a saturated tail) plus ONE target
// feature at a meaningfully large value, with a band computed at the
// midpoint between the weight's baseline contribution and zero — tight
// enough to catch the target's own removal, loose enough not to be
// fragile against unrelated future retuning. See the "which fixture
// bounded which weight" table (PR body / local_weights.yaml comments) for
// the exact baseline/zeroed risk values these bands were derived from.
func isolatedWeightScenarios() []mutationScenario {
	backdrop := func() map[string]float64 {
		return map[string]float64{
			"core.resource_velocity_1h":               0.75,
			"core.resource_total":                     1.25,
			"core.credential_velocity_1h":             0.5,
			"core.credential_total":                   1,
			"core.upgraded":                           0.25,
			"core.declines_before_first_success":      0.5,
			"core.first_funding_prepaid":              0.25,
			"brand.name_match":                        0.25,
			"email.first_day_distinct_domains":        0.75,
			"email.self_send_before_external":         0.5,
			"core.linked_deleted_n":                   0.5,
			"core.fingerprint_seen_on_other_subjects": 0.25,
		}
	}
	withTarget := func(name string, value float64) map[string]float64 {
		v := backdrop()
		v[name] = value
		return v
	}
	return []mutationScenario{
		// core.subject_age_h: negative weight, so zeroing INCREASES risk —
		// base=0.160, zeroed=0.235.
		{"isolated_subject_age_h", withTarget("core.subject_age_h", 24), 0.05, 0.20},
		// core.resource_total: base=0.309, zeroed=0.231.
		{"isolated_resource_total", withTarget("core.resource_total", 20), 0.27, 0.36},
		// core.credential_total: base=0.269, zeroed=0.231.
		{"isolated_key_total", withTarget("core.credential_total", 10), 0.25, 0.30},
		// core.upgrade_delay_min: negative weight, zeroing INCREASES risk —
		// base=0.130, zeroed=0.235.
		{"isolated_upgrade_delay_min", withTarget("core.upgrade_delay_min", 1440), 0.05, 0.185},
		// brand.name_has_at: base=0.336, zeroed=0.235.
		{"isolated_name_has_at", withTarget("brand.name_has_at", 1), 0.29, 0.40},
		// core.linked_labelled_abusive_n: base=0.579, zeroed=0.235.
		{"isolated_linked_labelled_abusive_n", withTarget("core.linked_labelled_abusive_n", 1), 0.42, 0.75},
		// core.neighbors_truncated: base=0.293, zeroed=0.235.
		{"isolated_neighbors_truncated", withTarget("core.neighbors_truncated", 1), 0.27, 0.35},
		// core.burst_ratio_24h_vs_lifetime: base=0.293, zeroed=0.235.
		{"isolated_burst_ratio_24h_vs_lifetime", withTarget("core.burst_ratio_24h_vs_lifetime", 1.0), 0.27, 0.35},
		// Round 2's R6 fix round replaced the isolated synthetic scenarios
		// that used to bound email.sends_1h, email.sends_first_day,
		// email.distinct_recipients_1h and email.subject_brand_match with REAL replay
		// fixtures instead (see mutationScenarios' own "Round 2, R6"
		// block) — a reviewer's own finding: a synthetic-only scenario
		// proves a weight moves SOME feature vector's score, but not that
		// the shipped feature-extraction code actually produces that
		// vector for a real fixture, and three distinct fixtures (rather
		// than one shared one) mean swapping which weight applies to
		// which feature is something a real fixture's own band would
		// actually notice.
	}
}

func scoreScenario(t *testing.T, scorer *local.Scorer, sc mutationScenario) float64 {
	t.Helper()
	res, err := scorer.Score(context.Background(), model.ScoreRequest{
		Labels:   []string{"benign", "suspicious", "abusive"},
		Features: sc.features,
	})
	if err != nil {
		t.Fatalf("Score(%s): %v", sc.name, err)
	}
	return 1 - res.Probs["benign"]
}

func inBand(risk float64, sc mutationScenario) bool {
	return risk >= sc.min && risk <= sc.max
}

// TestWeightMutation_EveryWeightIsLoadBearing is B1 fix round's mutation
// check, tightened by R2 round 2: zeroing any SINGLE weight in the shipped
// config/local_weights.yaml must make at least one scenario's band
// assertion fail — every committed replay fixture PLUS isolatedWeightScenarios
// (added specifically because eight of the eighteen weights never crossed
// any WIDE, realistic fixture's band on their own). A weight that survives
// being zeroed out — every scenario still lands in its expected band —
// isn't actually load-bearing for anything this repo tests, which is
// exactly the gap a silent weight-tuning regression could hide behind.
//
// R2 round 2 removed the OLD ablation-vector FALLBACK entirely (no more
// "or move the ablation vector's score by more than epsilon" escape
// hatch) and the OLD "skip weights that are already zero" behaviour
// (TestLocalWeights_GoldenSignsAndNonZero already guarantees none of the
// eighteen ever ship as zero, so every single one is genuinely mutated and
// checked here, unconditionally). Ablation itself is unchanged and
// remains its own separate test (ablation_test.go).
func TestWeightMutation_EveryWeightIsLoadBearing(t *testing.T) {
	weights, err := local.LoadWeightsFile(filepath.Join(repoRoot(t), "config", "local_weights.yaml"))
	if err != nil {
		t.Fatalf("LoadWeightsFile: %v", err)
	}
	scenarios := mutationScenarios(t)

	// Sanity check: the UNMUTATED weights must pass every scenario's band
	// first, or a "mutation broke it" result below would be meaningless.
	baseline, err := local.New(weights)
	if err != nil {
		t.Fatalf("local.New (baseline): %v", err)
	}
	for _, sc := range scenarios {
		risk := scoreScenario(t, baseline, sc)
		if !inBand(risk, sc) {
			t.Fatalf("baseline (unmutated) weights: %s risk = %v, want in [%v, %v] — fix the weights before trusting the mutation check", sc.name, risk, sc.min, sc.max)
		}
	}

	for name, original := range weights.Weight {
		mutated := weights
		mutated.Weight = make(map[string]float64, len(weights.Weight))
		for k, v := range weights.Weight {
			mutated.Weight[k] = v
		}
		mutated.Weight[name] = 0

		scorer, err := local.New(mutated)
		if err != nil {
			t.Fatalf("local.New (mutated %s): %v", name, err)
		}

		brokeSomething := false
		for _, sc := range scenarios {
			if _, ok := sc.features[name]; !ok || sc.features[name] == 0 {
				continue // this scenario doesn't exercise this feature at all; zeroing its weight can't change it.
			}
			risk := scoreScenario(t, scorer, sc)
			if !inBand(risk, sc) {
				brokeSomething = true
				break
			}
		}
		if !brokeSomething {
			t.Errorf("zeroing weight %q (was %v) did not push any scenario (replay fixture or isolated) out of its band", name, original)
		}
	}
}

// --- Round 2, R1: no-cliff continuity probe ------------------------------

// TestR1_AgeDecayContinuityProbe is round 2's explicit "probe 6d23h, 7d1h
// and 8d and show continuity" requirement, at the SCORE level (not just
// the raw ageDecayFactor unit — see internal/feature's own
// TestAgeDecayFactor_NoCliff for that): three otherwise-identical
// accounts (a 100-recipient webmail burst, no prior sending history at
// all) that differ ONLY in age at the moment of scoring. Old round-1
// behaviour had a hard cliff exactly at 7 days (score would jump from a
// young-account value straight to 0 contribution); round 2's
// history-relative burstFactor + smooth ageDecayFactor must show no such
// jump — each step's score differs only by the ordinary slope of the age
// decay ramp.
func TestR1_AgeDecayContinuityProbe(t *testing.T) {
	weights, err := local.LoadWeightsFile(filepath.Join(repoRoot(t), "config", "local_weights.yaml"))
	if err != nil {
		t.Fatalf("LoadWeightsFile: %v", err)
	}
	scorer, err := local.New(weights)
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}
	brands := loadTestBrands(t)
	webmail := loadShippedWebmail(t)

	probeBase := time.Date(2031, time.August, 1, 0, 0, 0, 0, time.UTC)
	riskAt := func(age time.Duration) float64 {
		burstOffset := age - 10*time.Minute
		mk := func(id, typ string, offset time.Duration, data map[string]any) event.Event {
			return event.Event{ID: id, Subject: "acct_probe", Type: typ, At: probeBase.Add(offset), Data: data}
		}
		events := []event.Event{
			mk("s1", "subject.created", 0, map[string]any{"channel": "signup", "email_domain_class": "webmail", "identity_kind": "individual"}),
			mk("r1", "resource.created", time.Minute, map[string]any{"kind": "agent", "name": "Notifications Agent"}),
			mk("c1", "content.sent", burstOffset, map[string]any{"subject_line": "Weekly update", "recipient_domain": "gmail.com", "recipient_count": float64(50), "recipient_is_own_identity": false}),
			mk("c2", "content.sent", burstOffset+5*time.Minute, map[string]any{"subject_line": "Weekly update", "recipient_domain": "gmail.com", "recipient_count": float64(50), "recipient_is_own_identity": false}),
		}
		now := probeBase.Add(age)
		res, err := feature.Extract(context.Background(), testTenant, "acct_probe", events, feature.NoNeighbors, feature.DefaultWindows(now), brands, webmail)
		if err != nil {
			t.Fatalf("feature.Extract: %v", err)
		}
		sr, err := scorer.Score(context.Background(), model.ScoreRequest{Labels: []string{"benign", "suspicious", "abusive"}, Features: res.Features.Map()})
		if err != nil {
			t.Fatalf("Score: %v", err)
		}
		return 1 - sr.Probs["benign"]
	}

	at6d23h := riskAt(6*24*time.Hour + 23*time.Hour)
	at7d1h := riskAt(7*24*time.Hour + time.Hour)
	at8d := riskAt(8 * 24 * time.Hour)

	t.Logf("continuity probe: 6d23h=%.4f 7d1h=%.4f 8d=%.4f", at6d23h, at7d1h, at8d)

	// No cliff: consecutive probes must move by a small, continuous
	// amount, never by anywhere near a hard gate's full swing.
	const maxStep = 0.03
	if diff := at6d23h - at7d1h; diff < 0 || diff > maxStep {
		t.Errorf("score(6d23h)=%.4f -> score(7d1h)=%.4f moved by %.4f, want a small continuous step (<= %v)", at6d23h, at7d1h, diff, maxStep)
	}
	if diff := at7d1h - at8d; diff < 0 || diff > maxStep {
		t.Errorf("score(7d1h)=%.4f -> score(8d)=%.4f moved by %.4f, want a small continuous step (<= %v)", at7d1h, at8d, diff, maxStep)
	}
}

// --- Round 2, R6: the ×0.5/×2 sweep, committed as a test -----------------

// s2bWeightNames are the seven weights this PR adds — the ones
// TestWeightMutation_NewWeightsSurviveHalfAndDoubleSweep sweeps.
var s2bWeightNames = []string{
	"email.sends_10m_max", "email.sends_1h", "email.sends_first_day",
	"email.webmail_recipient_share", "email.webmail_sends_1h", "email.distinct_recipients_1h",
	"email.subject_brand_match",
}

// TestWeightMutation_NewWeightsSurviveHalfAndDoubleSweep is round 2's R6:
// "commit the ×0.5 and ×2 sweep as a test, or remove the claim from the
// docs and commits". TestWeightMutation_EveryWeightIsLoadBearing already
// proves every weight (including these seven) load-bearing via zeroing;
// this test additionally proves each of the seven NEW weights specifically
// via scaling, not only zeroing — for each one, scaling it to 0.5x OR 2x
// (independently) must move at least one committed scenario out of its
// band.
func TestWeightMutation_NewWeightsSurviveHalfAndDoubleSweep(t *testing.T) {
	weights, err := local.LoadWeightsFile(filepath.Join(repoRoot(t), "config", "local_weights.yaml"))
	if err != nil {
		t.Fatalf("LoadWeightsFile: %v", err)
	}
	scenarios := mutationScenarios(t)

	baseline, err := local.New(weights)
	if err != nil {
		t.Fatalf("local.New (baseline): %v", err)
	}
	for _, sc := range scenarios {
		risk := scoreScenario(t, baseline, sc)
		if !inBand(risk, sc) {
			t.Fatalf("baseline (unmutated) weights: %s risk = %v, want in [%v, %v] — fix the weights before trusting the sweep", sc.name, risk, sc.min, sc.max)
		}
	}

	for _, name := range s2bWeightNames {
		original, ok := weights.Weight[name]
		if !ok || original == 0 {
			t.Fatalf("config/local_weights.yaml is missing or has a zero weight for %q — TestLocalWeights_GoldenSignsAndNonZero should already have caught this", name)
		}

		brokeSomething := false
		for _, scale := range []float64{0.5, 2.0} {
			mutated := weights
			mutated.Weight = make(map[string]float64, len(weights.Weight))
			for k, v := range weights.Weight {
				mutated.Weight[k] = v
			}
			mutated.Weight[name] = original * scale

			scorer, err := local.New(mutated)
			if err != nil {
				t.Fatalf("local.New (%s x%v): %v", name, scale, err)
			}
			for _, sc := range scenarios {
				if _, ok := sc.features[name]; !ok || sc.features[name] == 0 {
					continue
				}
				risk := scoreScenario(t, scorer, sc)
				if !inBand(risk, sc) {
					brokeSomething = true
				}
			}
		}
		if !brokeSomething {
			t.Errorf("scaling weight %q to 0.5x or 2x (was %v) did not push any scenario out of its band", name, original)
		}
	}
}
