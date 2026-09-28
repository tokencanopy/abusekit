package serve

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// fixedWindowLimiter is a small, dependency-free per-key rate limiter
// (AGENTS.md's minimal-dependency-footprint convention — this repo pulls
// in no rate-limiting library anywhere else) used for three v0 purposes:
// design §4.4's "evaluate is... rate-limited per subject (1/s)", a
// generous per-KEY request-rate guard on POST /v1/events (design's
// enumerated 429 rate_limited response), and a coarse per-IP guard applied
// BEFORE authentication on every endpoint (S3 fix round — see
// Server.preAuthLimiter's own doc comment). Fixed-window rather than a
// token bucket: simpler, and the burst-at-window-boundary imprecision a
// fixed window allows is immaterial at v0's traffic volumes — documented
// here rather than hidden, since a stricter algorithm would be an easy
// substitution later without changing this type's exported shape.
//
// Single-process, in-memory (v0: "One Go binary" per AGENTS.md) — the
// same assumption internal/serve's replay cache makes. A future
// multi-instance `serve` would need this backed by the store instead.
//
// S3 fix round: expired buckets are swept by a periodic background sweep
// (startSweeper), not inline on every Allow call — proven, an inline sweep
// makes an unauthenticated flood of distinct keys/IPs pay for scanning
// every OTHER distinct key/IP's bucket on every single request, turning
// the "cheap pre-auth guard" into the expensive thing it exists to guard
// against.
type fixedWindowLimiter struct {
	limit  int
	window time.Duration

	mu      sync.Mutex
	buckets map[string]*windowBucket

	stop chan struct{}
}

type windowBucket struct {
	start time.Time
	count int
}

func newFixedWindowLimiter(limit int, window time.Duration) *fixedWindowLimiter {
	return &fixedWindowLimiter{limit: limit, window: window, buckets: make(map[string]*windowBucket)}
}

// Allow reports whether key may proceed at now, and — when it may not —
// how long the caller should wait before retrying (for a Retry-After
// header). Does not sweep — see startSweeper.
func (l *fixedWindowLimiter) Allow(key string, now time.Time) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	b, exists := l.buckets[key]
	if !exists || now.Sub(b.start) >= l.window {
		l.buckets[key] = &windowBucket{start: now, count: 1}
		return true, 0
	}
	if b.count >= l.limit {
		return false, l.window - now.Sub(b.start)
	}
	b.count++
	return true, 0
}

// startSweeper runs a background goroutine that deletes expired buckets
// every interval, until Stop is called. Safe to call at most once per
// limiter (matching Worker.Start's own single-use convention) — Server's
// constructor is the only caller.
func (l *fixedWindowLimiter) startSweeper(interval time.Duration, now func() time.Time) {
	l.stop = make(chan struct{})
	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-l.stop:
				return
			case <-ticker.C:
				n := now()
				l.mu.Lock()
				for k, b := range l.buckets {
					if n.Sub(b.start) >= l.window {
						delete(l.buckets, k)
					}
				}
				l.mu.Unlock()
			}
		}
	}()
}

// Stop ends the background sweeper goroutine. A limiter never
// startSweeper'd (a test constructing one directly) has a nil stop channel
// and this is a safe no-op.
func (l *fixedWindowLimiter) Stop() {
	if l.stop != nil {
		close(l.stop)
	}
}

// clientIP extracts the remote IP (no port) from r, for the coarse
// pre-auth limiter (S3 fix round). Falls back to the raw RemoteAddr string
// if it doesn't parse as host:port — still a usable (if slightly coarser)
// rate-limit key, never a reason to skip limiting entirely.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
