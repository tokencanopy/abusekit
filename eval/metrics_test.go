package eval

import (
	"math"
	"testing"

	"github.com/tokencanopy/abusekit/internal/config"
)

const floatTol = 1e-9

func almostEqual(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

// TestComputeECE_HandComputed checks computeECE against a hand-worked
// 4-point, 2-bin example:
//
//	bin [0, 0.5): risk 0.2 (negative), risk 0.3 (positive)
//	  avg predicted = 0.25, fraction positive = 0.5, |diff| = 0.25
//	bin [0.5, 1.0]: risk 0.7 (positive), risk 0.9 (negative)
//	  avg predicted = 0.8, fraction positive = 0.5, |diff| = 0.3
//	ECE = (2/4)*0.25 + (2/4)*0.3 = 0.275
func TestComputeECE_HandComputed(t *testing.T) {
	points := []aurocPoint{
		{risk: 0.2, positive: false},
		{risk: 0.3, positive: true},
		{risk: 0.7, positive: true},
		{risk: 0.9, positive: false},
	}
	got := computeECE(points, 2)
	if !almostEqual(got.Value, 0.275, floatTol) {
		t.Fatalf("ECE.Value = %v, want 0.275", got.Value)
	}
	if len(got.Bins) != 2 {
		t.Fatalf("len(Bins) = %d, want 2", len(got.Bins))
	}
	bin0, bin1 := got.Bins[0], got.Bins[1]
	if bin0.Count != 2 || !almostEqual(bin0.AvgPredicted, 0.25, floatTol) || !almostEqual(bin0.FractionPositive, 0.5, floatTol) {
		t.Errorf("bin0 = %+v, want count=2 avg=0.25 fracPos=0.5", bin0)
	}
	if bin1.Count != 2 || !almostEqual(bin1.AvgPredicted, 0.8, floatTol) || !almostEqual(bin1.FractionPositive, 0.5, floatTol) {
		t.Errorf("bin1 = %+v, want count=2 avg=0.8 fracPos=0.5", bin1)
	}
}

func TestComputeECE_Empty(t *testing.T) {
	got := computeECE(nil, 10)
	if got.Value != 0 {
		t.Errorf("ECE.Value = %v, want 0 for zero points", got.Value)
	}
	if len(got.Bins) != 10 {
		t.Errorf("len(Bins) = %d, want 10", len(got.Bins))
	}
}

// TestComputeAUROC_HandComputed checks computeAUROC against three
// hand-worked cases via the pairwise "fraction of (positive, negative)
// pairs where positive > negative, ties counting 0.5" definition of
// AUROC, which is equivalent to (and independently verifies) the
// rank-sum formula the implementation actually uses.
func TestComputeAUROC_HandComputed(t *testing.T) {
	t.Run("perfect separation", func(t *testing.T) {
		points := []aurocPoint{
			{risk: 0.1, positive: false}, {risk: 0.2, positive: false},
			{risk: 0.8, positive: true}, {risk: 0.9, positive: true},
		}
		got := computeAUROC(points)
		if !got.Defined || !almostEqual(got.Value, 1.0, floatTol) {
			t.Fatalf("AUROC = %+v, want defined=true value=1.0", got)
		}
	})
	t.Run("single tie", func(t *testing.T) {
		// One positive, one negative, identical risk: the pair is a tie,
		// which counts as 0.5 — the "no discrimination" value for a
		// single pair.
		points := []aurocPoint{{risk: 0.5, positive: false}, {risk: 0.5, positive: true}}
		got := computeAUROC(points)
		if !got.Defined || !almostEqual(got.Value, 0.5, floatTol) {
			t.Fatalf("AUROC = %+v, want defined=true value=0.5", got)
		}
	})
	t.Run("partial separation", func(t *testing.T) {
		// negatives = [0.1, 0.6], positives = [0.4, 0.9]. Pairs:
		// (0.4,0.1)=1 (0.4,0.6)=0 (0.9,0.1)=1 (0.9,0.6)=1 -> 3/4 = 0.75.
		points := []aurocPoint{
			{risk: 0.1, positive: false}, {risk: 0.6, positive: false},
			{risk: 0.4, positive: true}, {risk: 0.9, positive: true},
		}
		got := computeAUROC(points)
		if !got.Defined || !almostEqual(got.Value, 0.75, floatTol) {
			t.Fatalf("AUROC = %+v, want defined=true value=0.75", got)
		}
	})
	t.Run("undefined with one class", func(t *testing.T) {
		got := computeAUROC([]aurocPoint{{risk: 0.5, positive: true}, {risk: 0.9, positive: true}})
		if got.Defined {
			t.Fatalf("AUROC.Defined = true with zero negatives, want false")
		}
	})
}

// TestComputeMetrics_HandComputedConfusionMatrix builds a small,
// hand-countable set of scoredRecords and checks the resulting
// confusion matrix and threshold PRF exactly:
//
//	4 true positives (positive, flagged)
//	1 false negative (positive, not flagged)
//	1 unscored positive (counts as a miss for recall — design)
//	2 false positives (negative, flagged)
//	3 true negatives (negative, not flagged)
//	1 unscored negative (excluded from precision entirely — design)
//
// precision = TP/(TP+FP) = 4/6 = 0.6667
// recall    = TP/(TP+FN) = 4/(1+1+4) = 4/6 = 0.6667  (unscored positive counts as FN)
func TestComputeMetrics_HandComputedConfusionMatrix(t *testing.T) {
	records := []scoredRecord{
		{positive: true, flagged: true, risk: 0.9},
		{positive: true, flagged: true, risk: 0.85},
		{positive: true, flagged: true, risk: 0.81},
		{positive: true, flagged: true, risk: 0.95},
		{positive: true, flagged: false, risk: 0.1},
		{positive: true, unscored: true},
		{positive: false, flagged: true, risk: 0.7},
		{positive: false, flagged: true, risk: 0.65},
		{positive: false, flagged: false, risk: 0.2},
		{positive: false, flagged: false, risk: 0.1},
		{positive: false, flagged: false, risk: 0.05},
		{positive: false, unscored: true},
	}

	m := computeMetrics(records, config.Tiers{}, false, 0, nil)

	if m.ConfusionMatrix != (ConfusionMatrix{TP: 4, FP: 2, TN: 3, FN: 2}) {
		t.Fatalf("ConfusionMatrix = %+v, want {TP:4 FP:2 TN:3 FN:2} (FN = the flagged-false positive PLUS the unscored positive)", m.ConfusionMatrix)
	}
	if m.UnscoredCount != 2 {
		t.Fatalf("UnscoredCount = %d, want 2", m.UnscoredCount)
	}
	wantPrecision := 4.0 / 6.0
	wantRecall := 4.0 / 6.0
	if !almostEqual(m.Threshold.Precision.Value, wantPrecision, floatTol) {
		t.Errorf("precision = %v, want %v", m.Threshold.Precision.Value, wantPrecision)
	}
	if m.Threshold.Precision.N != 6 {
		t.Errorf("precision.N = %d, want 6 (TP+FP, unscored negative excluded)", m.Threshold.Precision.N)
	}
	if !almostEqual(m.Threshold.Recall.Value, wantRecall, floatTol) {
		t.Errorf("recall = %v, want %v", m.Threshold.Recall.Value, wantRecall)
	}
	if m.Threshold.Recall.N != 6 {
		t.Errorf("recall.N = %d, want 6 (TP+FN, unscored positive counted as a miss)", m.Threshold.Recall.N)
	}
	wantF1 := 2 * wantPrecision * wantRecall / (wantPrecision + wantRecall)
	if !almostEqual(m.Threshold.F1, wantF1, floatTol) {
		t.Errorf("f1 = %v, want %v", m.Threshold.F1, wantF1)
	}
	if m.Total != len(records) {
		t.Errorf("Total = %d, want %d", m.Total, len(records))
	}
}

// TestComputeMetrics_TierCuts checks the tier-cut PRF path (a separate
// confusion matrix at each of medium/high, independent of the rule's own
// threshold used for the main ConfusionMatrix).
func TestComputeMetrics_TierCuts(t *testing.T) {
	records := []scoredRecord{
		{positive: true, flagged: true, risk: 0.9},   // >= high
		{positive: true, flagged: true, risk: 0.5},   // medium only
		{positive: false, flagged: true, risk: 0.85}, // >= high: a false "high"
		{positive: false, flagged: false, risk: 0.1},
	}
	tiers := config.Tiers{Medium: 0.4, High: 0.8}
	m := computeMetrics(records, tiers, false, 0, nil)

	if m.TierCuts == nil {
		t.Fatalf("TierCuts is nil, want populated for a valid Tiers")
	}
	high := m.TierCuts["high"]
	// high cut: positives >= 0.8 -> only the 0.9 one (TP=1, FN=1); negatives >= 0.8 -> the 0.85 one (FP=1).
	if high.Precision.K != 1 || high.Precision.N != 2 {
		t.Errorf("high precision = %+v, want k=1 n=2", high.Precision)
	}
	if high.Recall.K != 1 || high.Recall.N != 2 {
		t.Errorf("high recall = %+v, want k=1 n=2", high.Recall)
	}
	medium := m.TierCuts["medium"]
	// medium cut: BOTH positives clear 0.4 (TP=2, FN=0); the 0.85 negative also clears it (FP=1).
	if medium.Recall.K != 2 || medium.Recall.N != 2 {
		t.Errorf("medium recall = %+v, want k=2 n=2", medium.Recall)
	}
}

func TestComputeMetrics_InvalidTiersOmitsTierCuts(t *testing.T) {
	m := computeMetrics([]scoredRecord{{positive: true, flagged: true, risk: 0.9}}, config.Tiers{}, false, 0, nil)
	if m.TierCuts != nil {
		t.Fatalf("TierCuts = %+v, want nil for an invalid/zero Tiers", m.TierCuts)
	}
}

func TestPercentile(t *testing.T) {
	sorted := []float64{10, 20, 30, 40, 50}
	if got := percentile(sorted, 0.5); got != 30 {
		t.Errorf("p50 = %v, want 30", got)
	}
	if got := percentile(sorted, 0); got != 10 {
		t.Errorf("p0 = %v, want 10", got)
	}
	if got := percentile(sorted, 1); got != 50 {
		t.Errorf("p100 = %v, want 50", got)
	}
}
