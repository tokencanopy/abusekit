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
	ReleaseClaim(ctx context.Context, tenant, subject string, claimedUntil time.Time) error
	ExtendClaims(ctx context.Context, tenants, subjects []string, now time.Time) error
	QueueStats(ctx context.Context, now time.Time) (depth int, oldestDirtyAge time.Duration, err error)
	// ClaimSubjectForEvaluate backs EvaluateSubject (S3): design §4.4's
	// synchronous POST .../evaluate.
	ClaimSubjectForEvaluate(ctx context.Context, tenant, subject string, now time.Time) (store.DirtySubject, error)
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
	for i, d := range dirty {
		if err := ctx.Err(); err != nil {
			break
		}

		// R4 round 2: extend the WHOLE remaining tail's claim lease from a
		// freshly-taken now — not the tick-start now used to claim the
		// batch — before processing this subject. A single-subject
		// extension isn't enough: with a lease shorter than the batch's
		// cumulative processing time, a subject further back in the queue
		// would sit on its UNTOUCHED original claim until its own turn
		// comes up, which can already be past that original lease's
		// expiry. Re-extending everything still pending on every
		// iteration bounds that gap to one subject's own worst-case
		// scoring time, never the whole batch's.
		subjectNow := w.deps.now()
		remaining := dirty[i:]
		tenants := make([]string, len(remaining))
		subjects := make([]string, len(remaining))
		for j, rd := range remaining {
			tenants[j] = rd.Tenant
			subjects[j] = rd.Subject
		}
		if err := w.deps.Store.ExtendClaims(ctx, tenants, subjects, subjectNow); err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("%s/%s: extend claim: %w", d.Tenant, d.Subject, err))
			retryAt := subjectNow.Add(backoffDuration(d.FailCount + 1))
			if ferr := w.deps.Store.RecordSubjectFailure(ctx, d.Tenant, d.Subject, retryAt); ferr != nil {
				result.Errors = append(result.Errors, fmt.Errorf("%s/%s: record failure: %w", d.Tenant, d.Subject, ferr))
			}
			if w.deps.Metrics != nil {
				w.deps.Metrics.IncSubjectsFailed()
			}
			continue
		}

		scored, err := w.scoreSubject(ctx, d, subjectNow)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("%s/%s: %w", d.Tenant, d.Subject, err))
			retryAt := subjectNow.Add(backoffDuration(d.FailCount + 1))
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
			// what matters. d.ClaimedUntil (T3, round 3) is the exact value
			// THIS claim set, so ReleaseClaim's compare-and-clear can never
			// touch the newer round's own (different) lease.
			_ = w.deps.Store.ReleaseClaim(ctx, d.Tenant, d.Subject, d.ClaimedUntil)
		}
	}
	return result, nil
}

// elevated reports whether tier qualifies for design §4.8's 25%
// adapter-budget reservation.
func elevated(tier string) bool { return tier == "medium" || tier == "high" }

// countTrue counts the true values in bs — computeVerdict's R3 (round 2
// fix round) omitted-call filter uses it to size the filtered slices
// exactly, and to skip the filtering pass entirely when nothing was
// omitted.
func countTrue(bs []bool) int {
	n := 0
	for _, b := range bs {
		if b {
			n++
		}
	}
	return n
}

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

