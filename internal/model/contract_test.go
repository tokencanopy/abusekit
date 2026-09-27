package model_test

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/tokencanopy/abusekit/internal/model"
	"github.com/tokencanopy/abusekit/internal/model/fake"
	"github.com/tokencanopy/abusekit/internal/model/local"
)

// ContractConfig parameterizes RunContractSuite (design §4.6: "Contract
// test: every adapter, every CI run, against fakes... Asserts: valid
// probabilities summing to 1±0.01; determinism or reported variance;
// unknown label rejected; timeout -> error; Truncated on overflow;
// Capabilities honoured").
//
// A single generic suite (rather than one hand-written test per adapter)
// is what makes "every adapter passes internal/model/contract_test.go"
// (AGENTS.md) a checkable claim: adding an adapter means adding one
// RunContractSuite call, not re-deriving these assertions.
type ContractConfig struct {
	// New builds a fresh Scorer for each subtest.
	New func() model.Scorer
	// ValidRequest is a request the scorer is expected to answer
	// successfully — its Labels must satisfy the scorer's
	// Capabilities().LabelMode, and its Features/Text should be populated
	// only for the inputs the scorer actually accepts (the suite itself
	// separately verifies rejection of an unaccepted kind).
	ValidRequest model.ScoreRequest
	// RejectedLabelSets are label sets that must cause Score to return an
	// error for this scorer — e.g. a set missing a required benign label,
	// or containing a label outside a Fixed mode's set.
	RejectedLabelSets [][]string
	// OverflowText, if non-empty, is text expected to trigger
	// ScoreResult.Truncated when sent to a scorer whose
	// Capabilities().AcceptsText is true and MaxTokens > 0. Skipped
	// otherwise.
	OverflowText []string
}

// RunContractSuite runs the shared adapter contract assertions against
// cfg.New(). Call it once per adapter (local, vote, fake) from a
// TestXxxContract wrapper so `go test -run TestLocalContract` etc. still
// works.
func RunContractSuite(t *testing.T, cfg ContractConfig) {
	t.Helper()

	t.Run("valid_probabilities_sum_to_one", func(t *testing.T) {
		// A ContractConfig with a duplicate label in ValidRequest.Labels
		// would double-count that label's probability below, spuriously
		// passing (or failing) an otherwise well-behaved adapter — fail
		// loudly on the suite's own configuration rather than silently
		// producing a meaningless sum.
		seen := make(map[string]bool, len(cfg.ValidRequest.Labels))
		for _, label := range cfg.ValidRequest.Labels {
			if seen[label] {
				t.Fatalf("ValidRequest.Labels has duplicate label %q; the contract suite requires a label set with no duplicates", label)
			}
			seen[label] = true
		}

		s := cfg.New()
		res, err := s.Score(context.Background(), cfg.ValidRequest)
		if err != nil {
			t.Fatalf("Score: %v", err)
		}
		for _, label := range cfg.ValidRequest.Labels {
			if _, ok := res.Probs[label]; !ok {
				t.Fatalf("Probs missing requested label %q: %#v", label, res.Probs)
			}
		}
		if reason, ok := invalidProbsReason(res.Probs, cfg.ValidRequest.Labels); !ok {
			t.Fatalf("invalid Probs %#v for labels %v: %s", res.Probs, cfg.ValidRequest.Labels, reason)
		}
	})

	t.Run("determinism", func(t *testing.T) {
		s := cfg.New()
		res1, err := s.Score(context.Background(), cfg.ValidRequest)
		if err != nil {
			t.Fatalf("Score (1st): %v", err)
		}
		res2, err := s.Score(context.Background(), cfg.ValidRequest)
		if err != nil {
			t.Fatalf("Score (2nd): %v", err)
		}
		for _, label := range cfg.ValidRequest.Labels {
			if math.Abs(res1.Probs[label]-res2.Probs[label]) > 1e-9 {
				t.Fatalf("non-deterministic: Probs[%q] = %v then %v", label, res1.Probs[label], res2.Probs[label])
			}
		}
	})

	t.Run("unknown_label_rejected", func(t *testing.T) {
		if len(cfg.RejectedLabelSets) == 0 {
			t.Skip("no RejectedLabelSets configured for this adapter")
		}
		for _, labels := range cfg.RejectedLabelSets {
			req := cfg.ValidRequest
			req.Labels = labels
			s := cfg.New()
			if _, err := s.Score(context.Background(), req); err == nil {
				t.Errorf("Score with label set %v: expected error, got nil", labels)
			}
		}
	})

	t.Run("cancelled_context_is_an_error", func(t *testing.T) {
		s := cfg.New()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := s.Score(ctx, cfg.ValidRequest); err == nil {
			t.Fatalf("Score with an already-cancelled context: expected error, got nil")
		}
	})

	t.Run("expired_deadline_is_an_error", func(t *testing.T) {
		s := cfg.New()
		ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
		defer cancel()
		time.Sleep(time.Millisecond) // guarantee the deadline has passed
		if _, err := s.Score(ctx, cfg.ValidRequest); err == nil {
			t.Fatalf("Score past its deadline: expected error, got nil")
		}
	})

	t.Run("capabilities_honoured", func(t *testing.T) {
		s := cfg.New()
		caps := s.Capabilities()

		if !caps.AcceptsText {
			req := cfg.ValidRequest
			req.Text = []string{"some text"}
			if _, err := s.Score(context.Background(), req); err == nil {
				t.Errorf("scorer with AcceptsText=false accepted Text without error")
			}
		}
		if !caps.AcceptsFeatures {
			req := cfg.ValidRequest
			req.Features = map[string]float64{"x": 1}
			if _, err := s.Score(context.Background(), req); err == nil {
				t.Errorf("scorer with AcceptsFeatures=false accepted Features without error")
			}
		}
	})

	t.Run("truncated_on_overflow", func(t *testing.T) {
		s := cfg.New()
		caps := s.Capabilities()
		if !caps.AcceptsText || caps.MaxTokens <= 0 || len(cfg.OverflowText) == 0 {
			t.Skip("adapter does not declare a bounded text capability, or no OverflowText configured")
		}
		req := cfg.ValidRequest
		req.Text = cfg.OverflowText
		req.Features = nil
		res, err := s.Score(context.Background(), req)
		if err != nil {
			t.Fatalf("Score with overflow text: %v", err)
		}
		if !res.Truncated {
			t.Errorf("expected Truncated=true for text over MaxTokens=%d", caps.MaxTokens)
		}
	})
}

