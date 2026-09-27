// Package worker is abusekit's scoring loop (design §4.8): it finds
// subjects needing (re)scoring, extracts their features (internal/feature),
// drives internal/core's pure Plan/Combine over them with the registered
// scorers, and records the result (internal/store.UpsertVerdicts) —
// including the compare-and-clear, per-subject claim lease and failure
// backoff, rule-level backoff, and per-adapter/per-subject/per-tenant
// budget enforcement design §4.8 calls for (plus the PR #3 fix round's
// robustness hardening: a panicking scorer, a per-call timeout, and a
// clean cancelable shutdown).
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
	"log/slog"
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

// DefaultScoreTimeout bounds a single scorer.Score call (B2 fix round):
// local answers instantly, but nothing stops a future vendor adapter (S5)
// from hanging past any budget the tick loop can tolerate without a
// per-call deadline of its own.
const DefaultScoreTimeout = 10 * time.Second

// backoffSchedule and backoffCap are design §4.8's "backoff 30s, 2m, 5m,
// capped at 15m" — shared by rule-level backoff (rule_state) and
// whole-pass subject failure backoff (B3 fix round: subjects.fail_count/
// next_attempt_at), since both back off a repeatedly-failing thing on the
// identical schedule.
var backoffSchedule = []time.Duration{30 * time.Second, 2 * time.Minute, 5 * time.Minute}

const backoffCap = 15 * time.Minute

// backoffDuration returns how long to wait before retrying something that
// has now failed attempts consecutive times (attempts >= 1).
func backoffDuration(attempts int) time.Duration {
	if attempts <= 0 {
		attempts = 1
	}
	if i := attempts - 1; i < len(backoffSchedule) {
		return backoffSchedule[i]
	}
	return backoffCap
}

// nextUTCMidnight returns the start of the UTC day after now — when
// internal/worker.Budgets' daily counters reset (B4 fix round: a subject
// that hit a cost_cap denial needs a rescore scheduled for then, or it
// would never be retried once the queue otherwise goes quiet).
func nextUTCMidnight(now time.Time) time.Time {
	now = now.UTC()
	return time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
}

// Store is the narrow slice of *store.Store the worker depends on —
// codebase-design's "small interfaces": exactly the operations one
// scoring pass needs, nothing else. *store.Store satisfies this
// automatically; a test can supply a smaller fake without pulling in a
// database if it only needs to exercise Plan/Combine wiring (the S2 test
// suite doesn't need to — every worker test runs against a real, throwaway
// Postgres, since dirty_seq/scored_seq/claim-lease/FOR UPDATE SKIP LOCKED
// semantics are exactly what's under test).
type Store interface {
	EventsForSubject(ctx context.Context, tenant, subject string) ([]store.StoredEvent, error)
	ClaimDirtySubjects(ctx context.Context, now time.Time, limit int) ([]store.DirtySubject, error)
	LatestVerdicts(ctx context.Context, tenant, subject string) (map[string]store.LatestVerdict, error)
	UpsertVerdicts(ctx context.Context, tenant, subject string, dirtySeqAtStart int64, records []store.VerdictRecord, summary store.SubjectSummary) ([]int64, error)
	GetRuleBackoff(ctx context.Context, tenant, subject, rule string) (store.RuleBackoff, error)
	RecordRuleError(ctx context.Context, tenant, subject, rule string, retryAt time.Time, lastError string) error
	ClearRuleBackoff(ctx context.Context, tenant, subject, rule string) error
	PruneRuleState(ctx context.Context, tenant, subject string, currentRules []string) error
	RecordSubjectFailure(ctx context.Context, tenant, subject string, nextAttemptAt time.Time) error
	ReleaseClaim(ctx context.Context, tenant, subject string) error
	QueueStats(ctx context.Context, now time.Time) (depth int, oldestDirtyAge time.Duration, err error)
}

