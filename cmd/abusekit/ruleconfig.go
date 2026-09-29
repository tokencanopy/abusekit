package main

import (
	"fmt"
	"os"

	"github.com/tokencanopy/abusekit/internal/config"
	"github.com/tokencanopy/abusekit/internal/feature"
	"github.com/tokencanopy/abusekit/internal/model"
	"github.com/tokencanopy/abusekit/internal/model/local"
)

// ruleConfigPaths are the four YAML files that determine rule
// configuration (rules, vendor allowlist, local scorer weights, brand
// list) — every field boot's serveConfig already carries, factored out
// so the S4 harness subcommands (eval/score/corpus, see eval_cmd.go/
// score_cmd.go/corpus_cmd.go) can load exactly the same configuration
// `serve` runs on WITHOUT ever touching Postgres or keys.yaml (neither
// of which the harness needs).
type ruleConfigPaths struct {
	rulesPath   string
	vendorsPath string
	weightsPath string
	brandsPath  string
}

// testExtraScorers is declared here (not in a _test.go file) because it
// must exist in every build, production included, for loadRuleConfig to
// reference it — see loadRuleConfig's own doc comment. Its zero value
// (nil) is a no-op; nothing outside this package's tests ever assigns to
// it.
var testExtraScorers []model.Scorer

// loadRuleConfig builds the scorer registry (local, from weightsPath),
// loads and validates rules.yaml against it and vendors.yaml, and loads
// the brand list — the same sequence `boot` runs before it ever connects
// to Postgres (S4 fix round: extracted from boot, behavior unchanged,
// so both `serve` and the harness subcommands share one implementation
// rather than two that could drift).
func loadRuleConfig(p ruleConfigPaths) (*config.Config, *model.Registry, feature.BrandSet, error) {
	weights, err := local.LoadWeightsFile(p.weightsPath)
	if err != nil {
		return nil, nil, feature.BrandSet{}, err
	}
	localScorer, err := local.New(weights)
	if err != nil {
		return nil, nil, feature.BrandSet{}, err
	}
	registry := model.NewRegistry()
	if err := registry.Register(localScorer); err != nil {
		return nil, nil, feature.BrandSet{}, err
	}
	// testExtraScorers is a test-only hook (review round 2, T1: "inject a
	// fake non-local scorer into the registry") — nil in every real
	// invocation; only a _test.go file in this package ever sets it, to
	// exercise the CLI's own non-local-scorer-needs-a-cassette refusal
	// and cassette-miss handling without a real vendor adapter.
	for _, s := range testExtraScorers {
		if err := registry.Register(s); err != nil {
			return nil, nil, feature.BrandSet{}, err
		}
	}

	vendorsData, err := os.ReadFile(p.vendorsPath)
	if err != nil {
		return nil, nil, feature.BrandSet{}, fmt.Errorf("read vendors config: %w", err)
	}
	vendors, err := config.LoadVendors(vendorsData)
	if err != nil {
		return nil, nil, feature.BrandSet{}, err
	}

	rulesData, err := os.ReadFile(p.rulesPath)
	if err != nil {
		return nil, nil, feature.BrandSet{}, fmt.Errorf("read rules config: %w", err)
	}
	cfg, err := config.Load(rulesData, config.Dependencies{
		Registry: registry,
		Features: config.NewFeatureSet(feature.Names...),
		Vendors:  vendors,
	})
	if err != nil {
		return nil, nil, feature.BrandSet{}, fmt.Errorf("load rules config: %w", err)
	}

	brands, err := feature.LoadBrandsFile(p.brandsPath)
	if err != nil {
		return nil, nil, feature.BrandSet{}, fmt.Errorf("load brands config: %w", err)
	}

	return cfg, registry, brands, nil
}
