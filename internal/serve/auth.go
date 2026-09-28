// Package serve is abusekit's HTTP surface (design's "serve" module,
// §4.1: "HTTP handlers, auth, error envelope — shallow by design"): signed
// key auth, the event/score/evaluate/label endpoints, and the one error
// envelope every failure uses. Handlers stay thin — the actual work is
// store reads/writes and internal/worker.EvaluateSubject; this package
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
//
// TODO(S12 fix round): evaluate currently requires only the `read` scope
// (design frames it as a normal product-facing capability, called
// routinely before a send — see internal/worker.EvaluateSubject's own doc
// comment). Before any vendor scorer becomes sync-eligible (today,
// syncOnly restricts live execution to "local" regardless of scope), this
// needs its own narrower scope: a `read` key currently could trigger a
// real vendor API call's cost/latency the moment evaluate stops being
// local-only, which a read-only credential shouldn't be able to do on its
// own. Revisit when S5's vendor adapters land.
package serve

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"regexp"
	"strconv"
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
	// HeaderNonce is B1 fix round's addition to design §4.3/§4.2: a
	// per-request random value, signed alongside everything else, so two
	// requests that would otherwise be byte-for-byte identical (same
	// method/path/timestamp/key/body — e.g. two legitimate calls issued
	// within the same wall-clock second, or a client's own retry of a
	// request that failed after the server had already processed it) get
	// DIFFERENT signatures and are never confused with a replay of each
	// other. Required on every request, including a backfill-scoped key's
	// (design amendment: backfill's widened timestamp tolerance makes
	// nonce-based replay protection the ONLY thing closing that window —
	// see replayExpiry).
	HeaderNonce = "X-Abusekit-Nonce"
	// HeaderRequestID is accepted/generated per design and always echoed.
	HeaderRequestID = "X-Request-Id"
)

// MinNonceBytes is design §4.2's amended "≥16 random bytes" for
// X-Abusekit-Nonce, hex-encoded on the wire (so MinNonceHexLen is double).
const (
	MinNonceBytes  = 16
	MinNonceHexLen = MinNonceBytes * 2
)

var nonceHexRe = regexp.MustCompile(`^[0-9a-fA-F]+$`)

// TimestampSkew is design §4.3's "Timestamp within ±5 min" auth freshness
// window — how far X-Abusekit-Timestamp may drift from the server's own
// clock before a request is rejected as unauthenticated. A key with the
// `backfill` scope skips this check entirely (design: "backfill skips the
// clock-skew check"), matching internal/event's own ±24h-vs-unlimited
// split for the SEPARATE per-event `at` timestamp inside a batch body —
// but see replayExpiry for why backfill's REPLAY window is intentionally
// much longer than TimestampSkew itself.
const TimestampSkew = 5 * time.Minute

// backfillNonceRetention is B1 fix round: a backfill-scoped key skips the
// timestamp-skew check entirely, so there is no timestamp-derived bound on
// how long a captured backfill request stays "fresh" the way there is for
// an ordinary key — its own signed timestamp could be anything. Anchoring
// its replay-cache expiry to the RECEIVE time (now) instead, for a much
// longer window than TimestampSkew, is what actually makes a backfill
// replay "impossible" past a few minutes rather than merely past
// TimestampSkew — 24h matches this repo's other backfill-adjacent
// tolerance (internal/event's own ±24h non-backfill skew).
const backfillNonceRetention = 24 * time.Hour

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

// errInvalidCredentials is S10 fix round's single, uniform message for
// EVERY reason a (key, signature) pair might not check out — an unknown
// key id and a wrong signature for a REAL key id must be indistinguishable
// to the caller, or the error text itself becomes a key-id enumeration
// oracle (send garbage signatures against candidate ids; a distinct
// "unknown key" response confirms which ones are real).
var errInvalidCredentials = unauthenticated("invalid key or signature")

// replayCache remembers (key id, nonce) pairs already seen, so a captured
// valid request (or a client's own naive retry of one) can never be
// replayed verbatim — B1 fix round: keyed on the NONCE now (design
// amendment §4.2), not the signature, since the nonce is the field
// specifically designed to make two otherwise-identical requests
// distinguishable. This is a single-process, in-memory cache (v0: "One Go
// binary" per AGENTS.md — the same single-process assumption
// internal/serve's rate limiters make) — a future multi-instance `serve`
// would need this to live in the store instead; documented as a known v0
// limitation, not attempted here.
//
// S3 fix round: expired entries are swept by a periodic background sweep
// (startSweeper), not inline on every check — see fixedWindowLimiter's own
// doc comment for why an inline sweep is the wrong tradeoff here.
type replayCache struct {
	mu   sync.Mutex
	seen map[string]time.Time // "keyID|nonce" -> expiry

	stop chan struct{}
}

func newReplayCache() *replayCache {
	return &replayCache{seen: make(map[string]time.Time)}
}

// checkAndRemember returns true (and records id) the first time id is
// seen; returns false on a repeat before its expiry.
func (c *replayCache) checkAndRemember(id string, now, expiry time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if exp, ok := c.seen[id]; ok && exp.After(now) {
		return false
	}
	c.seen[id] = expiry
	return true
}

