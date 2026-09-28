package eval

import (
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/tokencanopy/abusekit/internal/config"
)

// eceBinCount is the number of equal-width bins ECE sorts predicted risk
// into (design §4.10: "ECE with 10 bins").
const eceBinCount = 10

// Rate is a binomial proportion K/N with its Wilson 95% confidence
// interval (design §4.10: "Wilson intervals on every rate"). N == 0
// means the rate is undefined for this run (e.g. precision when nothing
// was ever flagged) — Value/WilsonLow/WilsonHigh are all 0 in that case,
// and a reader should treat Defined == false as "no evidence", not "0%".
type Rate struct {
	K          int     `json:"k"`
	N          int     `json:"n"`
	Value      float64 `json:"value"`
	WilsonLow  float64 `json:"wilson_low"`
	WilsonHigh float64 `json:"wilson_high"`
	Defined    bool    `json:"defined"`
}

func newRate(k, n int) Rate {
	if n <= 0 {
		return Rate{}
	}
	lo, hi := wilsonInterval(k, n)
	return Rate{K: k, N: n, Value: float64(k) / float64(n), WilsonLow: lo, WilsonHigh: hi, Defined: true}
}

// PRF is precision/recall/F1 at one operating point (the rule's own
// threshold, or one tier cut). F1 has no Wilson interval of its own — it
// is a derived harmonic mean, not a binomial proportion — so only
// Precision and Recall carry one (design: "Wilson intervals on every
// rate" — every RATE, and F1 isn't one).
type PRF struct {
	Precision Rate    `json:"precision"`
	Recall    Rate    `json:"recall"`
	F1        float64 `json:"f1"`
}

func newPRF(tp, fp, fn int) PRF {
	precision := newRate(tp, tp+fp)
	recall := newRate(tp, tp+fn)
	var f1 float64
	if precision.Value+recall.Value > 0 {
		f1 = 2 * precision.Value * recall.Value / (precision.Value + recall.Value)
	}
	return PRF{Precision: precision, Recall: recall, F1: f1}
}

// ConfusionMatrix is the threshold-based confusion matrix among SCORED
// records only (design: unscored is folded into recall as a miss and
// excluded from precision — see computeMetrics — so it is deliberately
// not a TP/FP/TN/FN cell here; UnscoredPositive/UnscoredNegative on
// Metrics carry that count separately).
type ConfusionMatrix struct {
	TP int `json:"tp"`
	FP int `json:"fp"`
	TN int `json:"tn"`
	FN int `json:"fn"`
}

// ECEBin is one bin of the expected-calibration-error histogram.
type ECEBin struct {
	Lo               float64 `json:"lo"`
	Hi               float64 `json:"hi"`
	Count            int     `json:"count"`
	AvgPredicted     float64 `json:"avg_predicted"`
	FractionPositive float64 `json:"fraction_positive"`
}

// ECE is the expected calibration error over every scored record: the
// weighted average, across eceBinCount equal-width risk bins, of
// |avg predicted risk - empirical fraction positive| in that bin.
type ECE struct {
	Value float64  `json:"value"`
	Bins  []ECEBin `json:"bins"`
	// Defined is false when there were zero scored records to bin (fix
	// round B3: "ECE over an empty set is not 0" — a run that scored
	// nothing has no calibration error to report, and reporting 0 would
	// read as "perfectly calibrated", the opposite of "no evidence").
	// FloorEntry.Check treats an undefined ECE as failing any configured
	// max_ece floor outright, the same fail-closed direction as every
	// other undefined Rate in this package.
	Defined bool `json:"defined"`
}

// AUROC is the area under the ROC curve, computed via the Mann-Whitney U
// rank-sum identity (ties broken by average rank) over every scored
// record. Defined is false when there are zero positives or zero
// negatives among scored records — AUROC has no meaning with only one
// class present.
type AUROC struct {
	Value   float64 `json:"value"`
	Defined bool    `json:"defined"`
}

// LatencyStats are the scorer call latency percentiles (milliseconds),
// measured by eval itself (see scoreOne) so every scorer — self-reporting
// or not — gets a real figure.
type LatencyStats struct {
	P50MS float64 `json:"p50_ms"`
	P95MS float64 `json:"p95_ms"`
}