func TestLocalContract(t *testing.T) {
	newLocal := func() model.Scorer {
		w := local.Weights{
			Version:     "test",
			BenignLabel: "benign",
			Bias:        -2,
			Weight: map[string]float64{
				"resource_velocity_1h": 0.8,
				"name_brand_match":     1.2,
			},
		}
		s, err := local.New(w)
		if err != nil {
			t.Fatalf("local.New: %v", err)
		}
		return s
	}
	RunContractSuite(t, ContractConfig{
		New: newLocal,
		ValidRequest: model.ScoreRequest{
			Labels:   []string{"benign", "suspicious", "abusive"},
			Features: map[string]float64{"resource_velocity_1h": 3, "name_brand_match": 1},
		},
		RejectedLabelSets: [][]string{
			{"suspicious", "abusive"}, // missing the required benign label
			{},
		},
	})
}

func TestFakeContract_OpenUnbounded(t *testing.T) {
	RunContractSuite(t, ContractConfig{
		New: func() model.Scorer { return fake.New() },
		ValidRequest: model.ScoreRequest{
			Labels:   []string{"benign", "abusive"},
			Features: map[string]float64{"x": 1},
			Text:     []string{"hello"},
		},
		RejectedLabelSets: [][]string{{}},
	})
}

func TestFakeContract_FixedLabelsAndTruncation(t *testing.T) {
	newFixed := func() model.Scorer {
		s := fake.New()
		s.Caps.LabelMode = model.FixedLabelMode("benign", "phishing")
		s.Caps.MaxTokens = 16
		s.Caps.AcceptsFeatures = false
		return s
	}
	RunContractSuite(t, ContractConfig{
		New: newFixed,
		ValidRequest: model.ScoreRequest{
			Labels: []string{"benign", "phishing"},
			Text:   []string{"short"},
		},
		RejectedLabelSets: [][]string{
			{"benign", "some_other_label"},
			{"benign", "phishing", "extra"},
		},
		OverflowText: []string{"this text is deliberately much longer than sixteen bytes"},
	})
}

