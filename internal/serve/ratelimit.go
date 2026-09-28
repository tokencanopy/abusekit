package serve

import (
	"sync"
	"time"
)

// fixedWindowLimiter is a small, dependency-free per-key rate limiter
// (AGENTS.md's minimal-dependency-footprint convention — this repo pulls
// in no rate-limiting library anywhere else) used for two v0 purposes:
// design §4.4's "evaluate is... rate-limited per subject (1/s)", and a
// generous per-key request-rate guard on POST /v1/events (design's
// enumerated 429 rate_limited response). Fixed-window rather than a token
// bucket: simpler, and the burst-at-window-boundary imprecision a fixed
// window allows is immaterial at v0's traffic volumes — documented here
// rather than hidden, since a stricter algorithm would be a easy
// substitution later without changing this type's exported shape.
//
// Single-process, in-memory (v0: "One Go binary" per AGENTS.md) — the
// same assumption internal/serve's replay cache makes. A future
// multi-instance `serve` would need this backed by the store instead.
type fixedWindowLimiter struct {
	limit  int
	window time.Duration

	mu      sync.Mutex
	buckets map[string]*windowBucket
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
// header). Sweeps expired buckets opportunistically, same tradeoff as
// replayCache.checkAndRemember: O(buckets) per call, bounded in practice
// by v0's very small key/subject cardinality.
func (l *fixedWindowLimiter) Allow(key string, now time.Time) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	for k, b := range l.buckets {
		if now.Sub(b.start) >= l.window {
			delete(l.buckets, k)
		}
	}

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
