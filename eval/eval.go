package eval

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tokencanopy/abusekit/internal/config"
	"github.com/tokencanopy/abusekit/internal/core"
	"github.com/tokencanopy/abusekit/internal/model"
)

// Slice names the decision-point cut a replay-shaped subject's feature
// vector was rebuilt as of (task brief / design §4.10). A label-snapshot
// corpus row (design §4.6) has no slices of its own — LoadSnapshotCorpus
// stores its single point under SliceFull, so a caller that always asks
// for SliceFull (the default) never has to branch on which corpus shape
// it loaded.
type Slice string

const (
	SliceFirstSend Slice = "first_send"
	SliceEarly15m  Slice = "early_15m"
	SliceFull      Slice = "full"
)

// sliceOrder is the chronological order lead-time bucketing walks:
// first_send happens no later than early_15m, which happens no later
// than full (a subject's own decision_at map need not define all three;
// see computeLeadTime).
var sliceOrder = []Slice{SliceFirstSend, SliceEarly15m, SliceFull}

func validSlice(s Slice) bool {
	return s == SliceFirstSend || s == SliceEarly15m || s == SliceFull
}

// ValidSliceFlag reports whether s names one of the three slices — a
// small exported helper so the CLI can validate a `--slice` flag's raw
// string before ever constructing a Slice/Options.
func ValidSliceFlag(s string) bool { return validSlice(Slice(s)) }

// Point is one subject's resolved rule input at one Slice: the feature
// vector internal/feature.Extract computed as of that slice's decision
// point, plus the named text values a rule's `text:` list can select
// from (design's `subject_line_skeleton`/`first_link_host` convention —
// see replay.go's textFieldsOfInterest).
type Point struct {
	Features map[string]float64
	Text     map[string][]string
	// Context carries a label-snapshot corpus row's `input.context`
	// (design §4.6) through to ScoreRequest.Context. An event-replay
	// dataset never sets this — nothing in the replay-pair shape (task
	// brief) has an equivalent field.
	Context string
}

// Subject is one dataset row's resolved input, across every Slice it has
// data for.
type Subject struct {
	// ID is the corpus row's `id` (label-snapshot shape) or the
	// event-replay pair's `subject`.
	ID string
	// Label is the ground-truth label this subject was recorded under —
	// a rule's own label vocabulary value (design §4.6: the corpus row's
	// `label`), NOT necessarily binary. Metrics.go treats
	// Label == rule.BenignLabel as the negative class and anything else
	// as positive, mirroring internal/core.Combine's own
	// "risk = 1 - P(benign_label)" convention.
	Label string
	// Category is an optional finer-grained ground-truth tag (design's
	// replay-labels shape: `category?`, e.g. "phishing"/
	// "brand_impersonation"/"scam"). Metrics never branch on it in S4;
	// it rides along in Verdict/run.json for a human or a later slice to
	// use.
	Category string
	// Source is the label's provenance (design §4.9: "operator"|
	// "outcome", or a label-snapshot corpus row's own `source`).
	Source string
	// Split is "train", "test", or "" (unknown/unset) — design §4.9's
	// 80/20 split. A label-snapshot corpus row carries its own `split`
	// verbatim; a replay-derived subject gets one computed from a keyed
	// hash of its link cluster (fix round S7 — see replay.go's
	// computeSplits). FilterSplit uses this field.
	Split string
	// Meta is the corpus row's free-form `meta` object, or (for a
	// replay-derived subject) a small set of derived facts (decision_at
	// per slice, the events count considered) — carried through to
	// Verdict/run.json for a human reading the run, never consulted by
	// scoring itself.
	Meta map[string]any
	// Points holds one Point per Slice this subject has data for. A
	// label-snapshot corpus row always has exactly {SliceFull: ...}. An
	// event-replay subject has an entry for every slice its labels row's
	// `decision_at` map defined AND that had at least one event strictly
	// before it (see replay.go) — SliceFull is always present (it falls
	// back to "last event + 1ns" when decision_at.full is absent).
	Points map[Slice]Point
}

