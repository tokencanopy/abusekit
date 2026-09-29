package eval

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/tokencanopy/abusekit/internal/config"
	"github.com/tokencanopy/abusekit/internal/feature"
	"github.com/tokencanopy/abusekit/internal/model"
	"github.com/tokencanopy/abusekit/internal/model/local"
)

// repoRoot mirrors internal/worker/testutil_test.go's own helper: this
// file is <root>/eval/testutil_test.go.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..")
}

// loadShippedRuleConfig loads the real config/{rules,vendors,brands,
// webmail}.yaml and config/local_weights.yaml this repo ships, matching
// cmd/abusekit's own loadRuleConfig — used by determinism_test.go and
// floors_test.go's gate test so both exercise the SAME configuration
// `make gate` runs, not a hand-rolled stand-in. Brands additionally
// merges in eval/fixtures/test_brands.yaml's fictional entries, exactly
// the way Makefile's gate target's own --brands-extra flag does, so
// genSubjectLure's fictional-brand lure scores the same here as it does
// under `make gate` itself.
func loadShippedRuleConfig(t *testing.T) (*config.Config, feature.BrandSet, feature.WebmailSet) {
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

	brands, err := feature.LoadBrandsFile(filepath.Join(root, "config", "brands.yaml"))
	if err != nil {
		t.Fatalf("load brands.yaml: %v", err)
	}
	extra, err := feature.LoadBrandsFile(filepath.Join(root, "eval", "fixtures", "test_brands.yaml"))
	if err != nil {
		t.Fatalf("load eval/fixtures/test_brands.yaml: %v", err)
	}
	brands = feature.MergeBrandSets(brands, extra)

	webmail, err := feature.LoadWebmailFile(filepath.Join(root, "config", "webmail.yaml"))
	if err != nil {
		t.Fatalf("load webmail.yaml: %v", err)
	}
	return cfg, brands, webmail
}

// loadShippedLocalWeights loads config/local_weights.yaml's raw Weights
// value (not yet turned into a Scorer) — floors_test.go's mutation test
// clones and mutates a single weight from this before building a second,
// otherwise-identical local.Scorer.
func loadShippedLocalWeights(t *testing.T) local.Weights {
	t.Helper()
	w, err := local.LoadWeightsFile(filepath.Join(repoRoot(t), "config", "local_weights.yaml"))
	if err != nil {
		t.Fatalf("load weights: %v", err)
	}
	return w
}

func ruleByNameT(t *testing.T, cfg *config.Config, name string) config.Rule {
	t.Helper()
	for _, r := range cfg.Rules {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("rule %q not found", name)
	return config.Rule{}
}

// loadSyntheticDataset loads the committed synthetic corpus
// (eval/fixtures/synthetic/{events,labels}.jsonl) — the same dataset
// `make gate` runs.
func loadSyntheticDataset(t *testing.T, brands feature.BrandSet, webmail feature.WebmailSet) Dataset {
	t.Helper()
	root := repoRoot(t)
	eventsPath := filepath.Join(root, "eval", "fixtures", "synthetic", "events.jsonl")
	labelsPath := filepath.Join(root, "eval", "fixtures", "synthetic", "labels.jsonl")
	eventsF, err := os.Open(eventsPath)
	if err != nil {
		t.Fatalf("open synthetic events: %v", err)
	}
	defer eventsF.Close()
	labelsF, err := os.Open(labelsPath)
	if err != nil {
		t.Fatalf("open synthetic labels: %v", err)
	}
	defer labelsF.Close()

	// "benign" matches config/rules.yaml's new_account_velocity.benign_label
	// — every test using this helper scores that rule.
	ds, rowErrs, err := LoadReplayDataset(ReplayInput{EventsPath: eventsPath, Events: eventsF, LabelsPath: labelsPath, Labels: labelsF}, brands, webmail, "benign")
	if err != nil {
		t.Fatalf("LoadReplayDataset(synthetic corpus): %v (rowErrs=%v)", err, rowErrs)
	}
	return ds
}