// leadTimeBucket names the earliest Slice a positive (non-benign) subject
// was first flagged at, or "never".
type leadTimeBucket string

const (
	leadTimeFirstSend leadTimeBucket = "first_send"
	leadTimeEarly15m  leadTimeBucket = "early_15m"
	leadTimeFull      leadTimeBucket = "full"
	leadTimeNever     leadTimeBucket = "never"
)

func bucketForSlice(s Slice) leadTimeBucket {
	switch s {
	case SliceFirstSend:
		return leadTimeFirstSend
	case SliceEarly15m:
		return leadTimeEarly15m
	default:
		return leadTimeFull
	}
}

// LeadTime buckets every positive (ground-truth non-benign) subject in a
// replay-shaped Dataset by the EARLIEST Slice (in first_send < early_15m
// < full order) at which it was flagged — never cumulative: each subject
// contributes to exactly one bucket. Never is the count of positive
// subjects that had at least one available Slice but were flagged at
// none of them. Omitted entirely (nil Metrics.LeadTime) for a
// label-snapshot corpus, which has no earlier-slice comparison to make.
type LeadTime struct {
	FirstSend int `json:"first_send"`
	Early15m  int `json:"early_15m"`
	Full      int `json:"full"`
	Never     int `json:"never"`
}

// Metrics is Run's reduction over every scored subject (design §4.10).
type Metrics struct {
	// Threshold is precision/recall/F1 at the rule's own configured
	// threshold (Verdict.Flagged).
	Threshold       PRF             `json:"threshold"`
	ConfusionMatrix ConfusionMatrix `json:"confusion_matrix"`
	// TierCuts is precision/recall/F1 at each configured global tier cut
	// (risk >= medium, risk >= high) — keyed "medium"/"high". Empty when
	// Options.Tiers wasn't a validly configured Tiers.
	TierCuts map[string]PRF `json:"tier_cuts,omitempty"`
	// FamilyHighTierRecall is recall at the "high" tier cut for specific
	// named families the fix round's gate cares about (S2): "burst",
	// "dormant_then_blast", and the synthetic "churn_incarnation_ge3"
	// (every churn subject at or past its own chain's 3rd incarnation,
	// design §1.2(c)'s criterion — parsed from eval/gen's own
	// acct_gen_churn_<kind>_<chain>_<n> subject-id convention; see
	// churnIncarnationNumber). Omitted when Options.Tiers wasn't validly
	// configured; a family with zero positive subjects in this dataset
	// simply has no entry (never a spurious 0%).
	FamilyHighTierRecall map[string]Rate `json:"family_high_tier_recall,omitempty"`
	ECE                  ECE             `json:"ece"`
	AUROC                AUROC           `json:"auroc"`
	// LeadTime is nil unless the Dataset was replay-shaped (see Dataset.Replay).
	LeadTime       *LeadTime    `json:"lead_time,omitempty"`
	Latency        LatencyStats `json:"latency"`
	CostTotalMicro int64        `json:"cost_total_micro"`
	// UnscoredCount / TruncatedCount are reported separately from the
	// confusion matrix per design: "unscored counts as a miss [for
	// recall] ... report the unscored count separately".
	UnscoredCount int `json:"unscored_count"`
	// MissingSliceCount (fix round T6) is the subset of UnscoredCount
	// whose ErrorCode is specifically "missing_slice" (subj.Points had no
	// entry for the requested slice — e.g. a subject with no qualifying
	// event before decision_at for this slice) — reported separately so
	// "the corpus doesn't cover this slice for N subjects" is visible
	// without having to re-derive it from a full run.json's verdict list.
	MissingSliceCount int `json:"missing_slice_count"`
	TruncatedCount    int `json:"truncated_count"`
	Total             int `json:"total"`
}

