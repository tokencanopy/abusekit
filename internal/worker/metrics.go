package worker

import (
	"expvar"
	"sync"
	"time"
)

// Metrics collects design §4.8's runtime counters (S8 fix round: "queue
// depth, oldest dirty age, per-adapter latency/cost/errors, verdicts by
// tier, budget denials"). A Worker with nil Deps.Metrics simply doesn't
// record anything — the correct default for a short-lived test.
//
// Exposed via expvar (the standard library's own metrics registry: zero
// new dependencies, and any future HTTP surface (S3) can publish it at
// /debug/vars — or read Snapshot directly — without anything here
// changing) rather than a Prometheus client, matching this repo's stated
// preference for a minimal dependency footprint (Makefile: "No external
// lint tool is pulled in for S1, keeping the module's dependency
// footprint minimal").
//
// Safe for concurrent use.
type Metrics struct {
	mu sync.Mutex

	queueDepth     int
	oldestDirtyAge time.Duration
	subjectsFailed int64
	verdictsByTier map[string]int64
	adapterCalls   map[string]int64
	adapterErrors  map[string]int64
	adapterLatency map[string]time.Duration // running total; Snapshot divides by adapterCalls for a mean
	budgetDenials  map[string]int64
}

// NewMetrics returns an empty Metrics.
func NewMetrics() *Metrics {
	return &Metrics{
		verdictsByTier: make(map[string]int64),
		adapterCalls:   make(map[string]int64),
		adapterErrors:  make(map[string]int64),
		adapterLatency: make(map[string]time.Duration),
		budgetDenials:  make(map[string]int64),
	}
}

func (m *Metrics) SetQueueDepth(depth int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queueDepth = depth
}

func (m *Metrics) SetOldestDirtyAge(age time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.oldestDirtyAge = age
}

func (m *Metrics) IncSubjectsFailed() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.subjectsFailed++
}

func (m *Metrics) IncVerdictsByTier(tier string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.verdictsByTier[tier]++
}

func (m *Metrics) IncAdapterCalls(scorer string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.adapterCalls[scorer]++
}

func (m *Metrics) IncAdapterErrors(scorer string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.adapterErrors[scorer]++
}

func (m *Metrics) ObserveAdapterLatency(scorer string, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.adapterLatency[scorer] += d
}

func (m *Metrics) IncBudgetDenials(scorer string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.budgetDenials[scorer]++
}

// Snapshot is a point-in-time, immutable copy of Metrics for exposition or
// assertion.
type Snapshot struct {
	QueueDepth         int
	OldestDirtyAge     time.Duration
	SubjectsFailed     int64
	VerdictsByTier     map[string]int64
	AdapterCalls       map[string]int64
	AdapterErrors      map[string]int64
	AdapterMeanLatency map[string]time.Duration
	BudgetDenials      map[string]int64
}

// Snapshot returns a copy of m's current state.
func (m *Metrics) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()

	meanLatency := make(map[string]time.Duration, len(m.adapterLatency))
	for scorer, total := range m.adapterLatency {
		calls := m.adapterCalls[scorer]
		if calls > 0 {
			meanLatency[scorer] = total / time.Duration(calls)
		}
	}

	return Snapshot{
		QueueDepth:         m.queueDepth,
		OldestDirtyAge:     m.oldestDirtyAge,
		SubjectsFailed:     m.subjectsFailed,
		VerdictsByTier:     copyCounts(m.verdictsByTier),
		AdapterCalls:       copyCounts(m.adapterCalls),
		AdapterErrors:      copyCounts(m.adapterErrors),
		AdapterMeanLatency: meanLatency,
		BudgetDenials:      copyCounts(m.budgetDenials),
	}
}

func copyCounts(src map[string]int64) map[string]int64 {
	dst := make(map[string]int64, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// Publish registers m under name in expvar's default (process-wide)
// registry, so a future HTTP surface serving /debug/vars — or any other
// expvar consumer — picks it up automatically. Panics if name is already
// published (expvar's own behavior) — call this at most once per process,
// typically right after constructing the Metrics a Worker will use.
func (m *Metrics) Publish(name string) {
	expvar.Publish(name, expvar.Func(func() any { return m.Snapshot() }))
}