// Deps are a Worker's dependencies. Store, Config and Neighbors are
// required; everything else has a documented default.
type Deps struct {
	Store     Store
	Config    *config.Config
	Neighbors feature.Neighbors // feature.NewStoreNeighbors(realStore, cfg) in production; feature.NoNeighbors is a valid choice too.
	Brands    feature.BrandSet  // config/brands.yaml, loaded once at startup; the zero value holds name_brand_match at 0.

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

	// Metrics records design §4.8's runtime counters (S8 fix round). nil
	// disables metrics recording entirely — the correct default for a
	// short-lived test that doesn't care about them.
	Metrics *Metrics

	// BatchSize is how many dirty subjects Tick claims per pass. <= 0
	// uses DefaultBatchSize.
	BatchSize int
	// Interval is how often Start ticks. <= 0 uses DefaultInterval.
	Interval time.Duration
	// ScoreTimeout bounds a single scorer.Score call (B2 fix round). <= 0
	// uses DefaultScoreTimeout.
	ScoreTimeout time.Duration
	// Now returns the current time; nil uses time.Now().UTC(). Tests
	// inject a fixed or fixture-relative clock so window features and
	// backoff transitions are exercised deterministically.
	Now func() time.Time
	// Logger receives Tick-level and per-subject failures from Start's
	// loop (B2 fix round: "Tick errors logged, never discarded"). nil uses
	// slog.Default().
	Logger *slog.Logger
}

func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now().UTC()
}

func (d Deps) logger() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.Default()
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
	// real reason (a store error, a misconfigured rule, a scorer panic
	// that recover() caught). Tick itself never returns a non-nil error
	// just because some subjects in the batch failed — a batch of 200
	// shouldn't abort entirely over one bad subject — but every failure is
	// reported here for the caller (Start's own logging, or a test) to act
	// on, and each one has already been recorded against that subject via
	// store.RecordSubjectFailure (B3 fix round) so it backs off rather
	// than being reclaimed and retried every single tick forever.
	Errors []error
}

// Worker runs abusekit's scoring loop. Construct with New; the zero value
// is not usable.
type Worker struct {
	deps Deps

	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	stopped bool // R9 round 2: Stop() was called, possibly before Start() finished initializing — see both methods' doc comments.
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
	if deps.ScoreTimeout <= 0 {
		deps.ScoreTimeout = DefaultScoreTimeout
	}
	if deps.Calibration == nil {
		deps.Calibration = core.CalibrationSet{}
	}
	return &Worker{deps: deps}, nil
}

// Start runs the scoring loop until ctx is done or Stop is called, ticking
// every Deps.Interval. Every Tick error and per-subject failure is logged
// via Deps.Logger (B2 fix round: "Tick errors logged, never discarded") —
// a caller wanting to observe them programmatically should call Tick
// directly in its own loop instead of Start.
//
// Start derives its own cancelable context from ctx (B2 fix round) so
// Stop can cancel it independently of whatever the caller's ctx does; call
// Start at most once per Worker (starting it twice is not supported and
// leaves the first goroutine's cancel/done state overwritten).
//
// R9 round 2: a caller may legitimately run `go w.Start(ctx)` and call
// Stop from another goroutine with NO synchronization in between (that is
// the entire reason Stop exists as its own method rather than just asking
// the caller to cancel ctx itself) — if Stop's goroutine wins that race
// and runs before Start has set w.cancel, it must not silently treat that
// as "Start was never called" and no-op, leaving this goroutine's loop
// running with nothing left able to stop it. Start checks w.stopped
// (set under the SAME mutex Stop uses) right after recording its own
// cancel/done, and honors an already-requested stop immediately rather
// than entering the ticking loop at all.
func (w *Worker) Start(ctx context.Context) {
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	w.mu.Lock()
	w.cancel = cancel
	w.done = done
	alreadyStopped := w.stopped
	w.mu.Unlock()

	defer close(done)

	if alreadyStopped {
		return
	}

	ticker := time.NewTicker(w.deps.Interval)
	defer ticker.Stop()
	log := w.deps.logger()
	for {
		select {
		case <-runCtx.Done():
			return
		case <-ticker.C:
			result, err := w.Tick(runCtx)
			if err != nil {
				log.Error("worker: tick failed", "error", err)
				continue
			}
			for _, subjectErr := range result.Errors {
				log.Error("worker: scoring pass failed", "error", subjectErr)
			}
		}
	}
}