// Dataset is Run's fully-resolved input: one Subject per corpus row or
// per event-replay label. Replay reports whether every Subject came from
// an event-replay pair (as opposed to a label-snapshot corpus) — Run
// only computes the lead-time metric when this is true, since lead time
// is inherently about comparing a subject's OWN decision points against
// each other, which a label-snapshot corpus (one point per subject, no
// "earlier slice" to compare against) cannot supply.
type Dataset struct {
	Subjects []Subject
	Replay   bool
}

// Options are Run's non-dataset, non-rule, non-scorer inputs.
type Options struct {
	// Slice selects which of each Subject's Points is scored for the
	// headline metrics (threshold/tier precision-recall, ECE, AUROC,
	// confusion matrix, latency, cost). Defaults to SliceFull.
	Slice Slice
	// Tiers are the global score cut points (design §4.4) used to fill in
	// Verdict.Tier and Metrics.TierCuts. The zero value is intentionally
	// usable: every Verdict.Tier comes back "unknown" and Metrics.TierCuts
	// is empty, rather than Run guessing at cut points a caller didn't
	// supply.
	Tiers config.Tiers
	// Calibration, when non-nil, adjusts each scored raw risk (1 -
	// P(benign)) before it is compared against the rule's threshold or
	// tiers — design §4.6's calibration step. nil means "use the
	// scorer's raw probability as-is", correct for the local scorer
	// (Capabilities().Calibrated == true) and for any other
	// already-calibrated scorer.
	Calibration core.Calibrator
	// PromptVersion is recorded as every ScoreRequest.RenderVersion, as
	// part of the cassette key (design §4.10: "(model, checkpoint,
	// render, input_hash)") and on the manifest. Defaults to "v1".
	PromptVersion string
	// Now returns the manifest's `at` timestamp. Defaults to time.Now;
	// tests pass a fixed clock so two runs over identical inputs produce
	// byte-identical run.json apart from this one field.
	Now func() time.Time
	// DatasetSHA / LabelsSHA, when set, are recorded verbatim on the
	// manifest (the CLI computes these from the actual input file bytes
	// it read). Left empty, Run derives a content hash of the resolved
	// Dataset itself (see hash.go's datasetContentSHA) — less faithful to
	// "the exact bytes on disk" (a caller building a Dataset by hand in
	// Go, e.g. a test, has no file at all) but still a stable fingerprint
	// of what was actually scored.
	DatasetSHA string
	LabelsSHA  string
	// SplitFilter is recorded on the manifest verbatim (fix round S5) —
	// purely informational: Run does not itself filter dataset by split.
	// A caller applies FilterSplit(dataset, splitFilter) BEFORE calling
	// Run and passes the same string here so run.json records what was
	// actually scored.
	SplitFilter string
}

// FilterSplit returns the subset of dataset.Subjects whose Split matches
// split ("" or "all" means every subject, unfiltered) — fix round S7's
// `--split train|test|all` flag. A subject with Split == "" (unknown —
// possible if a caller built a Dataset by hand without setting it) never
// matches a non-"all" filter.
func FilterSplit(dataset Dataset, split string) Dataset {
	if split == "" || split == "all" {
		return dataset
	}
	out := Dataset{Replay: dataset.Replay}
	for _, s := range dataset.Subjects {
		if s.Split == split {
			out.Subjects = append(out.Subjects, s)
		}
	}
	return out
}

func (o Options) slice() Slice {
	if o.Slice == "" {
		return SliceFull
	}
	return o.Slice
}

func (o Options) promptVersion() string {
	if o.PromptVersion == "" {
		return "v1"
	}
	return o.PromptVersion
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now().UTC()
}

