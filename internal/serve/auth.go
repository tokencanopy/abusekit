// Package serve is abusekit's HTTP surface (design's "serve" module,
// §4.1: "HTTP handlers, auth, error envelope — shallow by design"): signed
// key auth, the event/score/evaluate/label/erasure endpoints, and the one
// error envelope every failure uses. Handlers stay thin — the actual work
// is store reads/writes and internal/worker.EvaluateSubject; this package
// exists to translate HTTP into those calls and their results back into
// HTTP, not to hold business logic of its own.
//
// TODO(design §4.4, plan.md's v0 scope): the `subject.tier_changed`
// webhook is explicitly deferred to v1 ("Out (v1): ... webhooks") and is
// NOT implemented anywhere in this package — no tier-change detection, no
// per-tenant target URL/secret config, no retry schedule. A future slice
// adding it would hook into UpsertVerdicts' tier transition (comparing the
// previous and new current_tier) and reuse this package's own signing
// scheme for the outbound POST.
package serve

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tokencanopy/abusekit/internal/config"
)

// Header names for design §4.3's signed-request scheme.
const (
	HeaderKey       = "X-Abusekit-Key"
	HeaderTimestamp = "X-Abusekit-Timestamp"
	HeaderSignature = "X-Abusekit-Signature"
	HeaderRequestID = "X-Request-Id"
)

// TimestampSkew is design §4.3's "Timestamp within ±5 min" auth freshness
// window — how far X-Abusekit-Timestamp may drift from the server's own
// clock before a request is rejected as unauthenticated. A key with the
// `backfill` scope skips this check entirely (design: "backfill skips the
// clock-skew check"), matching internal/event's own ±24h-vs-unlimited
// split for the SEPARATE per-event `at` timestamp inside a batch body.
const TimestampSkew = 5 * time.Minute

// authContext is what a successfully authenticated request carries to its
// handler: which key was used (and therefore its tenant/producer/scopes),
// plus the request id for logging/response echoing.
type authContext struct {
	Key       config.Key
	RequestID string
}

// authError is authenticate's structured failure: an envelope Code plus
// the HTTP status to send. Kept distinct from apiError (envelope.go) only
// because authenticate runs before a handler decides its own error
// shape — writeAPIError renders either one identically.
type authError struct {
	Status  int
	Code    string
	Message string
}

func (e *authError) Error() string { return e.Message }

func unauthenticated(msg string) *authError {
	return &authError{Status: http.StatusUnauthorized, Code: "unauthenticated", Message: msg}
}

func forbidden(msg string) *authError {
	return &authError{Status: http.StatusForbidden, Code: "forbidden", Message: msg}
}

// replayCache remembers (key id, timestamp, signature) triples already
// seen within TimestampSkew, so a captured valid request can't be
// replayed verbatim while its timestamp is still within the freshness
// window design's own skew check alone would otherwise still accept.
// This is a single-process, in-memory cache (v0: "One Go binary" per
// AGENTS.md — the same single-process assumption internal/serve's
// evaluate rate limiter makes) — a future multi-instance `serve` would
// need this to live in the store instead; documented as a known v0
// limitation, not attempted here.
//
// A backfill-scoped key (which also skips the timestamp-skew check
// entirely) is NOT exempted from replay detection: a leaked backfill
// credential replaying the exact same signed request is exactly the
// scenario this cache exists to catch, arguably more so given backfill's
// widened timestamp tolerance.
type replayCache struct {
	mu   sync.Mutex
	seen map[string]time.Time // signature -> first-seen expiry
}

func newReplayCache() *replayCache {
	return &replayCache{seen: make(map[string]time.Time)}
}

// checkAndRemember returns true (and records sig) the first time sig is
// seen; returns false on a repeat before its expiry. Expired entries are
// swept opportunistically on every call, which is Θ(seen) rather than
// O(1) — deliberately not optimized further given v0's very small key
// count and TimestampSkew's short window keep this bounded in practice.
func (c *replayCache) checkAndRemember(sig string, now, expiry time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, exp := range c.seen {
		if !exp.After(now) {
			delete(c.seen, k)
		}
	}
	if exp, ok := c.seen[sig]; ok && exp.After(now) {
		return false
	}
	c.seen[sig] = expiry
	return true
}

// canonicalString builds design §4.3's signing payload:
// method \n path?query \n timestamp \n key_id \n sha256(body). path?query
// is the request's exact request-target (path, plus "?query" only when a
// query string is present) — the same bytes r.URL.RequestURI() reports
// for an incoming request, so a client signs precisely what it sent on
// the wire.
func canonicalString(method, requestURI, timestamp, keyID string, bodyHash [32]byte) string {
	return method + "\n" + requestURI + "\n" + timestamp + "\n" + keyID + "\n" + hex.EncodeToString(bodyHash[:])
}

// sign computes design §4.3's hex(hmac-sha256(secret, ...)).
func sign(secret, method, requestURI, timestamp, keyID string, body []byte) string {
	bodyHash := sha256.Sum256(body)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(canonicalString(method, requestURI, timestamp, keyID, bodyHash)))
	return hex.EncodeToString(mac.Sum(nil))
}

// authenticate validates a request's signed-auth headers against s.keys
// and s.replay, returning the matched key's authContext or a structured
// authError (401 for anything about WHO is asking, 403 only for a
// correctly-authenticated key lacking a required scope — design's own
// 401-vs-403 split). It does not read the body itself; callers that need
// the body (events) pass it in, since the signature must cover the exact
// bytes a handler goes on to decode.
func (s *Server) authenticate(r *http.Request, body []byte, requiredScope config.Scope) (authContext, *authError) {
	keyID := r.Header.Get(HeaderKey)
	timestampStr := r.Header.Get(HeaderTimestamp)
	signature := r.Header.Get(HeaderSignature)
	if keyID == "" || timestampStr == "" || signature == "" {
		return authContext{}, unauthenticated("missing " + HeaderKey + "/" + HeaderTimestamp + "/" + HeaderSignature)
	}

	key, ok := s.keys[keyID]
	if !ok {
		return authContext{}, unauthenticated("unknown key")
	}

	timestamp, err := time.Parse(time.RFC3339, timestampStr)
	if err != nil {
		return authContext{}, unauthenticated(HeaderTimestamp + " must be RFC3339")
	}

	now := s.now()
	if !key.HasScope(config.ScopeBackfill) {
		delta := now.Sub(timestamp)
		if delta < 0 {
			delta = -delta
		}
		if delta > TimestampSkew {
			return authContext{}, unauthenticated("timestamp is outside the ±5min skew window")
		}
	}

	expected := sign(key.Secret, r.Method, r.URL.RequestURI(), timestampStr, keyID, body)
	// Constant-time comparison (design §4.3) — and on a LENGTH mismatch,
	// hmac.Equal already returns false without a variable-time compare, so
	// no separate length check is needed before it.
	if !hmac.Equal([]byte(strings.ToLower(signature)), []byte(expected)) {
		return authContext{}, unauthenticated("signature mismatch")
	}

	if !s.replay.checkAndRemember(keyID+"|"+timestampStr+"|"+expected, now, now.Add(TimestampSkew)) {
		return authContext{}, unauthenticated("replay detected")
	}

	if requiredScope != "" && !key.HasScope(requiredScope) {
		return authContext{}, forbidden("key lacks required scope " + string(requiredScope))
	}

	return authContext{Key: key, RequestID: requestID(r)}, nil
}
