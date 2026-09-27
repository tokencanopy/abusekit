package worker

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/tokencanopy/abusekit/internal/config"
	"github.com/tokencanopy/abusekit/internal/event"
	"github.com/tokencanopy/abusekit/internal/feature"
	"github.com/tokencanopy/abusekit/internal/model"
	"github.com/tokencanopy/abusekit/internal/model/fake"
	"github.com/tokencanopy/abusekit/internal/model/local"
	"github.com/tokencanopy/abusekit/internal/store"
)

const testTenant = "e2a"
const testProducer = "worker-test"

func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller failed")
	}
	// this file: <root>/internal/worker/testutil_test.go
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

// loadShippedConfig builds the real *config.Config this repo ships
// (config/rules.yaml, config/vendors.yaml, config/local_weights.yaml) —
// the same config production runs, registering only the local scorer
// (S1/S2's only registered adapter).
func loadShippedConfig(t *testing.T) *config.Config {
	t.Helper()
	root := repoRoot(t)

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
	return cfg
}

// loadShippedBrands loads the real config/brands.yaml this repo ships
// (S3 fix round) — used alongside loadShippedConfig by every replay test
// so name_brand_match is exercised against the actual curated list, not
// silently held at 0 by an unset Deps.Brands.
func loadShippedBrands(t *testing.T) feature.BrandSet {
	t.Helper()
	root := repoRoot(t)
	brands, err := feature.LoadBrandsFile(filepath.Join(root, "config", "brands.yaml"))
	if err != nil {
		t.Fatalf("load brands.yaml: %v", err)
	}
	return brands
}

// newFakeRuleConfig builds a *config.Config with exactly one advise rule,
// "fake_rule", scored by scorer (registered as "fake_test_scorer") — used
// by tests that need to force a scorer error or a budget denial, which the
// shipped local scorer (deterministic, always succeeds, never budgeted)
// can't exercise.
func newFakeRuleConfig(t *testing.T, scorer *fake.Scorer) *config.Config {
	t.Helper()
	scorer.NameValue = "fake_test_scorer"
	reg := model.NewRegistry()
	if err := reg.Register(scorer); err != nil {
		t.Fatalf("register fake scorer: %v", err)
	}

	const rulesYAML = `
tiers: {medium: 0.4, high: 0.8}
min_scored_advise: 1
rules:
  - name: fake_rule
    mode: advise
    scorer: fake_test_scorer
    inputs: [subject_age_h]
    labels: [benign, abusive]
    benign_label: benign
    threshold: 0.5
`
	cfg, err := config.Load([]byte(rulesYAML), config.Dependencies{
		Registry: reg,
		Features: config.NewFeatureSet(feature.Names...),
		Vendors: map[string]config.VendorEntry{
			"fake_test_scorer": {Name: "fake_test_scorer", TermsVersion: "n/a", DPARef: "n/a", Policy: scorer.Policy()},
		},
	})
	if err != nil {
		t.Fatalf("load fake rule config: %v", err)
	}
	return cfg
}

// appendEvent validates, redacts and appends one event via the real
// ingest path (mirroring what a producer's POST /v1/events would do),
// failing the test on any error — every worker test builds its fixture
// data this way rather than poking the store directly, so a test failure
// here would also mean the fixture itself is invalid, not just the code
// under test.
func appendEvent(t *testing.T, ctx context.Context, s *store.Store, subject, typ string, at time.Time, links event.Links, data map[string]any) {
	t.Helper()
	e := event.Event{ID: subject + "|" + typ + "|" + at.Format(time.RFC3339Nano), Subject: subject, Type: typ, At: at, Links: links, Data: data}
	if err := e.Validate(event.ValidateOptions{Now: at}); err != nil {
		t.Fatalf("validate fixture event %s: %v", e.ID, err)
	}
	if err := e.Redact(); err != nil {
		t.Fatalf("redact fixture event %s: %v", e.ID, err)
	}
	if _, err := s.AppendEvents(ctx, testTenant, testProducer, []event.Event{e}); err != nil {
		t.Fatalf("append fixture event %s: %v", e.ID, err)
	}
}