// Manifest identifies exactly what produced a Run, per the task brief's
// literal shape and design §4.10 ("Manifest: abusekit version and git
// sha, dataset sha, split, rule sha, scorer, model, checkpoint, render
// version, calibration id, ... timestamp"). Everything here is
// deterministic given the same Dataset/Rule/Scorer/Options except `At`,
// which is why determinism_test.go compares two runs with At zeroed out.
type Manifest struct {
	// SchemaVersion identifies run.json's own top-level shape (fix round
	// S5) — bump it whenever a field is renamed/removed/retyped (an
	// addition alone doesn't need a bump, per the repo's own additive-
	// change convention).
	SchemaVersion   string `json:"schema_version"`
	AbusekitVersion string `json:"abusekit_version"`
	// GitSHA is the build's VCS revision (fix round S5), read from
	// runtime/debug's build info — empty when the binary wasn't built
	// from a git checkout (e.g. `go run`, or GOFLAGS=-buildvcs=false).
	GitSHA     string `json:"git_sha,omitempty"`
	DatasetSHA string `json:"dataset_sha"`
	LabelsSHA  string `json:"labels_sha,omitempty"`
	RuleSHA    string `json:"rule_sha"`
	WeightsSHA string `json:"weights_sha"`
	Scorer     string `json:"scorer"`
	Model      string `json:"model"`
	// Checkpoint is the scorer's own ScoreResult.Checkpoint from the
	// first successfully scored subject this run (fix round S5) — empty
	// if nothing was ever scored (e.g. every subject was unscored).
	Checkpoint    string `json:"checkpoint,omitempty"`
	PromptVersion string `json:"prompt_version"`
	// Labels is the rule's own label vocabulary (fix round S5) — what
	// every Verdict.Label and Subject.Label is validated against (see
	// validateSubjectLabels).
	Labels []string `json:"labels"`
	// Threshold is the rule's own configured threshold (fix round S5),
	// duplicated onto the manifest so a reader of run.json alone (no
	// rules.yaml in hand) can tell what "flagged" meant for this run.
	Threshold float64 `json:"threshold"`
	// TierCuts is {"medium":.., "high":..} when Options.Tiers was validly
	// configured (fix round S5); omitted otherwise.
	TierCuts map[string]float64 `json:"tier_cuts,omitempty"`
	Slice    string             `json:"slice"`
	// SplitFilter is Options.SplitFilter verbatim (fix round S5/S7) — ""
	// or "all" means unfiltered.
	SplitFilter string    `json:"split_filter,omitempty"`
	At          time.Time `json:"at"`
	// CalibrationApplied reports whether Options.Calibration was non-nil
	// for this run — design §4.10 calls for a calibration id on every
	// verdict; S4's Options takes a bare core.Calibrator (no id — that
	// belongs to core.CalibrationSet, a store-backed concept the harness
	// doesn't have in scope), so this is the closest honest signal: "was
	// some calibrator in effect", not which one.
	CalibrationApplied bool `json:"calibration_applied"`
}

// Verdict is one subject's scored outcome, matching the task brief's
// literal shape plus a few fields a human reading run.json needs to make
// sense of an unscored row.
type Verdict struct {
	Subject   string  `json:"subject"`
	Label     string  `json:"label"`
	Risk      float64 `json:"risk"`
	Tier      string  `json:"tier"`
	Flagged   bool    `json:"flagged"`
	Unscored  bool    `json:"unscored,omitempty"`
	ErrorCode string  `json:"error_code,omitempty"`
}

// RunResult is Run's full result: run.json's top-level shape.
type RunResult struct {
	Manifest Manifest  `json:"manifest"`
	Verdicts []Verdict `json:"verdicts"`
	Metrics  Metrics   `json:"metrics"`
}

// scoredRecord is one subject's outcome as metrics.go needs it — a
// smaller, metrics-focused view than Verdict (which is run.json's own
// wire shape).
type scoredRecord struct {
	subjectID string
	category  string
	label     string
	positive  bool // label != rule.BenignLabel
	unscored  bool
	risk      float64
	flagged   bool
	truncated bool
	leadTime  leadTimeBucket // only meaningful when dataset.Replay && positive
	haveLead  bool
}