// startSweeper mirrors fixedWindowLimiter's own — see its doc comment.
func (c *replayCache) startSweeper(interval time.Duration, now func() time.Time) {
	c.stop = make(chan struct{})
	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-c.stop:
				return
			case <-ticker.C:
				n := now()
				c.mu.Lock()
				for k, exp := range c.seen {
					if !exp.After(n) {
						delete(c.seen, k)
					}
				}
				c.mu.Unlock()
			}
		}
	}()
}

func (c *replayCache) Stop() {
	if c.stop != nil {
		close(c.stop)
	}
}

// replayExpiry computes how long a (key, nonce) pair must be remembered
// (B1 fix round). For an ordinary key, max(now, timestamp)+TimestampSkew:
// using max rather than plain now+TimestampSkew closes a real bypass —
// a request signed with a timestamp already NEAR the future edge of the
// skew window (say now+4m, still valid) would otherwise have its replay-
// cache entry expire at now+5m, i.e. only 1 minute after the signed
// timestamp itself — but that timestamp remains independently acceptable
// to the skew check for a full 5 minutes past ITS OWN instant, so a replay
// arriving at (original now)+5m01s would pass the skew check again AND
// find an already-expired cache entry, defeating replay protection for
// any request signed slightly ahead of its send time. Anchoring to
// max(now, timestamp) instead guarantees the entry outlives every instant
// the timestamp could still independently pass.
//
// A backfill-scoped key skips the timestamp-skew check entirely, so there
// is no timestamp-derived bound to anchor to at all — it gets
// backfillNonceRetention from receive time instead (see that constant's
// own doc comment).
func replayExpiry(key config.Key, now, timestamp time.Time) time.Time {
	if key.HasScope(config.ScopeBackfill) {
		return now.Add(backfillNonceRetention)
	}
	expiry := now.Add(TimestampSkew)
	if tsExpiry := timestamp.Add(TimestampSkew); tsExpiry.After(expiry) {
		expiry = tsExpiry
	}
	return expiry
}

// canonicalString builds design §4.3's signing payload (B1 fix round adds
// nonce): method \n path?query \n timestamp \n key_id \n nonce \n
// sha256(body). path?query is the request's exact request-target (path,
// plus "?query" only when a query string is present) — the same bytes
// r.URL.RequestURI() reports for an incoming request, so a client signs
// precisely what it sent on the wire.
func canonicalString(method, requestURI, timestamp, keyID, nonce string, bodyHash [32]byte) string {
	return method + "\n" + requestURI + "\n" + timestamp + "\n" + keyID + "\n" + nonce + "\n" + hex.EncodeToString(bodyHash[:])
}

// sign computes design §4.3's hex(hmac-sha256(secret, ...)).
func sign(secret, method, requestURI, timestamp, keyID, nonce string, body []byte) string {
	bodyHash := sha256.Sum256(body)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(canonicalString(method, requestURI, timestamp, keyID, nonce, bodyHash)))
	return hex.EncodeToString(mac.Sum(nil))
}

// GenerateNonce returns a fresh, hex-encoded random nonce (MinNonceBytes
// of entropy) — exported for pkg/abusekit's client, which must generate a
// NEW one per attempt (B1 fix round: a client's own retry with an IDENTICAL
// nonce would be indistinguishable from a genuine replay to the server).
func GenerateNonce() (string, error) {
	b := make([]byte, MinNonceBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
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
	nonce := r.Header.Get(HeaderNonce)
	if keyID == "" || timestampStr == "" || signature == "" || nonce == "" {
		return authContext{}, unauthenticated("missing " + HeaderKey + "/" + HeaderTimestamp + "/" + HeaderSignature + "/" + HeaderNonce)
	}
	if len(nonce) < MinNonceHexLen || !nonceHexRe.MatchString(nonce) {
		return authContext{}, unauthenticated(HeaderNonce + " must be at least " + strconv.Itoa(MinNonceHexLen) + " hex characters")
	}

	key, ok := s.keys[keyID]
	if !ok {
		// S10 fix round: identical to the "signature mismatch" response
		// below — never reveal whether keyID itself exists.
		return authContext{}, errInvalidCredentials
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

	expected := sign(key.Secret, r.Method, r.URL.RequestURI(), timestampStr, keyID, nonce, body)
	// Constant-time comparison (design §4.3) — and on a LENGTH mismatch,
	// hmac.Equal already returns false without a variable-time compare, so
	// no separate length check is needed before it.
	if !hmac.Equal([]byte(strings.ToLower(signature)), []byte(expected)) {
		return authContext{}, errInvalidCredentials
	}

	if !s.replay.checkAndRemember(keyID+"|"+nonce, now, replayExpiry(key, now, timestamp)) {
		return authContext{}, unauthenticated("replay detected")
	}

	if requiredScope != "" && !key.HasScope(requiredScope) {
		return authContext{}, forbidden("key lacks required scope " + string(requiredScope))
	}

	return authContext{Key: key, RequestID: requestID(r)}, nil
}
