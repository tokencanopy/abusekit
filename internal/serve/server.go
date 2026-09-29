package serve

import (
	"context"
	"crypto/rand"
	"log/slog"
	"net/http"
	"time"

	"github.com/tokencanopy/abusekit/internal/config"
	"github.com/tokencanopy/abusekit/internal/feature"
	"github.com/tokencanopy/abusekit/internal/store"
	"github.com/tokencanopy/abusekit/internal/worker"
)

// MaxEventBatchBody is design §4.3's "body ≤1 MiB" for POST /v1/events.
const MaxEventBatchBody = 1 << 20

// MaxOtherBody bounds every OTHER endpoint's request body — design only
// specifies a limit for the events batch, but an unbounded body on any
// endpoint is an availability risk regardless; 64 KiB comfortably fits a
// label note (≤500 bytes, design §4.9) or an evaluate deadline_ms request
// with room to spare.
const MaxOtherBody = 64 << 10

// DefaultEventsPerKeyPerSecond is a v0 placeholder per-key request-rate
// guard on POST /v1/events (design's enumerated 429 rate_limited
// response), checked AFTER authentication (S3 fix round — see New's own
// comment on why) — generous enough not to bother a real producer at v0
// traffic volumes, deliberately round pending real data, matching this
// repo's other placeholder budget defaults (cmd/abusekit's
// defaultPerAdapterDailyBudget/defaultPerTenantDailyBudget).
const DefaultEventsPerKeyPerSecond = 50

// DefaultPreAuthPerIPPerSecond is S3 fix round's coarse, cheap guard,
// keyed on the remote IP — independent of (and much more generous than)
// any per-key limit that only applies once a request actually
// authenticates.
//
// R4 (round 2 fix round): consumed ONLY on a FAILED authentication
// (authenticate's own 401 path — see authenticate's doc comment), never on
// a successful one. Proven necessary: gating every request BEFORE
// authenticate ran at all — this limiter's original S3 shape — meant an
// IP that had racked up a bucket's worth of garbage/replayed requests
// could no longer authenticate as a REAL producer sharing that IP (a NAT
// gateway, a corporate proxy) even with a perfectly valid signed request,
// since the 429 fired before the signature was ever checked. Counting
// only failures turns this into a fail2ban-style guard against floods
// of UNAUTHENTICATED traffic specifically, while a request that would
// actually authenticate is never touched by it, no matter how exhausted
// that IP's bucket is.
const DefaultPreAuthPerIPPerSecond = 200

// EvaluateRateLimit is design §4.4's "rate-limited per subject (1/s)".
const EvaluateRateLimit = time.Second

// limiterSweepInterval is how often the rate limiters and replay cache
// sweep their expired entries (S3 fix round: a periodic background sweep,
// not one done inline on every request — see fixedWindowLimiter's own doc
// comment for why).
const limiterSweepInterval = time.Minute

// Deps are everything Server needs. Store, Worker, Config and Keys are
// required; everything else has a documented default — matching
// internal/worker.Deps' own convention.
type Deps struct {
	Store  *store.Store
	Worker *worker.Worker
	Config *config.Config
	Keys   map[string]config.Key

	// Neighbors, Brands and Webmail feed the label handler's corpus-
	// snapshot feature extraction (design §4.9) — the SAME values
	// cmd/abusekit wires into the worker's own Deps, so a labelled
	// subject's stored features match what the worker would have
	// computed for it.
	Neighbors feature.Neighbors
	Brands    feature.BrandSet
	Webmail   feature.WebmailSet

	// Now returns the current time; nil uses time.Now().UTC(). Tests
	// inject a fixed clock for deterministic signature/skew and
	// rate-limit assertions.
	Now func() time.Time
	// Logger receives request-handling failures. nil uses slog.Default().
	Logger *slog.Logger

	// EventsPerKeyPerSecond overrides DefaultEventsPerKeyPerSecond. <= 0
	// uses the default.
	EventsPerKeyPerSecond int
	// PreAuthPerIPPerSecond overrides DefaultPreAuthPerIPPerSecond. <= 0
	// uses the default.
	PreAuthPerIPPerSecond int

	// CorpusSplitSecret keys the HMAC internal/serve uses to assign a
	// labelled subject's corpus_examples row to "train" or "test" (N1 fix
	// round: a bare, unkeyed hash would let anyone who can guess/enumerate
	// subject ids predict, and therefore game, which split they land in).
	// Empty generates a random one at construction — reproducible WITHIN
	// one running process, but not stable across a restart; a deployment
	// that wants a stable split across restarts (e.g. for corpus/harness
	// reproducibility, S4) should supply one from a real secret store.
	CorpusSplitSecret []byte
}

// Server holds abusekit's HTTP handlers' dependencies. Construct with New;
// the zero value is not usable. Server is safe for concurrent use — every
// handler is stateless beyond the shared, mutex-guarded replay/rate-limit
// caches. Call Close when done with it to stop the background sweep
// goroutines New starts.
type Server struct {
	store  *store.Store
	worker *worker.Worker
	cfg    *config.Config
	keys   map[string]config.Key

	neighbors feature.Neighbors
	brands    feature.BrandSet
	webmail   feature.WebmailSet

	nowFn  func() time.Time
	logger *slog.Logger

	replay         *replayCache
	evalLimiter    *fixedWindowLimiter
	eventsLimiter  *fixedWindowLimiter
	preAuthLimiter *fixedWindowLimiter

	corpusSplitSecret []byte
}

