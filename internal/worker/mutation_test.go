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
// its magnitude is deliberately kept (upgrade_delay_min's -0.0005 is easy
// to miss in a fixture-band check, impossible to miss here).
//
// subject_age_h and upgrade_delay_min are the only two negative entries:
// an OLDER account, or one that has gone a long time with no PAID
// upgrade (design's clamp ceiling), is LESS likely to be a fresh
// throwaway — see local_weights.yaml's own comments for the full
// rationale on each.
var goldenWeightSigns = map[string]int{
	"subject_age_h":                      -1,
	"resource_velocity_1h":               1,
	"resource_total":                     1,
	"key_velocity_1h":                    1,
	"key_total":                          1,
	"upgrade_delay_min":                  -1,
	"upgraded":                           1,
	"declines_before_first_success":      1,
	"first_funding_prepaid":              1,
	"name_brand_match":                   1,
	"name_has_at":                        1,
	"first_day_distinct_domains":         1,
	"self_send_before_external":          1,
	"linked_deleted_n":                   1,
	"linked_labelled_abusive_n":          1,
	"fingerprint_seen_on_other_subjects": 1,
	"neighbors_truncated":                1,
	"burst_ratio_24h_vs_lifetime":        1,
	// [S2b]: every one of these is a non-negative count/share, so more of
	// it is never LESS risky — all seven are positive.
	"sends_10m_max":           1,
	"sends_1h":                1,
	"sends_first_day":         1,
	"webmail_recipient_share": 1,
	"webmail_sends_1h":        1,
	"distinct_recipients_1h":  1,
	"subject_brand_match":     1,
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

// mutationScenarios returns every committed replay fixture's scenario,
// with the identical bands replay_test.go/replay_churn_test.go assert
// through the full DB-backed worker+store pipeline — see those tests for
// why each band is what it is. Kept in sync with them deliberately (not
// derived from them mechanically): this file exists specifically so a
// weight mutation can be checked against many scenarios fast, without
// needing those same assertions to also be data-driven from here.
func mutationScenarios(t *testing.T) []mutationScenario {
	t.Helper()
	brands := loadShippedBrands(t)
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
		// [S2b] send-volume/webmail/recipient/subject-brand fixtures — see
		// replay_test.go's TestReplay_WebmailBlastReachesHigh /
		// TestReplay_SubjectLureOnlyReachesHigh / TestReplay_*StaysBelowHigh
		// for why each band is what it is; kept in sync deliberately, not
		// derived, same convention as every scenario above.
		{"webmail_blast", extractFixture(t, brands, webmail, "webmail_blast.jsonl", time.Minute, false, feature.NeighborEvidence{}), 0.9, 1.0},
		{"subject_lure_only", extractFixture(t, brands, webmail, "subject_lure_only.jsonl", time.Minute, false, feature.NeighborEvidence{}), 0.85, 1.0},
		{"benign_newsletter_webmail", extractFixture(t, brands, webmail, "benign_newsletter_webmail.jsonl", time.Minute, false, feature.NeighborEvidence{}), 0.0, 0.12},
		{"benign_support_desk_webmail", extractFixture(t, brands, webmail, "benign_support_desk_webmail.jsonl", time.Minute, false, feature.NeighborEvidence{}), 0.0, 0.1},
		{"benign_shop_integration_subject", extractFixture(t, brands, webmail, "benign_shop_integration_subject.jsonl", time.Minute, false, feature.NeighborEvidence{}), 0.0, 0.15},
	}
	return append(scenarios, isolatedWeightScenarios()...)
}