// scoreSubject runs one full scoring round for d via computeVerdict, then
// commits it. Returns (true, nil) on a normal commit, (false, nil) on a
// stale round (store.ErrStaleRound — not a failure), or a non-nil error
// for anything else (the caller, Tick, records this as a whole-pass
// failure via store.RecordSubjectFailure — B3 fix round).
func (w *Worker) scoreSubject(ctx context.Context, d store.DirtySubject, now time.Time) (bool, error) {
	// syncOnly is always false here, so the returned syncOnlyDeferred flag
	// (R3, round 2 fix round) is always false too — Tick always fully
	// validates every rule against d.DirtySeq, never deferring any of them
	// the way EvaluateSubject's syncOnly path can.
	verdict, records, nextRescoreAt, _, err := w.computeVerdict(ctx, ctx, d, now, false)
	if err != nil {
		return false, err
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

// releaseClaimTimeout bounds EvaluateSubject's best-effort claim release
// (B3 fix round) — long enough to comfortably complete a single UPDATE
// under normal load, short enough never to hang a request whose own
// caller has already given up.
const releaseClaimTimeout = 5 * time.Second

// EvaluateSubject is design §4.4's POST /v1/subjects/{subject}/evaluate:
// the same per-subject scoring path scoreSubject runs on the worker's own
// ticker, invoked synchronously for one named subject instead of a
// claimed batch. It shares scoreSubject's exact lease (via
// store.ClaimSubjectForEvaluate, which can never overlap a concurrent
// worker Tick's own claim on the same subject — see that method's doc
// comment), timeout, and budget rules.
//
// deadline bounds ONLY the scorer calls (S1 fix round: "the caller
// deadline applies to scorer calls only, not DB reads" — the claim,
// EventsForSubject, LatestVerdicts and PruneRuleState calls all run
// against ctx directly, never artificially shortened by deadline_ms, so a
// slow DB under load degrades gracefully instead of spuriously reporting
// "deadline exceeded" for a delay that had nothing to do with any
// scorer). <= 0 means "no additional deadline beyond ctx's own" (a test
// convenience — internal/serve's handler always supplies a positive
// deadline). Within that bound, only rules scored by "local" actually run
// synchronously (see computeVerdict's syncOnly parameter) — v0 has no
// vendor scorer whose measured p99 latency could ever justify running it
// inline on a product's send path, so this is the conservative default
// until one exists; see the S3 PR body for that interpretation.
//
// Returns (false, err) for every case that did NOT produce a fresh
// verdict — the caller (internal/serve) is expected to fall back to
// store.SubjectView for its response body in each of these instead of
// failing the whole request:
//   - store.ErrNotFound: the subject has never been seen by this tenant
//     (maps to 404 — there is no stored view to fall back to either).
//   - store.ErrNotScorable: the subject is class internal/synthetic
//     (design §4.3: "stored but never scored") — S1 fix round: e2a's own
//     prober accounts are synthetic, so this must be a normal 200 with
//     the stored (likely tier "unknown") view, never a 500.
//   - *store.ErrBusy: already claimed (a worker Tick, or a concurrent
//     evaluate call) or in whole-pass failure backoff — carries a REAL
//     RetryAt (S1 fix round) derived from the actual lease/backoff still
//     remaining, not a guess.
//   - context.DeadlineExceeded / context.Canceled: the scorer-call
//     deadline (or the caller's own ctx) elapsed before any rule could be
//     attempted (S1 fix round) — the round is abandoned rather than
//     committing whatever partial result existed, and the caller falls
//     back to the last known-good stored view.
//   - anything else: a genuine internal failure (maps to 500).
//
// Returns (true, nil) once a fresh round has actually committed —
// including the case where a concurrent round committed a NEWER one first
// (store.ErrStaleRound): the subject's current score IS fresh, just not
// from bytes this exact call produced, so the caller's stored-view
// fallback still reports evaluated_now correctly either way.
func (w *Worker) EvaluateSubject(ctx context.Context, tenant, subject string, deadline time.Duration) (evaluatedNow bool, err error) {
	now := w.deps.now()
	d, err := w.deps.Store.ClaimSubjectForEvaluate(ctx, tenant, subject, now)
	if err != nil {
		// R2 (round 2 fix round): this early return happens BEFORE the
		// release-on-not-committed defer below is even registered, so it
		// needs its own release. Ordinarily that would be wrong — we never
		// actually claimed anything on this path — but store.ErrClaimAmbiguous
		// specifically means the claim's COMMIT itself raced ctx expiring:
		// the UPDATE may have landed durably on the server even though we
		// got an error back. Best-effort release with a context independent
		// of ctx (which is likely already expired/cancelled — that's how we
		// got here) and a short bound of its own. Proven necessary: without
		// this, a claim left behind by an ambiguous commit sits for the full
		// ~2-minute lease with nothing to release it — neither a later
		// evaluate call nor the worker's own Tick can touch the subject
		// until the lease itself expires.
		//
		// T3 (round 3): passes ambiguous.ClaimedUntil — the exact value THIS
		// ambiguous commit attempted to set — to ReleaseClaim's own
		// compare-and-clear rather than a bare (tenant, subject). A plain
		// unconditional release here would clear WHATEVER lease happens to
		// be on the row at the moment this runs, including a DIFFERENT,
		// still-active one the worker's own next Tick (or another evaluate
		// call) legitimately took in the meantime, since the commit's real
		// outcome was never certain in the first place.
		var ambiguous *store.ErrClaimAmbiguous
		if errors.As(err, &ambiguous) {
			releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseClaimTimeout)
			if rerr := w.deps.Store.ReleaseClaim(releaseCtx, tenant, subject, ambiguous.ClaimedUntil); rerr != nil {
				w.deps.logger().Error("worker: release ambiguous evaluate claim failed", "tenant", tenant, "error", rerr)
			}
			cancel()
		}
		return false, err
	}

	// B3 fix round: whatever happens next, release the claim unless we
	// actually commit — using a context INDEPENDENT of ctx's own
	// cancellation (context.WithoutCancel) so a caller that already
	// disconnected or timed out doesn't also prevent its own cleanup from
	// running, bounded by releaseClaimTimeout so a release attempt can
	// never hang indefinitely either. Proven necessary: a bare `defer
	// ReleaseClaim(ctx, ...)` using the caller's own (already-cancelled)
	// ctx makes the release call itself fail immediately, leaving the
	// subject claimed for the full ~2-minute lease.
	//
	// T3 (round 3): d.ClaimedUntil is the exact value THIS call's own claim
	// set — ReleaseClaim's compare-and-clear means this defer can never
	// clear a DIFFERENT lease a concurrent claimant took after this one's
	// own natural expiry or an otherwise-unexpected release path.
	committed := false
	defer func() {
		if committed {
			return
		}
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseClaimTimeout)
		defer cancel()
		if rerr := w.deps.Store.ReleaseClaim(releaseCtx, tenant, subject, d.ClaimedUntil); rerr != nil {
			w.deps.logger().Error("worker: release evaluate claim failed", "tenant", tenant, "error", rerr)
		}
	}()

	// S1 fix round: the deadline bounds ONLY scoreCtx, passed to
	// computeVerdict as the context scorer calls (safeScore) use — ctx
	// itself (unmodified) is what DB reads use. See this method's own doc
	// comment for why.
	scoreCtx := ctx
	if deadline > 0 {
		var cancel context.CancelFunc
		scoreCtx, cancel = context.WithTimeout(ctx, deadline)
		defer cancel()
	}

	verdict, records, nextRescoreAt, syncOnlyDeferred, cvErr := w.computeVerdict(ctx, scoreCtx, d, now, true)
	if cvErr != nil {
		// A context error here means the round couldn't even get through
		// its DB-read/plan phase before ctx itself (the caller's own,
		// never deadline_ms-shortened) gave up — propagate it as-is so the
		// caller can recognize it via errors.Is rather than a wrapped
		// opaque failure.
		if errors.Is(cvErr, context.DeadlineExceeded) || errors.Is(cvErr, context.Canceled) {
			return false, cvErr
		}
		return false, fmt.Errorf("worker: evaluate %s/%s: %w", tenant, subject, cvErr)
	}
	// S1 fix round: checked AFTER computeVerdict (which only spends
	// scoreCtx on the scorer-call phase, always the last thing it does)
	// rather than before it — scoreCtx cannot be "already" expired the
	// instant it's created (its deadline is relative to creation), so the
	// only way it's expired here is real elapsed time during the DB-read
	// phase eating the whole budget before scoring ever got a chance. When
	// that happens, no rule was genuinely "evaluated now": abandon the
	// round (never commit a verdict built against a blown budget) and let
	// the caller fall back to the stored view.
	if err := scoreCtx.Err(); err != nil {
		return false, err
	}

	// R3 (round 2 fix round): every call this round could have made was
	// omitted (every configured rule is non-local, syncOnly, with no prior
	// scored result to carry forward — never true of v0's shipped config,
	// which always has at least the local rule, but reachable with a
	// smaller test/future config). UpsertVerdicts itself treats an empty
	// records slice as a no-op that "does not touch subjects at all" (its
	// own doc comment) — including never clearing claimed_until — so
	// calling it here would leave committed=true below with nothing
	// actually committed, leaking this claim for its full lease. Skip it
	// outright and report "nothing evaluated," not a failure: the deferred
	// release above (committed is still false) hands the subject straight
	// back.
	if len(records) == 0 {
		return false, nil
	}

	// R3 (round 2 fix round): dirtySeqAtStart is what UpsertVerdicts
	// advances scored_seq TO (GREATEST(scored_seq, dirtySeqAtStart)) — a
	// round that deferred a non-local rule (carried its last result
	// forward, or omitted it for lack of one) never actually validated
	// that rule against d.DirtySeq's inputs, so advancing scored_seq up to
	// d.DirtySeq would wrongly tell ClaimDirtySubjects' dirty-seq check
	// this subject is fully caught up — starving that rule of ever
	// getting a genuine (non-syncOnly) round. Passing d.ScoredSeq instead
	// is a no-op on scored_seq (GREATEST(scored_seq, d.ScoredSeq) can only
	// ever be scored_seq itself, since d.ScoredSeq IS scored_seq as of the
	// claim), which leaves dirty_seq > scored_seq exactly as it already
	// was — the subject stays claimable by the worker's own Tick for that
	// rule's real round, while current_tier/current_score/the verdict rows
	// themselves still land normally either way.
	dirtySeqAtStart := d.DirtySeq
	if syncOnlyDeferred {
		dirtySeqAtStart = d.ScoredSeq
	}
	if _, upErr := w.deps.Store.UpsertVerdicts(ctx, tenant, subject, dirtySeqAtStart, records, store.SubjectSummary{
		Tier:          verdict.Tier,
		Score:         verdict.Score,
		NextRescoreAt: nextRescoreAt,
	}); upErr != nil {
		if errors.Is(upErr, store.ErrStaleRound) {
			// A concurrent round (astonishing under ClaimSubjectForEvaluate's
			// exclusive lease, but UpsertVerdicts' compare-and-clear is the
			// authoritative guard, not the claim) already committed a
			// NEWER summary — the subject's current score is still fresh,
			// just not from this call; report success, not failure.
			committed = true
			return true, nil
		}
		return false, fmt.Errorf("worker: evaluate %s/%s: upsert verdicts: %w", tenant, subject, upErr)
	}
	committed = true
	return true, nil
}