// Stop cancels Start's context and BLOCKS until its goroutine has actually
// returned (B2 fix round) — a caller past Stop() can rely on the worker
// having stopped touching the store, not just having been asked to. Safe
// to call more than once, and safe to call even if Start was never called
// (a no-op in that case) — including when Stop happens to run BEFORE a
// concurrently-starting Start has recorded its cancel func yet (R9 round
// 2): this always records the stop request first, under the same mutex
// Start checks right after its own setup, so a race between the two never
// leaves the worker running with no way left to stop it.
func (w *Worker) Stop() {
	w.mu.Lock()
	w.stopped = true
	cancel := w.cancel
	done := w.done
	w.mu.Unlock()

	if cancel == nil {
		return // Start was never called.
	}
	cancel()
	<-done
}

// Tick does exactly one scoring pass, synchronously: claim up to
// Deps.BatchSize dirty subjects, score each one, and return a summary.
// Usable directly from a test without a goroutine or a real ticker.
//
// Tick checks ctx between subjects (B2 fix round) and stops claiming
// further work — already-scored subjects in this batch stay scored; the
// rest simply wait for the next tick, exactly as if this tick had claimed
// fewer of them to begin with.
func (w *Worker) Tick(ctx context.Context) (TickResult, error) {
	now := w.deps.now()
	dirty, err := w.deps.Store.ClaimDirtySubjects(ctx, now, w.deps.BatchSize)
	if err != nil {
		return TickResult{}, fmt.Errorf("worker: claim dirty subjects: %w", err)
	}

	if w.deps.Metrics != nil {
		if depth, oldest, qerr := w.deps.Store.QueueStats(ctx, now); qerr == nil {
			w.deps.Metrics.SetQueueDepth(depth)
			w.deps.Metrics.SetOldestDirtyAge(oldest)
		}
	}

	result := TickResult{Claimed: len(dirty)}
	for _, d := range dirty {
		if err := ctx.Err(); err != nil {
			break
		}
		scored, err := w.scoreSubject(ctx, d, now)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("%s/%s: %w", d.Tenant, d.Subject, err))
			retryAt := now.Add(backoffDuration(d.FailCount + 1))
			if ferr := w.deps.Store.RecordSubjectFailure(ctx, d.Tenant, d.Subject, retryAt); ferr != nil {
				result.Errors = append(result.Errors, fmt.Errorf("%s/%s: record failure: %w", d.Tenant, d.Subject, ferr))
			}
			if w.deps.Metrics != nil {
				w.deps.Metrics.IncSubjectsFailed()
			}
			continue
		}
		if scored {
			result.Scored++
		} else {
			result.Stale++
			// The stale round's own claim never got to release itself via
			// UpsertVerdicts (the round that actually won already did) —
			// release defensively so a slow loser never holds a lease past
			// its useful life. Best-effort: a failure here isn't this
			// round's problem to report, since the round it lost to is
			// what matters.
			_ = w.deps.Store.ReleaseClaim(ctx, d.Tenant, d.Subject)
		}
	}
	return result, nil
}

// elevated reports whether tier qualifies for design §4.8's 25%
// adapter-budget reservation.
func elevated(tier string) bool { return tier == "medium" || tier == "high" }

// scoreOutcome carries safeScore's goroutine result back to its caller.
type scoreOutcome struct {
	result model.ScoreResult
	err    error
}