// isolatedWeightScenarios is R2 round 2: eight of the eighteen weights
// (resource_total/key_total — minor companions to their velocity
// counterparts; upgrade_delay_min — deliberately shrunk ~20x by B5 so it
// can't swamp the model; subject_age_h, name_has_at,
// linked_labelled_abusive_n, neighbors_truncated,
// burst_ratio_24h_vs_lifetime) never move any of the WIDE, realistic
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
			"resource_velocity_1h":               0.75,
			"resource_total":                     1.25,
			"key_velocity_1h":                    0.5,
			"key_total":                          1,
			"upgraded":                           0.25,
			"declines_before_first_success":      0.5,
			"first_funding_prepaid":              0.25,
			"name_brand_match":                   0.25,
			"first_day_distinct_domains":         0.75,
			"self_send_before_external":          0.5,
			"linked_deleted_n":                   0.5,
			"fingerprint_seen_on_other_subjects": 0.25,
		}
	}
	withTarget := func(name string, value float64) map[string]float64 {
		v := backdrop()
		v[name] = value
		return v
	}
	return []mutationScenario{
		// subject_age_h: negative weight, so zeroing INCREASES risk —
		// base=0.160, zeroed=0.235.
		{"isolated_subject_age_h", withTarget("subject_age_h", 24), 0.05, 0.20},
		// resource_total: base=0.309, zeroed=0.231.
		{"isolated_resource_total", withTarget("resource_total", 20), 0.27, 0.36},
		// key_total: base=0.269, zeroed=0.231.
		{"isolated_key_total", withTarget("key_total", 10), 0.25, 0.30},
		// upgrade_delay_min: negative weight, zeroing INCREASES risk —
		// base=0.130, zeroed=0.235.
		{"isolated_upgrade_delay_min", withTarget("upgrade_delay_min", 1440), 0.05, 0.185},
		// name_has_at: base=0.336, zeroed=0.235.
		{"isolated_name_has_at", withTarget("name_has_at", 1), 0.29, 0.40},
		// linked_labelled_abusive_n: base=0.579, zeroed=0.235.
		{"isolated_linked_labelled_abusive_n", withTarget("linked_labelled_abusive_n", 1), 0.42, 0.75},
		// neighbors_truncated: base=0.293, zeroed=0.235.
		{"isolated_neighbors_truncated", withTarget("neighbors_truncated", 1), 0.27, 0.35},
		// burst_ratio_24h_vs_lifetime: base=0.293, zeroed=0.235.
		{"isolated_burst_ratio_24h_vs_lifetime", withTarget("burst_ratio_24h_vs_lifetime", 1.0), 0.27, 0.35},

		// [S2b]: none of the committed wide fixtures are individually
		// sensitive enough to any ONE of these six weights alone to prove
		// it's load-bearing (TestLocalWeights_GoldenSignsAndNonZero already
		// proves each is configured, non-zero and correctly signed) — same
		// reason as the eight above. zeroed=0.235 throughout (the same
		// shared backdrop).
		//
		// sends_10m_max: base=0.530, zeroed=0.235.
		{"isolated_sends_10m_max", withTarget("sends_10m_max", 100), 0.45, 0.60},
		// sends_1h: base=0.293, zeroed=0.235. 300 = internal/feature's
		// unexported sendsVolumeCap (the send-volume features' shared
		// ceiling) — literal here since it isn't importable from this
		// package.
		{"isolated_sends_1h", withTarget("sends_1h", 300), 0.27, 0.35},
		// sends_first_day: base=0.293, zeroed=0.235 (also at sendsVolumeCap).
		{"isolated_sends_first_day", withTarget("sends_first_day", 300), 0.27, 0.35},
		// webmail_recipient_share: base=0.263, zeroed=0.235. Capped at its
		// own natural ceiling of 1.0 (a share), unlike the raw counts above.
		{"isolated_webmail_recipient_share", withTarget("webmail_recipient_share", 1.0), 0.25, 0.30},
		// webmail_sends_1h: base=0.604, zeroed=0.235.
		{"isolated_webmail_sends_1h", withTarget("webmail_sends_1h", 200), 0.50, 0.70},
		// distinct_recipients_1h: base=0.293, zeroed=0.235 (also at
		// sendsVolumeCap).
		{"isolated_distinct_recipients_1h", withTarget("distinct_recipients_1h", 300), 0.27, 0.35},
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
