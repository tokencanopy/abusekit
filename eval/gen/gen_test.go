package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/tokencanopy/abusekit/eval"
	"github.com/tokencanopy/abusekit/internal/config"
	"github.com/tokencanopy/abusekit/internal/event"
	"github.com/tokencanopy/abusekit/internal/feature"
	"github.com/tokencanopy/abusekit/internal/model"
	"github.com/tokencanopy/abusekit/internal/model/local"
)

// repoRootForGenTest mirrors every other package's own repoRoot test
// helper: this file is <root>/eval/gen/gen_test.go.
func repoRootForGenTest(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

// loadShippedConfigForGenTest builds the real config/{rules,vendors,
// local_weights,brands}.yaml this repo ships — used by
// TestGenerate_RecallVariesWithSeed to score against the actual shipped
// weights, not a hand-rolled stand-in.
func loadShippedConfigForGenTest(t *testing.T, root string) (*config.Config, feature.BrandSet) {
	t.Helper()
	weights, err := local.LoadWeightsFile(filepath.Join(root, "config", "local_weights.yaml"))
	if err != nil {
		t.Fatalf("load weights: %v", err)
	}
	scorer, err := local.New(weights)
	if err != nil {
		t.Fatalf("new local scorer: %v", err)
	}
	reg := model.NewRegistry()
	if err := reg.Register(scorer); err != nil {
		t.Fatalf("register local: %v", err)
	}
	vendorsData, err := os.ReadFile(filepath.Join(root, "config", "vendors.yaml"))
	if err != nil {
		t.Fatalf("read vendors.yaml: %v", err)
	}
	vendors, err := config.LoadVendors(vendorsData)
	if err != nil {
		t.Fatalf("load vendors: %v", err)
	}
	rulesData, err := os.ReadFile(filepath.Join(root, "config", "rules.yaml"))
	if err != nil {
		t.Fatalf("read rules.yaml: %v", err)
	}
	cfg, err := config.Load(rulesData, config.Dependencies{
		Registry: reg,
		Features: config.NewFeatureSet(feature.Names...),
		Vendors:  vendors,
	})
	if err != nil {
		t.Fatalf("load rules.yaml: %v", err)
	}
	brands, err := feature.LoadBrandsFile(filepath.Join(root, "config", "brands.yaml"))
	if err != nil {
		t.Fatalf("load brands.yaml: %v", err)
	}
	return cfg, brands
}

func ruleByNameForGenTest(cfg *config.Config, name string) (config.Rule, bool) {
	for _, r := range cfg.Rules {
		if r.Name == name {
			return r, true
		}
	}
	return config.Rule{}, false
}

// TestGenerate_Deterministic proves Generate is a pure function of
// Options.Seed (task brief: "a seeded generator") — two calls with the
// same seed must produce byte-identical (here: DeepEqual, which is
// stronger — no field may differ, not just their JSON encoding) output.
func TestGenerate_Deterministic(t *testing.T) {
	a := Generate(Options{Seed: 20260927})
	b := Generate(Options{Seed: 20260927})
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("Generate(seed=20260927) was not deterministic across two calls")
	}

	c := Generate(Options{Seed: 1})
	if reflect.DeepEqual(a, c) {
		t.Fatalf("Generate with a different seed produced identical output — the seed isn't actually driving anything")
	}
}

// requiredBenignFamilies / requiredAbusiveFamilies mirror the task
// brief's named families exactly, so this test fails loudly if one is
// ever dropped or renamed.
var requiredBenignFamilies = []string{
	"benign_fast_dev_onboarding",
	"benign_integration_heavy",
	"benign_day1_receipts_fanout",
	"benign_support_desk_later_fanout",
	"benign_newsletter_later_fanout",
	"benign_slow_upgrader",
	"benign_trial_zero_dollar",
}

var requiredAbusiveFamilies = []string{
	"abusive_burst",
	"abusive_fast",
	"abusive_churn_email",
	"abusive_churn_card",
	"abusive_churn_device",
	"abusive_dormant_then_blast",
	"abusive_slow_operator",
}

// TestGenerate_FamilyCoverage checks the task brief's floors: >=150
// benign subjects across the realistic families, >=40 abusive subjects
// across burst/fast/churn(x3)/dormant-then-blast/slow-operator, every
// required family present with at least one subject.
func TestGenerate_FamilyCoverage(t *testing.T) {
	res := Generate(Options{Seed: 20260927})

	counts := map[string]int{}
	for _, family := range res.FamilyOf {
		counts[family]++
	}

	var benignTotal, abusiveTotal int
	for family, n := range counts {
		if len(family) >= len("benign_") && family[:len("benign_")] == "benign_" {
			benignTotal += n
		} else {
			abusiveTotal += n
		}
	}

	if benignTotal < 150 {
		t.Errorf("benign total = %d, want >= 150", benignTotal)
	}
	if abusiveTotal < 40 {
		t.Errorf("abusive total = %d, want >= 40", abusiveTotal)
	}
	for _, f := range requiredBenignFamilies {
		if counts[f] == 0 {
			t.Errorf("required benign family %q has zero subjects", f)
		}
	}
	for _, f := range requiredAbusiveFamilies {
		if counts[f] == 0 {
			t.Errorf("required abusive family %q has zero subjects", f)
		}
	}
	if len(res.Labels) != len(res.FamilyOf) {
		t.Errorf("len(Labels) = %d, len(FamilyOf) = %d, want equal (one family entry per labelled subject)", len(res.Labels), len(res.FamilyOf))
	}
}