// safeScore calls scorer.Score with a per-call timeout (B2 fix round:
// DefaultScoreTimeout/Deps.ScoreTimeout — local never needs it, but
// nothing bounds a future network-bound vendor adapter otherwise) and
// recovers a panic into a plain error (B2, proven necessary: an adapter
// panic must never escape Tick or take down Start's whole loop) so every
// caller sees exactly one failure mode — a returned error — regardless of
// why the scorer didn't answer.
//
// R3 round 2: Score runs in its own goroutine, and safeScore selects on
// EITHER that goroutine finishing OR callCtx.Done() — a context.WithTimeout
// alone only cancels the CONTEXT, it can't force an uncooperative callee to
// return, so a scorer that ignores ctx entirely (blocks on a channel, a
// mutex, a slow syscall) previously hung safeScore, and through it Tick,
// and through it Start's whole loop, indefinitely; Stop() would then block
// on <-done for the same reason. The spawned goroutine may still leak
// (blocked inside Score forever, if the scorer truly never returns), but it
// can no longer block the CALLER — the buffered channel absorbs its result
// whenever/if it eventually arrives, with nobody left listening.
func safeScore(ctx context.Context, scorer model.Scorer, req model.ScoreRequest, timeout time.Duration) (model.ScoreResult, error) {
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	done := make(chan scoreOutcome, 1)
	go func() {
		res, err := safeScoreCall(callCtx, scorer, req)
		done <- scoreOutcome{res, err}
	}()

	select {
	case o := <-done:
		return o.result, o.err
	case <-callCtx.Done():
		return model.ScoreResult{}, fmt.Errorf("scorer %s: %w", scorer.Name(), callCtx.Err())
	}
}

// safeScoreCall runs scorer.Score itself, recovering a panic into a plain
// error (B2 fix round) — split out from safeScore so the recover() runs
// inside the SAME goroutine that calls Score (a deferred recover in a
// different goroutine than the panic can never catch it).
func safeScoreCall(ctx context.Context, scorer model.Scorer, req model.ScoreRequest) (result model.ScoreResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("scorer %s panicked: %v", scorer.Name(), r)
		}
	}()
	return scorer.Score(ctx, req)
}