func TestFake_TimeoutViaDelay(t *testing.T) {
	s := fake.New()
	s.Delay = 50 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	_, err := s.Score(ctx, model.ScoreRequest{Labels: []string{"benign", "abusive"}})
	if err == nil {
		t.Fatalf("expected a timeout error from a slow Score racing a short deadline")
	}
}

// invalidProbsReason is the exact predicate the "valid_probabilities_sum_to_one"
// subtest above checks; it is factored out to a plain function (returning
// a reason instead of calling t.Fatalf) so a dedicated test can assert
// directly that a malformed Probs map is rejected. This is deliberate,
// not just a style choice: Go's testing package always propagates a
// failing t.Run subtest's failure up through every ancestor Test,
// regardless of what the calling code does with t.Run's returned bool —
// there is no supported way to run RunContractSuite against a
// known-broken adapter inside another test and observe "it correctly
// failed" without that also failing the outer test. Testing the predicate
// directly is what actually proves the suite's math (not just its
// plumbing) rejects an invalid result.
func invalidProbsReason(probs map[string]float64, labels []string) (reason string, ok bool) {
	var sum float64
	for _, label := range labels {
		p, present := probs[label]
		if !present {
			return fmt.Sprintf("missing label %q", label), false
		}
		if math.IsNaN(p) || math.IsInf(p, 0) {
			return fmt.Sprintf("Probs[%q] = %v is not finite", label, p), false
		}
		if p < 0 || p > 1 {
			return fmt.Sprintf("Probs[%q] = %v is outside [0,1]", label, p), false
		}
		sum += p
	}
	if math.IsNaN(sum) || math.IsInf(sum, 0) {
		return fmt.Sprintf("sum = %v is not finite", sum), false
	}
	if math.Abs(sum-1) > 0.01 {
		return fmt.Sprintf("sum = %v, want 1±0.01", sum), false
	}
	return "", true
}

// TestContractSuite_AllNaNAdapterFailsValidation proves the contract
// suite's own probability check (B2) rejects an all-NaN result — the
// proven failure mode: before adding math.IsNaN/IsInf checks, comparing a
// NaN sum against 1±0.01 (and NaN < 0 / NaN > 1) is always false, so an
// all-NaN adapter would silently pass "valid_probabilities_sum_to_one".
func TestContractSuite_AllNaNAdapterFailsValidation(t *testing.T) {
	labels := []string{"benign", "abusive"}
	probs := map[string]float64{"benign": math.NaN(), "abusive": math.NaN()}
	if _, ok := invalidProbsReason(probs, labels); ok {
		t.Fatalf("expected an all-NaN Probs map to fail validation")
	}
}

func TestRegistry(t *testing.T) {
	r := model.NewRegistry()
	s := fake.New()
	if err := r.Register(s); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := r.Register(s); err == nil {
		t.Fatalf("expected duplicate Register to error")
	}
	got, ok := r.Get("fake")
	if !ok || got.Name() != "fake" {
		t.Fatalf("Get(%q) = %v, %v", "fake", got, ok)
	}
	if _, ok := r.Get("does-not-exist"); ok {
		t.Fatalf("Get of unregistered name returned ok=true")
	}
	if err := r.Register(nil); err == nil {
		t.Fatalf("expected Register(nil) to error")
	}
}

func TestLabelMode(t *testing.T) {
	open := model.OpenLabelMode()
	if !open.Accepts([]string{"a"}) || !open.Accepts([]string{"a", "b"}) {
		t.Errorf("open label mode should accept any non-empty set")
	}
	if open.Accepts(nil) {
		t.Errorf("open label mode should reject an empty set")
	}

	fixed := model.FixedLabelMode("benign", "abusive")
	if !fixed.Accepts([]string{"abusive", "benign"}) {
		t.Errorf("fixed label mode should accept its set regardless of order")
	}
	if fixed.Accepts([]string{"benign"}) {
		t.Errorf("fixed label mode should reject a subset")
	}
	if fixed.Accepts([]string{"benign", "abusive", "extra"}) {
		t.Errorf("fixed label mode should reject a superset")
	}

	if !fixed.Equal(model.FixedLabelMode("abusive", "benign")) {
		t.Errorf("Equal should be order-independent")
	}
	if fixed.Equal(open) {
		t.Errorf("fixed and open should not be Equal")
	}
}
