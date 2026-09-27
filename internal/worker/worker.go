// Package worker is abusekit's scoring loop (design §4.8): it finds
// subjects needing (re)scoring, extracts their features (internal/feature),
// drives internal/core's pure Plan/Combine over them with the registered
// scorers, and records the result (internal/store.UpsertVerdicts) —
// including the compare-and-clear, rule-level backoff, and per-adapter/
// per-subject/per-tenant budget enforcement design §4.8 calls for.
//
// The exported surface is deliberately small: Start/Stop run the loop as a
// background goroutine, and Tick does exactly one pass synchronously — the
// same code path Start's ticker drives, but usable directly from a test
// without a goroutine, a real ticker, or a sleep.
package worker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/tokencanopy/abusekit/internal/config"
	"github.com/tokencanopy/abusekit/internal/core"
	"github.com/tokencanopy/abusekit/internal/event"
	"github.com/tokencanopy/abusekit/internal/feature"
	"github.com/tokencanopy/abusekit/internal/model"
	"github.com/tokencanopy/abusekit/internal/store"
)

// DefaultBatchSize is design §4.8's "batch 200 per 10s tick".
const DefaultBatchSize = store.DefaultClaimBatchSize

// DefaultInterval is design §4.8's "batch 200 per 10s tick".
const DefaultInterval = 10 * time.Second

// backoffSchedule and backoffCap are design §4.8's "backoff 30s, 2m, 5m,
// capped at 15m".
var backoffSchedule = []time.Duration{30 * time.Second, 2 * time.Minute, 5 * time.Minute}

const backoffCap = 15 * time.Minute

// backoffDuration returns how long to wait before retrying a rule whose
// scorer has now failed attempts consecutive times (attempts >= 1).
func backoffDuration(attempts int) time.Duration {
	if attempts <= 0 {
		attempts = 1
	}
	if i := attempts - 1; i < len(backoffSchedule) {
		return backoffSchedule[i]
	}
	return backoffCap
}

// Store is the narrow slice of *store.Store the worker depends on —
// codebase-design's "small interfaces": exactly the seven operations one
// scoring pass needs, nothing else. *store.Store satisfies this
// automatically; a test can supply a smaller fake without pulling in a
// database if it only needs to exercise Plan/Combine wiring (the S2 test
// suite doesn't need to — every worker test runs against a real, throwaway
// Postgres, since dirty_seq/scored_seq/FOR UPDATE SKIP LOCKED semantics are
// exactly what's under test).
type Store interface {
	EventsForSubject(ctx context.Context, tenant, subject string) ([]store.StoredEvent, error)
	ClaimDirtySubjects(ctx context.Context, now time.Time, limit int) ([]store.DirtySubject, error)
	LatestVerdicts(ctx context.Context, tenant, subject string) (map[string]store.LatestVerdict, error)
	UpsertVerdicts(ctx context.Context, tenant, subject string, dirtySeqAtStart int64, records []store.VerdictRecord, summary store.SubjectSummary) ([]int64, error)
	GetRuleBackoff(ctx context.Context, tenant, subject, rule string) (store.RuleBackoff, error)
	RecordRuleError(ctx context.Context, tenant, subject, rule string, retryAt time.Time, lastError string) error
	ClearRuleBackoff(ctx context.Context, tenant, subject, rule string) error
}