func computeMetrics(records []scoredRecord, tiers config.Tiers, replay bool, totalCostMicro int64, latenciesMS []float64) Metrics {
	var (
		tp, fp, tn, fn    int
		unscoredCount     int
		missingSliceCount int
		truncatedCount    int
		aurocPts          []aurocPoint
	)

	tierBuckets := map[string]struct{ tp, fp, fn int }{}
	haveValidTiers := validTiers(tiers)
	if haveValidTiers {
		tierBuckets["medium"] = struct{ tp, fp, fn int }{}
		tierBuckets["high"] = struct{ tp, fp, fn int }{}
	}
	familyBuckets := map[string]struct{ tp, fn int }{}

	addFamilyOutcome := func(r scoredRecord, flaggedAtHigh bool) {
		if !haveValidTiers || !r.positive {
			return
		}
		for _, fam := range familyKeysFor(r) {
			b := familyBuckets[fam]
			if r.unscored || !flaggedAtHigh {
				b.fn++
			} else {
				b.tp++
			}
			familyBuckets[fam] = b
		}
	}

	for _, r := range records {
		if r.truncated {
			truncatedCount++
		}
		if r.unscored {
			unscoredCount++
			if r.errorCode == "missing_slice" {
				missingSliceCount++
			}
			if r.positive {
				fn++ // design: "treat an unscored verdict as a miss for recall"
				if haveValidTiers {
					for cut := range tierBuckets {
						b := tierBuckets[cut]
						b.fn++
						tierBuckets[cut] = b
					}
				}
			}
			addFamilyOutcome(r, false)
			// design: "exclude [an unscored verdict] from precision" — a
			// negative-labelled unscored record contributes to neither
			// TP/FP/TN/FN nor the tier buckets.
			continue
		}

		aurocPts = append(aurocPts, aurocPoint{risk: r.risk, positive: r.positive})

		if r.positive && r.flagged {
			tp++
		} else if r.positive && !r.flagged {
			fn++
		} else if !r.positive && r.flagged {
			fp++
		} else {
			tn++
		}

		if haveValidTiers {
			for cut, cutValue := range map[string]float64{"medium": tiers.Medium, "high": tiers.High} {
				flaggedAtCut := r.risk >= cutValue
				b := tierBuckets[cut]
				switch {
				case r.positive && flaggedAtCut:
					b.tp++
				case r.positive && !flaggedAtCut:
					b.fn++
				case !r.positive && flaggedAtCut:
					b.fp++
				}
				tierBuckets[cut] = b
			}
			addFamilyOutcome(r, r.risk >= tiers.High)
		}
	}

	m := Metrics{
		Threshold:         newPRF(tp, fp, fn),
		ConfusionMatrix:   ConfusionMatrix{TP: tp, FP: fp, TN: tn, FN: fn},
		ECE:               computeECE(aurocPts, eceBinCount),
		AUROC:             computeAUROC(aurocPts),
		Latency:           computeLatency(latenciesMS),
		CostTotalMicro:    totalCostMicro,
		UnscoredCount:     unscoredCount,
		MissingSliceCount: missingSliceCount,
		TruncatedCount:    truncatedCount,
		Total:             len(records),
	}
	if haveValidTiers {
		m.TierCuts = map[string]PRF{
			"medium": newPRF(tierBuckets["medium"].tp, tierBuckets["medium"].fp, tierBuckets["medium"].fn),
			"high":   newPRF(tierBuckets["high"].tp, tierBuckets["high"].fp, tierBuckets["high"].fn),
		}
		if len(familyBuckets) > 0 {
			m.FamilyHighTierRecall = make(map[string]Rate, len(familyBuckets))
			for fam, b := range familyBuckets {
				m.FamilyHighTierRecall[fam] = newRate(b.tp, b.tp+b.fn)
			}
		}
	}
	if replay {
		lt := &LeadTime{}
		for _, r := range records {
			if !r.positive || !r.haveLead {
				continue
			}
			switch r.leadTime {
			case leadTimeFirstSend:
				lt.FirstSend++
			case leadTimeEarly15m:
				lt.Early15m++
			case leadTimeFull:
				lt.Full++
			default:
				lt.Never++
			}
		}
		m.LeadTime = lt
	}
	return m
}

type aurocPoint struct {
	risk     float64
	positive bool
}

// churnSubjectRe matches eval/gen's own churn subject-id convention:
// acct_gen_churn_<kind>_<chain>_<n>, capturing the trailing incarnation
// number n.
var churnSubjectRe = regexp.MustCompile(`^acct_gen_churn_[a-z0-9]+_\d+_(\d+)$`)