// validateSubjectLabels rejects (fix round S6) any subject whose ground-
// truth Label isn't in rule.Labels — a corpus authored for one rule's
// vocabulary silently scored against a different rule (e.g. a "phishing"/
// "brand_impersonation"/"scam" corpus meant for `lure_similarity` run
// against `new_account_velocity`, whose vocabulary is "benign"/
// "suspicious"/"abusive") previously scored every subject as an
// unrecognized-but-accepted label, silently miscounting positives.
func validateSubjectLabels(subjects []Subject, rule config.Rule) error {
	allowed := make(map[string]bool, len(rule.Labels))
	for _, l := range rule.Labels {
		allowed[l] = true
	}
	var bad []string
	for _, s := range subjects {
		if !allowed[s.Label] {
			bad = append(bad, fmt.Sprintf("%s (label %q)", s.ID, s.Label))
		}
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Strings(bad)
	return fmt.Errorf("eval: %d subject(s) have a label outside rule %q's vocabulary %v: %s", len(bad), rule.Name, rule.Labels, strings.Join(bad, ", "))
}

// Run scores every Subject in dataset at rule using scorer, per Options,
// and reduces the result to metrics (design §4.10). Run is pure: it does
// no I/O beyond calling scorer.Score, which callers control (a
// CassetteScorer for CI, a real adapter for a live/nightly run, the local
// scorer for neither).
//
// Run returns a non-nil error — aborting before any metrics are computed
// — in two cases: a subject's ground-truth label isn't in rule.Labels
// (fix round S6), or a scorer call misses its cassette
// (errors.Is(err, ErrCassetteMiss) — fix round B3: this must never be
// silently folded into an "unscored" verdict, since that's exactly the
// "CI quietly called a vendor, or quietly pretended a subject scored
// low" failure mode a cassette exists to prevent). Every OTHER scorer
// error (a live adapter's genuine timeout/5xx) stays a soft per-subject
// "unscored" verdict, unchanged from before this fix round — Run keeps
// scoring the rest of the dataset rather than aborting over one flaky
// call.
//
// Run never mutates dataset, rule, or opts.
func Run(ctx context.Context, dataset Dataset, rule config.Rule, scorer model.Scorer, opts Options) (RunResult, error) {
	if scorer == nil {
		return RunResult{}, errors.New("eval: scorer must not be nil")
	}
	slice := opts.slice()
	if !validSlice(slice) {
		return RunResult{}, errors.New("eval: invalid slice " + string(slice) + " (want first_send, early_15m, or full)")
	}
	if err := validateSubjectLabels(dataset.Subjects, rule); err != nil {
		return RunResult{}, err
	}

	subjects := make([]Subject, len(dataset.Subjects))
	copy(subjects, dataset.Subjects)
	sort.Slice(subjects, func(i, j int) bool { return subjects[i].ID < subjects[j].ID }) // determinism: fixed iteration/output order regardless of load order

	verdicts := make([]Verdict, 0, len(subjects))
	records := make([]scoredRecord, 0, len(subjects))
	var totalCostMicro int64
	var latenciesMS []float64
	var checkpoint string

	for _, subj := range subjects {
		rec := scoredRecord{subjectID: subj.ID, category: subj.Category, label: subj.Label, positive: subj.Label != rule.BenignLabel}

		pt, ok := subj.Points[slice]
		v := Verdict{Subject: subj.ID, Label: subj.Label, Tier: "unknown"} // nit: an unscored verdict's tier is "unknown", never the Go zero value ""
		if !ok {
			v.Unscored = true
			v.ErrorCode = "missing_slice"
			rec.unscored = true
		} else {
			risk, flagged, unscored, res, errCode, callErr := scoreOne(ctx, rule, scorer, opts, pt)
			switch {
			case callErr != nil && errors.Is(callErr, ErrCassetteMiss):
				return RunResult{}, fmt.Errorf("eval: run aborted scoring subject %s: %w", subj.ID, callErr)
			case callErr != nil:
				v.Unscored = true
				v.ErrorCode = "scorer_error"
				rec.unscored = true
			case unscored:
				v.Unscored = true
				v.ErrorCode = errCode
				rec.unscored = true
			default:
				v.Risk = risk
				v.Flagged = flagged
				v.Tier = tierFor(risk, opts.Tiers)
				rec.risk = risk
				rec.flagged = flagged
				rec.truncated = res.Truncated
				totalCostMicro += res.CostMicro
				latenciesMS = append(latenciesMS, float64(res.LatencyMS))
				if checkpoint == "" {
					checkpoint = res.Checkpoint
				}
			}
		}

		if dataset.Replay && rec.positive {
			lt, haveLead, leadCostMicro, leadLatenciesMS, err := computeLeadTime(ctx, rule, scorer, opts, subj, slice, v)
			if err != nil {
				return RunResult{}, fmt.Errorf("eval: run aborted computing lead time for subject %s: %w", subj.ID, err)
			}
			if haveLead {
				rec.leadTime = lt
				rec.haveLead = true
			}
			totalCostMicro += leadCostMicro
			latenciesMS = append(latenciesMS, leadLatenciesMS...)
		}

		verdicts = append(verdicts, v)
		records = append(records, rec)
	}

	metrics := computeMetrics(records, opts.Tiers, dataset.Replay, totalCostMicro, latenciesMS)

	var tierCuts map[string]float64
	if validTiers(opts.Tiers) {
		tierCuts = map[string]float64{"medium": opts.Tiers.Medium, "high": opts.Tiers.High}
	}

	manifest := Manifest{
		SchemaVersion:      SchemaVersion,
		AbusekitVersion:    AbusekitVersion,
		GitSHA:             gitSHA(),
		DatasetSHA:         datasetSHA(opts.DatasetSHA, dataset),
		LabelsSHA:          opts.LabelsSHA,
		RuleSHA:            ruleSHA(rule),
		WeightsSHA:         scorer.Version(),
		Scorer:             scorer.Name(),
		Model:              scorer.Name(),
		Checkpoint:         checkpoint,
		PromptVersion:      opts.promptVersion(),
		Labels:             rule.Labels,
		Threshold:          rule.Threshold,
		TierCuts:           tierCuts,
		Slice:              string(slice),
		SplitFilter:        opts.SplitFilter,
		At:                 opts.now(),
		CalibrationApplied: opts.Calibration != nil,
	}

	return RunResult{Manifest: manifest, Verdicts: verdicts, Metrics: metrics}, nil
}

// computeLeadTime finds the earliest Slice (in sliceOrder) at which subj
// was flagged, reusing selectedVerdict for slice itself (no duplicate
// scorer call) and scoring subj's other available Points fresh. Returns
// ok=false when subj has no Points at all beyond the selected slice AND
// the selected slice itself was unscored — i.e. truly nothing to bucket
// (a "never" bucket, by contrast, means every available slice WAS
// scored, just never flagged).
//
// A cassette miss during one of these extra scoring calls aborts the
// whole Run, exactly like the main scoring loop (fix round B3) — err is
// non-nil only for that case. Any other scorer error for one slice is
// treated as "couldn't tell, keep checking the other slices" (fix round,
// nit: "computeLeadTime surfaces scorer errors" — it no longer silently
// swallows one without at least trying the remaining slices). costMicro/
// latenciesMS accumulate every extra call actually made, successful or
// not, so Metrics.CostTotalMicro/Latency reflect the true number of
// scorer calls a lead-time computation makes (nit: previously discarded).
func computeLeadTime(ctx context.Context, rule config.Rule, scorer model.Scorer, opts Options, subj Subject, selected Slice, selectedVerdict Verdict) (bucket leadTimeBucket, ok bool, costMicro int64, latenciesMS []float64, err error) {
	haveAny := false
	for _, s := range sliceOrder {
		var flagged bool
		if s == selected {
			if selectedVerdict.Unscored {
				continue
			}
			haveAny = true
			flagged = selectedVerdict.Flagged
		} else {
			pt, ptOK := subj.Points[s]
			if !ptOK {
				continue
			}
			haveAny = true
			risk, fl, unscored, res, _, callErr := scoreOne(ctx, rule, scorer, opts, pt)
			_ = risk
			if callErr != nil {
				if errors.Is(callErr, ErrCassetteMiss) {
					return "", false, costMicro, latenciesMS, callErr
				}
				continue // a non-cassette-miss error for this slice: can't tell, try the others
			}
			costMicro += res.CostMicro
			latenciesMS = append(latenciesMS, float64(res.LatencyMS))
			if unscored {
				continue
			}
			flagged = fl
		}
		if flagged {
			return bucketForSlice(s), true, costMicro, latenciesMS, nil
		}
	}
	if !haveAny {
		return "", false, costMicro, latenciesMS, nil
	}
	return leadTimeNever, true, costMicro, latenciesMS, nil
}