// TestGenerate_EventsValidateAndRedact proves every generated event
// actually passes internal/event's real Validate/Redact contract — the
// same gate every producer's real POST /v1/events body goes through. A
// generator that silently produced structurally-invalid events (a bad
// link hash length, an over-length name, a non-UTC timestamp) would
// otherwise only be caught later, as a confusing eval.LoadReplayDataset
// RowError with no obvious connection back to this package.
func TestGenerate_EventsValidateAndRedact(t *testing.T) {
	res := Generate(Options{Seed: 20260927})
	for i := range res.Events {
		e := res.Events[i]
		if err := e.Validate(event.ValidateOptions{Now: e.At}); err != nil {
			t.Fatalf("event %s (subject %s) failed Validate: %v", e.ID, e.Subject, err)
		}
		if err := e.Redact(); err != nil {
			t.Fatalf("event %s (subject %s) failed Redact: %v", e.ID, e.Subject, err)
		}
	}
}

// TestGenerate_RoundTripsThroughLoadReplayDataset is the end-to-end
// check: JSON-encode the generated events/labels exactly the way `gen`'s
// main.go writes them to disk, then feed them back through
// eval.LoadReplayDataset — the same function `abusekit eval` and `make
// gate` use — and require zero RowErrors. This is what actually proves
// the committed eval/fixtures/synthetic/*.jsonl files are usable, not
// just that Generate's in-memory Go values look right.
func TestGenerate_RoundTripsThroughLoadReplayDataset(t *testing.T) {
	res := Generate(Options{Seed: 20260927})

	var eventsBuf, labelsBuf bytes.Buffer
	enc := json.NewEncoder(&eventsBuf)
	for _, e := range res.Events {
		if err := enc.Encode(e); err != nil {
			t.Fatalf("encode event %s: %v", e.ID, err)
		}
	}
	lenc := json.NewEncoder(&labelsBuf)
	for _, l := range res.Labels {
		if err := lenc.Encode(l); err != nil {
			t.Fatalf("encode label %s: %v", l.Subject, err)
		}
	}

	dataset, rowErrs, err := eval.LoadReplayDataset(eval.ReplayInput{EventsPath: "events.jsonl", Events: &eventsBuf, LabelsPath: "labels.jsonl", Labels: &labelsBuf}, feature.BrandSet{}, "benign")
	if err != nil {
		t.Fatalf("LoadReplayDataset: %v (row errors: %v)", err, rowErrs)
	}
	if len(rowErrs) != 0 {
		t.Fatalf("LoadReplayDataset returned %d row errors on a generator-produced corpus: %v", len(rowErrs), rowErrs)
	}
	if len(dataset.Subjects) != len(res.Labels) {
		t.Fatalf("dataset has %d subjects, want %d (one per label row)", len(dataset.Subjects), len(res.Labels))
	}
	if !dataset.Replay {
		t.Fatalf("dataset.Replay = false, want true for an event-replay pair")
	}
}

// TestGenerate_RecallVariesWithSeed is fix round S3's own acceptance
// test: "recall must vary with the seed" — proof the corpus's jitter
// actually reaches scoring outcomes, not just cosmetic field values. Two
// different seeds' generated corpora are scored with the real shipped
// config/local_weights.yaml against config/rules.yaml's
// new_account_velocity, and their recall values must differ.
func TestGenerate_RecallVariesWithSeed(t *testing.T) {
	root := repoRootForGenTest(t)
	cfg, brands := loadShippedConfigForGenTest(t, root)
	rule, ok := ruleByNameForGenTest(cfg, "new_account_velocity")
	if !ok {
		t.Fatalf("rule not found")
	}
	scorer, ok := cfg.ScorerFor(rule)
	if !ok {
		t.Fatalf("scorer not found")
	}

	recallFor := func(seed int64) float64 {
		res := Generate(Options{Seed: seed})
		var eventsBuf, labelsBuf bytes.Buffer
		enc := json.NewEncoder(&eventsBuf)
		for _, e := range res.Events {
			if err := enc.Encode(e); err != nil {
				t.Fatalf("encode event: %v", err)
			}
		}
		lenc := json.NewEncoder(&labelsBuf)
		for _, l := range res.Labels {
			if err := lenc.Encode(l); err != nil {
				t.Fatalf("encode label: %v", err)
			}
		}
		dataset, rowErrs, err := eval.LoadReplayDataset(eval.ReplayInput{EventsPath: "events.jsonl", Events: &eventsBuf, LabelsPath: "labels.jsonl", Labels: &labelsBuf}, brands, rule.BenignLabel)
		if err != nil {
			t.Fatalf("LoadReplayDataset(seed=%d): %v (rowErrs=%v)", seed, err, rowErrs)
		}
		run, err := eval.Run(context.Background(), dataset, rule, scorer, eval.Options{Tiers: cfg.Tiers})
		if err != nil {
			t.Fatalf("eval.Run(seed=%d): %v", seed, err)
		}
		return run.Metrics.Threshold.Recall.Value
	}

	r1 := recallFor(1)
	r2 := recallFor(2)
	r3 := recallFor(20260927)
	if r1 == r2 && r2 == r3 {
		t.Fatalf("recall was identical (%v) across three different seeds — the corpus's jitter isn't reaching scoring outcomes", r1)
	}
}