// churnIncarnationNumber extracts n from a churn subject id, if it
// matches eval/gen's convention.
func churnIncarnationNumber(subjectID string) (int, bool) {
	m := churnSubjectRe.FindStringSubmatch(subjectID)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

// familyKeysFor returns the Metrics.FamilyHighTierRecall keys r belongs
// to (fix round S2) — 0, 1, or 2 of them (a churn subject contributes to
// both its literal category-derived family, if any is tracked, i.e.
// none today, and possibly "churn_incarnation_ge3").
func familyKeysFor(r scoredRecord) []string {
	var keys []string
	switch r.category {
	case "burst", "dormant_then_blast":
		keys = append(keys, r.category)
	}
	if strings.HasPrefix(r.category, "churn_") {
		if n, ok := churnIncarnationNumber(r.subjectID); ok && n >= 3 {
			keys = append(keys, "churn_incarnation_ge3")
		}
	}
	return keys
}

func computeAUROC(points []aurocPoint) AUROC {
	nPos, nNeg := 0, 0
	for _, p := range points {
		if p.positive {
			nPos++
		} else {
			nNeg++
		}
	}
	if nPos == 0 || nNeg == 0 {
		return AUROC{}
	}
	sorted := append([]aurocPoint(nil), points...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].risk < sorted[j].risk })
	ranks := make([]float64, len(sorted))
	i := 0
	for i < len(sorted) {
		j := i
		for j+1 < len(sorted) && sorted[j+1].risk == sorted[i].risk {
			j++
		}
		avgRank := float64(i+j)/2 + 1 // 1-based average rank across the tie block [i,j]
		for k := i; k <= j; k++ {
			ranks[k] = avgRank
		}
		i = j + 1
	}
	var sumPosRanks float64
	for k, p := range sorted {
		if p.positive {
			sumPosRanks += ranks[k]
		}
	}
	u := sumPosRanks - float64(nPos)*float64(nPos+1)/2
	return AUROC{Value: u / (float64(nPos) * float64(nNeg)), Defined: true}
}

func computeECE(points []aurocPoint, bins int) ECE {
	counts := make([]int, bins)
	sumRisk := make([]float64, bins)
	sumPos := make([]float64, bins)
	for _, p := range points {
		b := eceBinIndex(p.risk, bins)
		counts[b]++
		sumRisk[b] += p.risk
		if p.positive {
			sumPos[b]++
		}
	}
	total := len(points)
	out := ECE{Bins: make([]ECEBin, bins)}
	if total == 0 {
		for b := 0; b < bins; b++ {
			out.Bins[b] = ECEBin{Lo: float64(b) / float64(bins), Hi: float64(b+1) / float64(bins)}
		}
		return out // Defined stays false: fix round B3, "ECE over an empty set is not 0"
	}
	out.Defined = true
	var value float64
	for b := 0; b < bins; b++ {
		bin := ECEBin{Lo: float64(b) / float64(bins), Hi: float64(b+1) / float64(bins), Count: counts[b]}
		if counts[b] > 0 {
			bin.AvgPredicted = sumRisk[b] / float64(counts[b])
			bin.FractionPositive = sumPos[b] / float64(counts[b])
			value += (float64(counts[b]) / float64(total)) * math.Abs(bin.AvgPredicted-bin.FractionPositive)
		}
		out.Bins[b] = bin
	}
	out.Value = value
	return out
}

func eceBinIndex(risk float64, bins int) int {
	idx := int(risk * float64(bins))
	if idx >= bins {
		idx = bins - 1
	}
	if idx < 0 {
		idx = 0
	}
	return idx
}

func computeLatency(latenciesMS []float64) LatencyStats {
	if len(latenciesMS) == 0 {
		return LatencyStats{}
	}
	sorted := append([]float64(nil), latenciesMS...)
	sort.Float64s(sorted)
	return LatencyStats{P50MS: percentile(sorted, 0.50), P95MS: percentile(sorted, 0.95)}
}

// percentile returns the p-th percentile (0<=p<=1) of sorted (already
// ascending) via linear interpolation between the two nearest ranks.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	idx := p * float64(len(sorted)-1)
	lo := int(math.Floor(idx))
	hi := int(math.Ceil(idx))
	if lo == hi {
		return sorted[lo]
	}
	frac := idx - float64(lo)
	return sorted[lo]*(1-frac) + sorted[hi]*frac
}
