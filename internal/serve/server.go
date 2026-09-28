package serve

import (
	"context"
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
// response) — generous enough not to bother a real producer at v0
// traffic volumes, deliberately round pending real data, matching this
// repo's other placeholder budget defaults (cmd/abusekit's
// defaultPerAdapterDailyBudget/defaultPerTenantDailyBudget).
const DefaultEventsPerKeyPerSecond = 50

// EvaluateRateLimit is design §4.4's "rate-limited per subject (1/s)".
const EvaluateRateLimit = time.Second

// Deps are everything Server needs. Store, Worker, Config and Keys are
// required; everything else has a documented default — matching
// internal/worker.Deps' own convention.
type Deps struct {
	Store  *store.Store
	Worker *worker.Worker
	Config *config.Config
	Keys   map[string]config.Key

	// Neighbors and Brands feed the label handler's corpus-snapshot
	// feature extraction (design §4.9) — the SAME values cmd/abusekit
	// wires into the worker's own Deps, so a labelled subject's stored
	// features match what the worker would have computed for it.
	Neighbors feature.Neighbors
	Brands    feature.BrandSet

	// Now returns the current time; nil uses time.Now().UTC(). Tests
	// inject a fixed clock for deterministic signature/skew and
	// rate-limit assertions.
	Now func() time.Time
	// Logger receives request-handling failures. nil uses slog.Default().
	Logger *slog.Logger

	// EventsPerKeyPerSecond overrides DefaultEventsPerKeyPerSecond. <= 0
	// uses the default.
	EventsPerKeyPerSecond int
}

// Server holds abusekit's HTTP handlers' dependencies. Construct with New;
// the zero value is not usable. Server is safe for concurrent use — every
// handler is stateless beyond the shared, mutex-guarded replay/rate-limit
// caches.
type Server struct {
	store  *store.Store
	worker *worker.Worker
	cfg    *config.Config
	keys   map[string]config.Key

	neighbors feature.Neighbors
	brands    feature.BrandSet

	nowFn  func() time.Time
	logger *slog.Logger

	replay        *replayCache
	evalLimiter   *fixedWindowLimiter
	eventsLimiter *fixedWindowLimiter
}

// New validates deps and returns a Server.
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

	return &Server{
		store:         deps.Store,
		worker:        deps.Worker,
		cfg:           deps.Config,
		keys:          deps.Keys,
		neighbors:     deps.Neighbors,
		brands:        deps.Brands,
		nowFn:         deps.Now,
		logger:        deps.Logger,
		replay:        newReplayCache(),
		evalLimiter:   newFixedWindowLimiter(1, EvaluateRateLimit),
		eventsLimiter: newFixedWindowLimiter(eventsPerSec, time.Second),
	}, nil
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
// minimal-footprint convention). withRequestID wraps every route so every
// response, including one from a route that doesn't match at all, gets a
// resolved X-Request-Id.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/events", s.handleEvents)
	mux.HandleFunc("GET /v1/subjects", s.handleListSubjects)
	mux.HandleFunc("GET /v1/subjects/{subject}", s.handleGetSubject)
	mux.HandleFunc("POST /v1/subjects/{subject}/evaluate", s.handleEvaluate)
	mux.HandleFunc("DELETE /v1/subjects/{subject}", s.handleEraseSubject)
	mux.HandleFunc("POST /v1/labels", s.handleLabels)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("/", s.handleNotFound)

	return withRequestID(mux)
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