// Deps are a Worker's dependencies. Store, Config and Neighbors are
// required; everything else has a documented default.
type Deps struct {
	Store     Store
	Config    *config.Config
	Neighbors feature.Neighbors // feature.NewStoreNeighbors(realStore, cfg) in production; feature.NoNeighbors is a valid choice too.

	// Calibration is consulted by internal/core.Combine the same way it
	// would be by the harness (S4). v0 has no vendor scorer needing a
	// calibration map (local reports Capabilities.Calibrated: true), so
	// nil (no entries) is the correct default and every rule.
	Calibration core.CalibrationSet

	// Budgets enforces design §4.8's per-adapter/per-subject/per-tenant
	// daily caps for every rule EXCEPT one scored by "local" (always
	// allowed, regardless of budget — design: "the local rule is
	// unaffected"). nil disables budget enforcement entirely (every call
	// allowed) — the correct default while v0 has no paid adapter to
	// protect.
	Budgets *Budgets

	// BatchSize is how many dirty subjects Tick claims per pass. <= 0
	// uses DefaultBatchSize.
	BatchSize int
	// Interval is how often Start ticks. <= 0 uses DefaultInterval.
	Interval time.Duration
	// Now returns the current time; nil uses time.Now().UTC(). Tests
	// inject a fixed or fixture-relative clock so window features and
	// backoff transitions are exercised deterministically.
	Now func() time.Time
}

func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now().UTC()
}

// TickResult summarizes one Tick pass.
type TickResult struct {
	// Claimed is how many subjects ClaimDirtySubjects returned.
	Claimed int
	// Scored is how many of those subjects had a scoring round actually
	// committed.
	Scored int
	// Stale is how many rounds lost the compare-and-clear race
	// (store.ErrStaleRound) — not a failure, just a round a newer one beat
	// to the commit (design §4.8's compare-and-clear).
	Stale int
	// Errors holds one entry per subject whose scoring pass failed for a
	// real reason (a store error, a misconfigured rule). Tick itself never
	// returns a non-nil error just because some subjects in the batch
	// failed — a batch of 200 shouldn't abort entirely over one bad
	// subject — but every failure is reported here for the caller
	// (Start's OnTick hook, or a test) to act on.
	Errors []error
}

// Worker runs abusekit's scoring loop. Construct with New; the zero value
// is not usable.
type Worker struct {
	deps     Deps
	stopOnce sync.Once
	stopCh   chan struct{}
}

// New validates deps and returns a Worker, applying documented defaults
// for every optional field.
func New(deps Deps) (*Worker, error) {
	if deps.Store == nil {
		return nil, errors.New("worker: Deps.Store is required")
	}
	if deps.Config == nil {
		return nil, errors.New("worker: Deps.Config is required")
	}
	if deps.Neighbors == nil {
		deps.Neighbors = feature.NoNeighbors
	}
	if deps.BatchSize <= 0 {
		deps.BatchSize = DefaultBatchSize
	}
	if deps.Interval <= 0 {
		deps.Interval = DefaultInterval
	}
	if deps.Calibration == nil {
		deps.Calibration = core.CalibrationSet{}
	}
	return &Worker{deps: deps, stopCh: make(chan struct{})}, nil
}

// Start runs the scoring loop until ctx is done or Stop is called,
// ticking every Deps.Interval. Each tick's TickResult and error (Tick
// itself only errors on a failure to even claim a batch — see TickResult.Errors
// for per-subject failures) are ignored here beyond that; a caller wanting
// to observe them should call Tick directly in its own loop instead of
// Start.
func (w *Worker) Start(ctx context.Context) {
	ticker := time.NewTicker(w.deps.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.stopCh:
			return
		case <-ticker.C:
			_, _ = w.Tick(ctx)
		}
	}
}

// Stop signals Start's loop to return. Safe to call more than once, and
// safe to call even if Start was never called.
func (w *Worker) Stop() {
	w.stopOnce.Do(func() { close(w.stopCh) })
}

// Tick does exactly one scoring pass, synchronously: claim up to
// Deps.BatchSize dirty subjects, score each one, and return a summary.
// Usable directly from a test without a goroutine or a real ticker.
func (w *Worker) Tick(ctx context.Context) (TickResult, error) {
	now := w.deps.now()
	dirty, err := w.deps.Store.ClaimDirtySubjects(ctx, now, w.deps.BatchSize)
	if err != nil {
		return TickResult{}, fmt.Errorf("worker: claim dirty subjects: %w", err)
	}

	result := TickResult{Claimed: len(dirty)}
	for _, d := range dirty {
		scored, err := w.scoreSubject(ctx, d, now)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("%s/%s: %w", d.Tenant, d.Subject, err))
			continue
		}
		if scored {
			result.Scored++
		} else {
			result.Stale++
		}
	}
	return result, nil
}