// computeVerdict is scoreSubject/EvaluateSubject's shared computation:
// load events, extract features, build core.RuleState per rule, Plan,
// execute each call (honouring rule-level backoff, reusing an
// input-unchanged prior result, and budgets), and Combine — everything
// scoreSubject used to do inline, up to but NOT including the
// UpsertVerdicts commit, which its two callers each do themselves (they
// commit to different rows — d.Tenant/d.Subject either way, but
// EvaluateSubject's caller cares about the returned core.Verdict directly,
// where scoreSubject's doesn't).
//
// ctx and scoreCtx are deliberately separate (S1 fix round): every DB call
// in here (EventsForSubject, LatestVerdicts, PruneRuleState, rule_state
// reads/writes, budget checks) uses ctx; only the scorer call itself
// (safeScore) uses scoreCtx, which EvaluateSubject shortens to its
// deadline_ms budget — a slow DB never gets mistaken for "the deadline was
// too short." scoreSubject (the worker's own Tick path) passes the SAME
// context for both, so this split changes nothing for it.
//
// syncOnly, when true (EvaluateSubject only), restricts LIVE execution to
// rules scored by "local" (see EvaluateSubject's own doc comment for why):
// a non-local rule with a prior SCORED result carries it forward exactly
// as an input-unchanged skip would (B4 fix round: never overwrite an
// existing vendor verdict with "unscored" just because evaluate can't
// synchronously refresh it); only a non-local rule with NO prior scored
// result at all is marked unscored/"sync_scorer_unsupported".
func (w *Worker) computeVerdict(ctx, scoreCtx context.Context, d store.DirtySubject, now time.Time, syncOnly bool) (core.Verdict, []store.VerdictRecord, time.Time, bool, error) {
	storedEvents, err := w.deps.Store.EventsForSubject(ctx, d.Tenant, d.Subject)
	if err != nil {
		return core.Verdict{}, nil, time.Time{}, false, fmt.Errorf("load events: %w", err)
	}
	events := make([]event.Event, len(storedEvents))
	for i, se := range storedEvents {
		events[i] = se.Event
	}

	windows := feature.DefaultWindows(now)
	fr, err := feature.Extract(ctx, d.Tenant, d.Subject, events, w.deps.Neighbors, windows, w.deps.Brands)
	if err != nil {
		return core.Verdict{}, nil, time.Time{}, false, fmt.Errorf("extract features: %w", err)
	}

	latest, err := w.deps.Store.LatestVerdicts(ctx, d.Tenant, d.Subject)
	if err != nil {
		return core.Verdict{}, nil, time.Time{}, false, fmt.Errorf("load latest verdicts: %w", err)
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
		return core.Verdict{}, nil, time.Time{}, false, fmt.Errorf("prune rule_state: %w", err)
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

	// inputHashOverride carries, per call index, the STORED input_hash to
	// record instead of this round's freshly-computed call.InputHash (B4
	// fix round) — set only for a syncOnly-carried-forward non-local rule
	// (below). Recording the OLD hash rather than the new one is load-
	// bearing: it means the round's own bookkeeping never claims this rule
	// was validated against the CURRENT inputs, so the next real (non-
	// syncOnly) round still sees a hash mismatch and genuinely re-invokes
	// the vendor scorer — recording the fresh hash instead would falsely
	// look like "already validated," permanently starving it of a real
	// re-score. "" means no override (use call.InputHash as normal).
	inputHashOverride := make([]string, len(calls))

	// omit marks a call index for exclusion from this round entirely (R3,
	// round 2 fix round) — set only for a syncOnly non-local rule with no
	// prior scored result to carry forward (see below). Filtered out of
	// calls/outcomes/results/inputHashOverride together, right before
	// core.Combine, so Combine never sees (and verdicts history never
	// records) a rule evaluate never actually asked a question.
	omit := make([]bool, len(calls))

	// syncOnlyDeferred is set whenever THIS round left a non-local rule
	// either carried forward or omitted rather than genuinely re-run
	// against the CURRENT inputs (R3, round 2 fix round) — see its use
	// after this loop, where it holds back scored_seq so the subject
	// stays claimable for that rule's real, non-syncOnly round.
	syncOnlyDeferred := false

	for i, call := range calls {
		r := call.Rule

		// EvaluateSubject's syncOnly restriction (design §4.4: "the local
		// scorer always can; vendor scorers only if their p99 fits" — v0
		// has no measured p99 for any vendor scorer). B4 fix round: this
		// must NOT overwrite an existing vendor verdict with "unscored" —
		// doing so silently dropped that rule's risk from THIS round's
		// score/tier computation even though nothing about the rule's own
		// last real answer changed. Instead, carry the rule's latest
		// SCORED result forward exactly as SkipInputUnchanged already does
		// below, so its risk still counts toward this round's tier. Checked
		// before the stage/backoff/skip logic below so a staged or
		// backed-off non-local rule is handled identically to any other
		// non-local rule under evaluate.
		if syncOnly && r.Scorer != "local" {
			syncOnlyDeferred = true
			if lv, ok := latest[r.Name]; ok && lv.Status == "scored" {
				res := model.ScoreResult{Probs: lv.Probs, Model: lv.Model, Checkpoint: lv.Checkpoint, Render: call.Request.RenderVersion}
				results[i] = &res
				outcomes[i] = core.RuleOutcome{Rule: r, Result: &res, Reason: lv.Reason}
				inputHashOverride[i] = lv.InputHash
			} else {
				// R3 (round 2 fix round): nothing to carry forward, and
				// evaluate can't run this scorer live — omit the rule from
				// this round entirely rather than recording a synthetic
				// "sync_scorer_unsupported" attempt that never happened.
				// Proven necessary: that synthetic verdict row made it look
				// like this rule had already been considered and found
				// wanting for this dirty_seq, when in truth evaluate never
				// asked it anything — the worker's own Tick (the only path
				// that can actually give this rule a first answer) still
				// needs to see it as due, not already handled.
				omit[i] = true
			}
			continue
		}

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
			return core.Verdict{}, nil, time.Time{}, false, fmt.Errorf("load rule backoff for %s: %w", r.Name, err)
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
				return core.Verdict{}, nil, time.Time{}, false, fmt.Errorf("check budget for %s: %w", r.Name, berr)
			}
			if !allowed {
				outcomes[i] = core.RuleOutcome{Rule: r, Unscored: true, ErrorCode: code}
				costCapped = true
				if w.deps.Metrics != nil {
					w.deps.Metrics.IncBudgetDenials(r.Scorer)
				}
				// R8 round 2: a budget denial is an operationally
				// significant event (a rule silently stopped scoring for a
				// whole tenant/adapter combination) — must be visible in
				// logs, not just an incrementing counter nobody's
				// necessarily watching yet.
				w.deps.logger().Warn("worker: budget denied", "tenant", d.Tenant, "subject", d.Subject, "rule", r.Name, "adapter", r.Scorer, "code", code)
				continue
			}
		}

		// R8 round 2: start is taken HERE, immediately before this
		// specific scorer.Score call — not once for the whole subject
		// (the old `start := now` reused the subject-level now for every
		// rule) — so a subject scored by more than one rule doesn't have
		// its LATER rules' recorded latency inflated by however long an
		// EARLIER rule's own call took.
		start := w.deps.now()
		if w.deps.Metrics != nil {
			w.deps.Metrics.IncAdapterCalls(r.Scorer)
		}
		res, err := safeScore(scoreCtx, scorer, call.Request, w.deps.ScoreTimeout)
		if w.deps.Metrics != nil {
			w.deps.Metrics.ObserveAdapterLatency(r.Scorer, w.deps.now().Sub(start))
		}
		if err != nil {
			if w.deps.Metrics != nil {
				w.deps.Metrics.IncAdapterErrors(r.Scorer)
			}
			// R1 (round 2 fix round): a call cut short by scoreCtx's OWN
			// deadline (evaluate's deadline_ms) or by the caller's outer ctx
			// being cancelled is NOT a scorer failure — it's evaluate's own
			// synchronous budget running out, which can happen to a
			// perfectly healthy "local" scorer too (syncOnly still calls it
			// live). Proven necessary: recording it via RecordRuleError put
			// the RULE itself into backoff (rule_state.retry_at, 30s+) even
			// though the round it happened in is then ABANDONED below
			// (scoreCtx.Err() check) and never committed — so a tiny
			// deadline_ms on one evaluate call was silently poisoning every
			// SUBSEQUENT normal-deadline evaluate AND the worker's own Tick
			// for up to 30s, turning a transient budget miss into a real
			// outage. Marked unscored/deadline_exceeded with no backoff
			// bookkeeping and no retry scheduling — a future round (this
			// same evaluate retried, or the worker's own Tick, neither of
			// which artificially shortens ctx) tries again with a full
			// budget on its own schedule.
			//
			// T1 (round 3): checking errors.Is(err, context.DeadlineExceeded)
			// ALONE also matched safeScore's own PER-CALL ScoreTimeout — a
			// completely different, genuine adapter timeout, wrapped in its
			// own context.WithTimeout(scoreCtx, w.deps.ScoreTimeout) child
			// that expires on its own schedule regardless of scoreCtx's
			// (the ROUND's) budget. In Tick specifically (ctx==scoreCtx,
			// never artificially shortened by a deadline_ms), a scorer that
			// simply timed out was committed unscored with NO backoff and
			// no retry scheduled — silently going quiet for up to
			// next_rescore_at's feature-window-decay fallback (hours, not
			// the ~30s a real adapter failure should retry after). The
			// distinguishing signal is scoreCtx.Err() itself, checked here
			// rather than the error safeScore returned: it is non-nil ONLY
			// when the ROUND's own outer context is what expired/was
			// cancelled — a child callCtx's shorter ScoreTimeout firing on
			// its own never sets its parent scoreCtx's Err().
			if scoreCtx.Err() != nil {
				outcomes[i] = core.RuleOutcome{Rule: r, Unscored: true, ErrorCode: "deadline_exceeded"}
				continue
			}
			retryAt := now.Add(backoffDuration(backoff.Attempts + 1))
			if rerr := w.deps.Store.RecordRuleError(ctx, d.Tenant, d.Subject, r.Name, retryAt, err.Error()); rerr != nil {
				return core.Verdict{}, nil, time.Time{}, false, fmt.Errorf("record rule error for %s: %w", r.Name, rerr)
			}
			outcomes[i] = core.RuleOutcome{Rule: r, Unscored: true, ErrorCode: "adapter_error"}
			retryCandidates = append(retryCandidates, retryAt)
			continue
		}

		if budgeted {
			if berr := w.deps.Budgets.Record(ctx, r.Scorer, d.Tenant, d.Subject, now); berr != nil {
				return core.Verdict{}, nil, time.Time{}, false, fmt.Errorf("record budget usage for %s: %w", r.Name, berr)
			}
		}
		if backoff.Attempts > 0 {
			if cerr := w.deps.Store.ClearRuleBackoff(ctx, d.Tenant, d.Subject, r.Name); cerr != nil {
				return core.Verdict{}, nil, time.Time{}, false, fmt.Errorf("clear rule backoff for %s: %w", r.Name, cerr)
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

	// R3 (round 2 fix round): drop every omitted index from calls,
	// outcomes, results and inputHashOverride TOGETHER, in lockstep, right
	// before Combine — everything downstream (Combine's own len(Signals)
	// == len(calls) check, and the records loop below, which re-walks
	// calls by index) must never see an omitted rule at all.
	if n := countTrue(omit); n > 0 {
		filteredCalls := make([]core.Call, 0, len(calls)-n)
		filteredOutcomes := make([]core.RuleOutcome, 0, len(calls)-n)
		filteredResults := make([]*model.ScoreResult, 0, len(calls)-n)
		filteredInputHashOverride := make([]string, 0, len(calls)-n)
		for i := range calls {
			if omit[i] {
				continue
			}
			filteredCalls = append(filteredCalls, calls[i])
			filteredOutcomes = append(filteredOutcomes, outcomes[i])
			filteredResults = append(filteredResults, results[i])
			filteredInputHashOverride = append(filteredInputHashOverride, inputHashOverride[i])
		}
		calls, outcomes, results, inputHashOverride = filteredCalls, filteredOutcomes, filteredResults, filteredInputHashOverride
	}

	verdict := core.Combine(outcomes, core.CombineParams{
		Tiers:                       w.deps.Config.Tiers,
		MinScoredAdvise:             w.deps.Config.MinScoredAdvise,
		TextRulesNeedFeatureSupport: w.deps.Config.TextRulesNeedFeatureSupport,
	}, w.deps.Calibration)

	if len(verdict.Signals) != len(calls) {
		return core.Verdict{}, nil, time.Time{}, false, fmt.Errorf("worker: internal/core.Combine returned %d signals for %d calls", len(verdict.Signals), len(calls))
	}
	if w.deps.Metrics != nil {
		w.deps.Metrics.IncVerdictsByTier(verdict.Tier)
	}

	records := make([]store.VerdictRecord, len(calls))
	for i, call := range calls {
		sig := verdict.Signals[i]
		if sig.Rule != call.Rule.Name {
			return core.Verdict{}, nil, time.Time{}, false, fmt.Errorf("worker: signal/call order mismatch at index %d: got rule %q, want %q", i, sig.Rule, call.Rule.Name)
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
		inputHash := call.InputHash
		if inputHashOverride[i] != "" {
			// B4 fix round: a syncOnly-carried-forward vendor rule records
			// its OLD input_hash, not this round's freshly-computed one —
			// see inputHashOverride's own doc comment above.
			inputHash = inputHashOverride[i]
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
			InputHash:   inputHash,
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

	return verdict, records, nextRescoreAt, syncOnlyDeferred, nil
}