// scoreSubject runs one full scoring round for d: load events, extract
// features, build core.RuleState per rule, Plan, execute each call
// (honouring rule-level backoff, reusing an input-unchanged prior result,
// and budgets), Combine, and commit via UpsertVerdicts. Returns (true,
// nil) on a normal commit, (false, nil) on a stale round (store.ErrStaleRound
// — not a failure), or a non-nil error for anything else (the caller,
// Tick, records this as a whole-pass failure via
// store.RecordSubjectFailure — B3 fix round).
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
	fr, err := feature.Extract(ctx, d.Tenant, d.Subject, events, w.deps.Neighbors, windows, w.deps.Brands)
	if err != nil {
		return false, fmt.Errorf("extract features: %w", err)
	}

	latest, err := w.deps.Store.LatestVerdicts(ctx, d.Tenant, d.Subject)
	if err != nil {
		return false, fmt.Errorf("load latest verdicts: %w", err)
	}

	currentRuleNames := make([]string, len(w.deps.Config.Rules))
	ruleStates := make([]core.RuleState, len(w.deps.Config.Rules))
	for i, r := range w.deps.Config.Rules {
		currentRuleNames[i] = r.Name
		rs := core.RuleState{Rule: r, CalibrationID: "none"} // see Deps.Calibration's doc comment
		if scorer, ok := w.deps.Config.ScorerFor(r); ok {
			rs.ScorerVersion = scorer.Version()
		}
		if lv, ok := latest[r.Name]; ok {
			// R7 round 2, proven: LatestVerdict.InputHash reflects the
			// literal latest row regardless of status. Feeding an ERRORED
			// round's hash to Plan as LastInputHash meant that once
			// quantization (below) made subject_age_h/upgrade_delay_min
			// repeat across rounds, a rule already PAST its backoff window
			// would match that stale error hash and skip re-invoking the
			// scorer at all — silently freezing on the SAME error forever
			// instead of actually retrying. Only a SCORED round's hash
			// means "the answer we already have is still valid, no need to
			// call again" — an errored round has no valid answer to reuse,
			// so it must never suppress a retry once backoff clears.
			if lv.Status == "scored" {
				rs.LastInputHash = lv.InputHash
			}
			rs.LastRisk = lv.Risk
		}
		ruleStates[i] = rs
	}
	// S11 fix round: a rule renamed or removed from config no longer needs
	// its backoff row — harmless to leave (GetRuleBackoff is only ever
	// queried by name a live rule actually has), but there's no reason to
	// keep it either.
	if err := w.deps.Store.PruneRuleState(ctx, d.Tenant, d.Subject, currentRuleNames); err != nil {
		return false, fmt.Errorf("prune rule_state: %w", err)
	}

	calls := core.Plan(fr.Features.Map(), nil, ruleStates) // N1 fix round: nil Text — v0 has no text-input rule registered yet (S5 adds the first one); Plan's own doc comment covers what a non-nil map would do.
	results := make([]*model.ScoreResult, len(calls))
	outcomes := make([]core.RuleOutcome, len(calls))
	isElevated := elevated(d.CurrentTier)

	// retryCandidates and costCapped feed this round's next_rescore_at
	// (B4 fix round), alongside fr.NextRescoreAt (feature-window decay):
	// a rule currently backing off, or a budget a rule just hit, must get
	// its own rescore scheduled or it would sit unretried until this
	// subject's next unrelated event.
	var retryCandidates []time.Time
	costCapped := false

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
			retryCandidates = append(retryCandidates, *backoff.RetryAt)
			continue
		}

		// S6/S11 fix round: honour Plan's SkipInputUnchanged by reusing the
		// prior round's own result rather than re-invoking the scorer —
		// proven wasteful otherwise (60 sends in quick succession produced
		// 59 redundant full recomputations of an unchanged rule). This
		// still writes a (cheap, local-Postgres-only) verdict row each
		// round rather than skipping the write entirely — the reviewed
		// alternative ("or at least skip the vendor call") this PR takes,
		// since making UpsertVerdicts advance scored_seq with zero new
		// verdict rows would be a materially bigger change to its
		// already-reviewed contract for a cost (a Postgres row) v0 isn't
		// actually trying to protect.
		if call.Skip && call.SkipReason == core.SkipInputUnchanged {
			if lv, ok := latest[r.Name]; ok && lv.Status == "scored" {
				res := model.ScoreResult{Probs: lv.Probs, Model: lv.Model, Checkpoint: lv.Checkpoint, Render: call.Request.RenderVersion}
				results[i] = &res
				outcomes[i] = core.RuleOutcome{Rule: r, Result: &res, Reason: lv.Reason}
			} else if ok {
				outcomes[i] = core.RuleOutcome{Rule: r, Unscored: true, ErrorCode: lv.ErrorCode}
			} else {
				// Shouldn't happen (a non-empty LastInputHash that matched
				// implies a prior verdict row exists), but never invent an
				// outcome for a rule we have no record of.
				outcomes[i] = core.RuleOutcome{Rule: r, Unscored: true, ErrorCode: "unknown_scorer"}
			}
			continue
		}

		scorer, ok := w.deps.Config.ScorerFor(r)
		if !ok {
			outcomes[i] = core.RuleOutcome{Rule: r, Unscored: true, ErrorCode: "unknown_scorer"}
			continue
		}

		budgeted := r.Scorer != "local" && w.deps.Budgets != nil
		if budgeted {
			allowed, code, berr := w.deps.Budgets.Allow(ctx, r.Scorer, d.Tenant, d.Subject, isElevated, now)
			if berr != nil {
				return false, fmt.Errorf("check budget for %s: %w", r.Name, berr)
			}
			if !allowed {
				outcomes[i] = core.RuleOutcome{Rule: r, Unscored: true, ErrorCode: code}
				costCapped = true
				if w.deps.Metrics != nil {
					w.deps.Metrics.IncBudgetDenials(r.Scorer)
				}
				continue
			}
		}

		start := now
		if w.deps.Metrics != nil {
			w.deps.Metrics.IncAdapterCalls(r.Scorer)
		}
		res, err := safeScore(ctx, scorer, call.Request, w.deps.ScoreTimeout)
		if w.deps.Metrics != nil {
			w.deps.Metrics.ObserveAdapterLatency(r.Scorer, w.deps.now().Sub(start))
		}
		if err != nil {
			if w.deps.Metrics != nil {
				w.deps.Metrics.IncAdapterErrors(r.Scorer)
			}
			retryAt := now.Add(backoffDuration(backoff.Attempts + 1))
			if rerr := w.deps.Store.RecordRuleError(ctx, d.Tenant, d.Subject, r.Name, retryAt, err.Error()); rerr != nil {
				return false, fmt.Errorf("record rule error for %s: %w", r.Name, rerr)
			}
			outcomes[i] = core.RuleOutcome{Rule: r, Unscored: true, ErrorCode: "adapter_error"}
			retryCandidates = append(retryCandidates, retryAt)
			continue
		}

		if budgeted {
			if berr := w.deps.Budgets.Record(ctx, r.Scorer, d.Tenant, d.Subject, now); berr != nil {
				return false, fmt.Errorf("record budget usage for %s: %w", r.Name, berr)
			}
		}
		if backoff.Attempts > 0 {
			if cerr := w.deps.Store.ClearRuleBackoff(ctx, d.Tenant, d.Subject, r.Name); cerr != nil {
				return false, fmt.Errorf("clear rule backoff for %s: %w", r.Name, cerr)
			}
		}

		results[i] = &res
		// N1 fix round: renderReason is only ever attached to a rule
		// actually scored by "local" — the design's real template
		// Explainer (§4.6) is what would produce a reason for a vendor
		// scorer; this v0 placeholder summary is local-specific (it reads
		// off the SAME feature values local's weights use) and would be
		// actively misleading attached to a rule it didn't help score.
		reason := ""
		if r.Scorer == "local" {
			reason = renderReason(fr.Features)
		}
		outcomes[i] = core.RuleOutcome{Rule: r, Result: &res, Reason: reason}
	}

	verdict := core.Combine(outcomes, core.CombineParams{
		Tiers:                       w.deps.Config.Tiers,
		MinScoredAdvise:             w.deps.Config.MinScoredAdvise,
		TextRulesNeedFeatureSupport: w.deps.Config.TextRulesNeedFeatureSupport,
	}, w.deps.Calibration)

	if len(verdict.Signals) != len(calls) {
		return false, fmt.Errorf("worker: internal/core.Combine returned %d signals for %d calls", len(verdict.Signals), len(calls))
	}
	if w.deps.Metrics != nil {
		w.deps.Metrics.IncVerdictsByTier(verdict.Tier)
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

	nextRescoreAt := fr.NextRescoreAt
	for _, t := range retryCandidates {
		if nextRescoreAt.IsZero() || t.Before(nextRescoreAt) {
			nextRescoreAt = t
		}
	}
	if costCapped {
		if reset := nextUTCMidnight(now); nextRescoreAt.IsZero() || reset.Before(nextRescoreAt) {
			nextRescoreAt = reset
		}
	}

	_, err = w.deps.Store.UpsertVerdicts(ctx, d.Tenant, d.Subject, d.DirtySeq, records, store.SubjectSummary{
		Tier:          verdict.Tier,
		Score:         verdict.Score,
		NextRescoreAt: nextRescoreAt,
	})
	if err != nil {
		if errors.Is(err, store.ErrStaleRound) {
			return false, nil
		}
		return false, fmt.Errorf("upsert verdicts: %w", err)
	}
	return true, nil
}