// elevatedTiers are the subjects.current_tier values design §4.8's 25%
// adapter-budget reservation is available to.
func elevated(tier string) bool { return tier == "medium" || tier == "high" }

// scoreSubject runs one full scoring round for d: load events, extract
// features, build core.RuleState per rule, Plan, execute each call
// (honouring rule-level backoff and budgets), Combine, and commit via
// UpsertVerdicts. Returns (true, nil) on a normal commit, (false, nil) on
// a stale round (store.ErrStaleRound — not a failure), or a non-nil error
// for anything else.
func (w *Worker) scoreSubject(ctx context.Context, d store.DirtySubject, now time.Time) (bool, error) {
	storedEvents, err := w.deps.Store.EventsForSubject(ctx, d.Tenant, d.Subject)
	if err != nil {
		return false, fmt.Errorf("load events: %w", err)
	}
	events := make([]event.Event, len(storedEvents))
	for i, se := range storedEvents {
		events[i] = se.Event
	}

	windows := feature.DefaultWindows(now)
	fr, err := feature.Extract(ctx, d.Tenant, d.Subject, events, w.deps.Neighbors, windows)
	if err != nil {
		return false, fmt.Errorf("extract features: %w", err)
	}

	latest, err := w.deps.Store.LatestVerdicts(ctx, d.Tenant, d.Subject)
	if err != nil {
		return false, fmt.Errorf("load latest verdicts: %w", err)
	}

	ruleStates := make([]core.RuleState, len(w.deps.Config.Rules))
	for i, r := range w.deps.Config.Rules {
		rs := core.RuleState{Rule: r, CalibrationID: "none"} // see Deps.Calibration's doc comment
		if scorer, ok := w.deps.Config.ScorerFor(r); ok {
			rs.ScorerVersion = scorer.Version()
		}
		if lv, ok := latest[r.Name]; ok {
			rs.LastInputHash = lv.InputHash
			rs.LastRisk = lv.Risk
		}
		ruleStates[i] = rs
	}

	calls := core.Plan(fr.Features.Map(), nil, ruleStates)
	results := make([]*model.ScoreResult, len(calls))
	outcomes := make([]core.RuleOutcome, len(calls))
	isElevated := elevated(d.CurrentTier)

	for i, call := range calls {
		r := call.Rule

		if call.Skip && call.SkipReason == core.SkipStageCondition {
			// A staged rule (e.g. lure_similarity's max_subject_age_h, or a
			// shadow rule gated on min_local_risk) simply hasn't reached its
			// staging condition yet — not an error, and (being shadow-mode
			// in every case design §4.5 currently describes) not something
			// that should mark an advise rule degraded either.
			outcomes[i] = core.RuleOutcome{Rule: r, Unscored: true, ErrorCode: "staged"}
			continue
		}

		backoff, err := w.deps.Store.GetRuleBackoff(ctx, d.Tenant, d.Subject, r.Name)
		if err != nil {
			return false, fmt.Errorf("load rule backoff for %s: %w", r.Name, err)
		}
		if backoff.InBackoff(now) {
			outcomes[i] = core.RuleOutcome{Rule: r, Unscored: true, ErrorCode: "backoff"}
			continue
		}

		scorer, ok := w.deps.Config.ScorerFor(r)
		if !ok {
			outcomes[i] = core.RuleOutcome{Rule: r, Unscored: true, ErrorCode: "unknown_scorer"}
			continue
		}

		// S2 interpretation: Plan's SkipInputUnchanged is not exploited
		// here — v0's only scorer (local) is free, instant and
		// deterministic, so recomputing an "unchanged" call costs nothing
		// and produces an identical result. Reusing a skipped call's PRIOR
		// verdict correctly (rather than just its risk) needs the full
		// prior model.ScoreResult, which store.LatestVerdicts doesn't carry
		// (SubjectSignal/LatestVerdict intentionally don't; see their doc
		// comments) — worth adding once a paid vendor scorer (S5) makes
		// skipping actually matter for cost/latency.
		budgeted := r.Scorer != "local" && w.deps.Budgets != nil
		if budgeted {
			if allowed, code := w.deps.Budgets.Allow(r.Scorer, d.Tenant, d.Subject, isElevated, now); !allowed {
				outcomes[i] = core.RuleOutcome{Rule: r, Unscored: true, ErrorCode: code}
				continue
			}
		}

		res, err := scorer.Score(ctx, call.Request)
		if err != nil {
			retryAt := now.Add(backoffDuration(backoff.Attempts + 1))
			if rerr := w.deps.Store.RecordRuleError(ctx, d.Tenant, d.Subject, r.Name, retryAt, err.Error()); rerr != nil {
				return false, fmt.Errorf("record rule error for %s: %w", r.Name, rerr)
			}
			outcomes[i] = core.RuleOutcome{Rule: r, Unscored: true, ErrorCode: "adapter_error"}
			continue
		}

		if budgeted {
			w.deps.Budgets.Record(r.Scorer, d.Tenant, d.Subject, now)
		}
		if backoff.Attempts > 0 {
			if cerr := w.deps.Store.ClearRuleBackoff(ctx, d.Tenant, d.Subject, r.Name); cerr != nil {
				return false, fmt.Errorf("clear rule backoff for %s: %w", r.Name, cerr)
			}
		}

		results[i] = &res
		outcomes[i] = core.RuleOutcome{Rule: r, Result: &res, Reason: renderReason(fr.Features)}
	}

	verdict := core.Combine(outcomes, core.CombineParams{
		Tiers:                       w.deps.Config.Tiers,
		MinScoredAdvise:             w.deps.Config.MinScoredAdvise,
		TextRulesNeedFeatureSupport: w.deps.Config.TextRulesNeedFeatureSupport,
	}, w.deps.Calibration)

	if len(verdict.Signals) != len(calls) {
		return false, fmt.Errorf("worker: internal/core.Combine returned %d signals for %d calls", len(verdict.Signals), len(calls))
	}

	records := make([]store.VerdictRecord, len(calls))
	for i, call := range calls {
		sig := verdict.Signals[i]
		if sig.Rule != call.Rule.Name {
			return false, fmt.Errorf("worker: signal/call order mismatch at index %d: got rule %q, want %q", i, sig.Rule, call.Rule.Name)
		}
		var probs map[string]float64
		if results[i] != nil {
			probs = results[i].Probs
		}
		var risk *float64
		if sig.Status == "scored" {
			r := sig.Risk
			risk = &r
		}
		records[i] = store.VerdictRecord{
			Rule:        sig.Rule,
			Mode:        string(sig.Mode),
			Scorer:      call.Rule.Scorer,
			Model:       sig.Model,
			Checkpoint:  sig.Checkpoint,
			Render:      call.Request.RenderVersion,
			Calibration: sig.Calibration,
			Probs:       probs,
			Risk:        risk,
			Flagged:     sig.Flagged,
			Reason:      sig.Reason,
			InputHash:   call.InputHash,
			Status:      sig.Status,
			ErrorCode:   sig.ErrorCode,
		}
	}

	_, err = w.deps.Store.UpsertVerdicts(ctx, d.Tenant, d.Subject, d.DirtySeq, records, store.SubjectSummary{
		Tier:          verdict.Tier,
		Score:         verdict.Score,
		NextRescoreAt: fr.NextRescoreAt,
	})
	if err != nil {
		if errors.Is(err, store.ErrStaleRound) {
			return false, nil
		}
		return false, fmt.Errorf("upsert verdicts: %w", err)
	}
	return true, nil
}