// New validates deps, returns a Server, and starts its background sweep
// goroutines (replay cache, rate limiters) — call Close to stop them.
func New(deps Deps) (*Server, error) {
	if deps.Store == nil {
		return nil, errRequired("Deps.Store")
	}
	if deps.Worker == nil {
		return nil, errRequired("Deps.Worker")
	}
	if deps.Config == nil {
		return nil, errRequired("Deps.Config")
	}
	if deps.Keys == nil {
		return nil, errRequired("Deps.Keys")
	}
	if deps.Neighbors == nil {
		deps.Neighbors = feature.NoNeighbors
	}
	eventsPerSec := deps.EventsPerKeyPerSecond
	if eventsPerSec <= 0 {
		eventsPerSec = DefaultEventsPerKeyPerSecond
	}
	preAuthPerSec := deps.PreAuthPerIPPerSecond
	if preAuthPerSec <= 0 {
		preAuthPerSec = DefaultPreAuthPerIPPerSecond
	}
	corpusSplitSecret := deps.CorpusSplitSecret
	if len(corpusSplitSecret) == 0 {
		corpusSplitSecret = make([]byte, 32)
		if _, err := rand.Read(corpusSplitSecret); err != nil {
			return nil, err
		}
	}

	nowFn := deps.Now
	now := func() time.Time {
		if nowFn != nil {
			return nowFn()
		}
		return time.Now().UTC()
	}

	s := &Server{
		store:          deps.Store,
		worker:         deps.Worker,
		cfg:            deps.Config,
		keys:           deps.Keys,
		neighbors:      deps.Neighbors,
		brands:         deps.Brands,
		webmail:        deps.Webmail,
		nowFn:          deps.Now,
		logger:         deps.Logger,
		replay:         newReplayCache(),
		evalLimiter:    newFixedWindowLimiter(1, EvaluateRateLimit),
		eventsLimiter:  newFixedWindowLimiter(eventsPerSec, time.Second),
		preAuthLimiter: newFixedWindowLimiter(preAuthPerSec, time.Second),

		corpusSplitSecret: corpusSplitSecret,
	}
	s.replay.startSweeper(limiterSweepInterval, now)
	s.evalLimiter.startSweeper(limiterSweepInterval, now)
	s.eventsLimiter.startSweeper(limiterSweepInterval, now)
	s.preAuthLimiter.startSweeper(limiterSweepInterval, now)
	return s, nil
}

// Close stops the background sweep goroutines New started. Safe to call
// once; not required for correctness (the goroutines hold no resources
// that leak beyond process exit), but good hygiene for a test or a
// short-lived Server, and for cmd/abusekit's own graceful shutdown.
func (s *Server) Close() {
	s.replay.Stop()
	s.evalLimiter.Stop()
	s.eventsLimiter.Stop()
	s.preAuthLimiter.Stop()
}

func (s *Server) now() time.Time {
	if s.nowFn != nil {
		return s.nowFn()
	}
	return time.Now().UTC()
}

func (s *Server) log() *slog.Logger {
	if s.logger != nil {
		return s.logger
	}
	return slog.Default()
}

type requiredErr string

func (e requiredErr) Error() string  { return string(e) + " is required" }
func errRequired(field string) error { return requiredErr(field) }

// Handler returns the full routed HTTP handler (Go 1.22+ ServeMux method+
// path-parameter patterns — no router dependency, matching AGENTS.md's
// minimal-footprint convention). Middleware order (outermost first):
// withRequestID (every response, including a 404/429 before any handler
// runs, gets a resolved X-Request-Id) → recoverMiddleware (S7 fix round: a
// panicking handler becomes a clean 500 envelope, never a raw stack trace
// or a bare connection drop) → the mux itself.
//
// R4 (round 2 fix round): the coarse per-IP pre-auth limiter that used to
// wrap the mux HERE, gating every request before any handler (and
// therefore before authenticate ran at all), is gone — see
// DefaultPreAuthPerIPPerSecond's doc comment for why. It's now consulted
// from inside authenticate itself, only once a request has already failed
// to authenticate.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/events", s.handleEvents)
	mux.HandleFunc("GET /v1/subjects/{subject}", s.handleGetSubject)
	mux.HandleFunc("POST /v1/subjects/{subject}/evaluate", s.handleEvaluate)
	mux.HandleFunc("POST /v1/labels", s.handleLabels)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("/", s.handleNotFound)

	return withRequestID(s.recoverMiddleware(mux))
}

// handleNotFound is the catch-all for any path the mux itself didn't
// match (an unknown route, or a known path with an unsupported method —
// ServeMux's own method-mismatch 405 is bypassed by using a single
// catch-all pattern set per method above; a path that exists under a
// DIFFERENT method falls through to this, reported as 404 not_found
// rather than leaking which methods a path supports).
func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, http.StatusNotFound, "not_found", "no such route", nil)
}

// withRequestID resolves X-Request-Id (design: "accepted/generated and
// returned") once per request, before any handler or auth check runs, and
// stashes it on the request context so writeJSON/writeError (which run on
// every response path, including an auth failure before a handler even
// starts) can always echo the SAME value.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(HeaderRequestID)
		if !requestIDRe.MatchString(id) {
			id = generateRequestID()
		}
		ctx := context.WithValue(r.Context(), requestIDCtxKey, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
