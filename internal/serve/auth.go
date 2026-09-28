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
//
// MaxNonceBytes/MaxNonceHexLen (R5, round 2 fix round) cap a client-
// supplied nonce's length — generous headroom above GenerateNonce's own
// output (MinNonceBytes) for a future increase, while still bounding the
// cost of hex-validating and hashing an adversarially oversized header
// value before authentication has even confirmed the caller holds a real
// key's secret.
const (
	MinNonceBytes  = 16
	MinNonceHexLen = MinNonceBytes * 2
	MaxNonceBytes  = 64
	MaxNonceHexLen = MaxNonceBytes * 2
)

// BackfillEventsTimestampWindow is R5 (round 2 fix round)'s bounded
// replacement for "a backfill-scoped key skips the timestamp-skew check
// entirely" — and ONLY on POST /v1/events, the one endpoint backfill
// exists for at all (design: producers backfilling historical events).
// Every OTHER endpoint uses the ordinary ±TimestampSkew window even for a
// backfill-scoped key: there is no legitimate reason for a backfilled
// timestamp on a read, an evaluate, or a label. An unbounded window on
// EVERY endpoint meant a captured backfill-scoped request signature (for
// ANY endpoint that key could reach) never aged out on timestamp grounds
// at all — only nonce-replay protection stood between it and reuse
// forever.
const BackfillEventsTimestampWindow = 24 * time.Hour

var nonceHexRe = regexp.MustCompile(`^[0-9a-fA-F]+$`)

// TimestampSkew is design §4.3's "Timestamp within ±5 min" auth freshness
// window — how far X-Abusekit-Timestamp may drift from the server's own
// clock before a request is rejected as unauthenticated. A key with the
// `backfill` scope skips this on POST /v1/events specifically, in favour of
// BackfillEventsTimestampWindow (R5, round 2 fix round tightened this from
// "skips the clock-skew check" entirely to that endpoint-scoped, bounded
// window — see BackfillEventsTimestampWindow's own doc comment), matching
// internal/event's own ±24h-vs-unlimited split for the SEPARATE per-event
// `at` timestamp inside a batch body — but see replayExpiry for why a
// wider accepted window needs an equally wide replay window to match.
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
	// RetryAfter is set only for a rate-limited (429) authError (R4, round
	// 2 fix round) — writeAuthError turns it into a Retry-After header,
	// matching writeRateLimited/writeSubjectBusy's own convention.
	RetryAfter time.Duration
}

func (e *authError) Error() string { return e.Message }

func unauthenticated(msg string) *authError {
	return &authError{Status: http.StatusUnauthorized, Code: "unauthenticated", Message: msg}
}

func forbidden(msg string) *authError {
	return &authError{Status: http.StatusForbidden, Code: "forbidden", Message: msg}
}

// rateLimitedAuth is R4 (round 2 fix round)'s response once an IP's
// pre-auth bucket of FAILED authentications is exhausted (see
// authenticate's doc comment) — a 429, not a 401, so a caller can tell
// "back off" from "your credentials are wrong."
func rateLimitedAuth(retryAfter time.Duration) *authError {
	return &authError{Status: http.StatusTooManyRequests, Code: "rate_limited", Message: "too many requests", RetryAfter: retryAfter}
}

// errInvalidCredentials is S10 fix round's single, uniform message for
// EVERY reason a (key, signature) pair might not check out — an unknown
// key id and a wrong signature for a REAL key id must be indistinguishable
// to the caller, or the error text itself becomes a key-id enumeration
// oracle (send garbage signatures against candidate ids; a distinct
// "unknown key" response confirms which ones are real).
//
// R7 (round 2 fix round) widens this from "wrong (key, signature)" to
// EVERY authenticateInner failure that happens before the scope check —
// missing headers, a malformed nonce, an unknown key, a malformed or
// out-of-window timestamp, and a bad signature all now return this exact
// message. S10's own enumeration-oracle reasoning generalizes: a caller
// probing "is this key id real" could otherwise tell an unknown key
// (previously already unified) apart from, say, a real key whose request
// merely had a malformed timestamp — each PREVIOUSLY distinct message
// leaked one more bit about which part of the request an attacker got
// right. The real reason is still fully available, just moved to a
// debug-level log (logAuthFailure) that never reaches the response body —
// an operator debugging a real integration issue still sees exactly what
// went wrong; a caller fishing for information does not. (Replay
// detection is deliberately NOT included here — see its own call site,
// which now runs AFTER the scope check per R5.)
var errInvalidCredentials = unauthenticated("invalid key or signature")

// logAuthFailure records the REAL reason an authenticateInner check
// failed, at debug level only (R7, round 2 fix round) — never in the HTTP
// response, which always sees errInvalidCredentials' uniform message
// instead. reason and any extra args may name which check failed and
// non-secret context (a key id, a header name, a computed delta) but must
// never include the request's own signature or a key's secret.
func (s *Server) logAuthFailure(r *http.Request, reason string, args ...any) {
	attrs := append([]any{"reason", reason, "ip", clientIP(r), "request_id", requestID(r)}, args...)
	s.log().Debug("serve: auth failure", attrs...)
}

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
//
// R5 (round 2 fix round): keyed on a 16-byte SHA-256 digest of "keyID|nonce"
// rather than that raw string — bounds each entry's key to a fixed 16
// bytes regardless of MaxNonceHexLen, which matters most for a
// backfill-scoped key on POST /v1/events: BackfillEventsTimestampWindow's
// 24h retention (vs. TimestampSkew's ~10min for an ordinary key) means
// this cache can accumulate roughly 24h/10min ≈ 144x as many live entries
// for the same request rate, so bounding the PER-ENTRY size is what keeps
// that worst case a fixed, calculable bound (live entries × (16-byte key +
// time.Time value + map overhead)) instead of scaling with however long a
// client's nonce happened to be, up to MaxNonceHexLen. A 16-byte prefix of
// a cryptographic hash is deliberately used over a fast general-purpose
// hash (e.g. FNV) — this is an attacker-facing cache key, and a
// non-cryptographic hash's engineered collisions could let a chosen nonce
// evict or collide with another caller's live entry.
type replayCache struct {
	mu   sync.Mutex
	seen map[[16]byte]time.Time // sha256("keyID|nonce")[:16] -> expiry

	stop chan struct{}
}

func newReplayCache() *replayCache {
	return &replayCache{seen: make(map[[16]byte]time.Time)}
}

// replayCacheKey hashes id ("keyID|nonce") down to a fixed 16 bytes — see
// replayCache's own doc comment for why.
func replayCacheKey(id string) [16]byte {
	sum := sha256.Sum256([]byte(id))
	var k [16]byte
	copy(k[:], sum[:16])
	return k
}

// checkAndRemember returns true (and records id) the first time id is
// seen; returns false on a repeat before its expiry.
//
// T2 (round 3): "before its expiry" is INCLUSIVE of the exact expiry
// instant (!exp.Before(now), not exp.After(now)) — the boundary case a
// plain exp.After(now) got wrong. replayExpiry sets exp to EXACTLY the
// latest instant a replay could still independently pass the timestamp
// accepted-window check (max(received_at, timestamp)+window, itself
// inclusive: authenticateInner rejects only delta > window, so delta ==
// window is accepted) — so at now == exp, a replay of the original
// request is STILL one the accepted-window check would let through. A
// strict exp.After(now) treats the entry as already expired at that exact
// instant, letting checkAndRemember overwrite it and report "not a
// replay" — the one instant where both checks must agree, and previously
// didn't.
func (c *replayCache) checkAndRemember(id string, now, expiry time.Time) bool {
	key := replayCacheKey(id)
	c.mu.Lock()
	defer c.mu.Unlock()
	if exp, ok := c.seen[key]; ok && !exp.Before(now) {
		return false
	}
	c.seen[key] = expiry
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
// (B1 fix round), given the SAME window duration the caller's timestamp
// was just checked against (window is TimestampSkew for an ordinary
// request, or BackfillEventsTimestampWindow for a backfill-scoped key on
// POST /v1/events — R5, round 2 fix round: this is deliberately the exact
// window authenticateInner accepted the timestamp against, not a
// separately-chosen one, so the replay window can never be narrower than
// what the accepted-timestamp check itself allows).
//
// max(now, timestamp)+window, not plain now+window: using max closes a
// real bypass — a request signed with a timestamp already NEAR the future
// edge of the accepted window (say now+window-1m, still valid) would
// otherwise have its replay-cache entry expire only 1 minute after the
// signed timestamp itself — but that timestamp remains independently
// acceptable to the same check for a full `window` past ITS OWN instant,
// so a replay arriving at (original now)+window+1m would pass the
// accepted-window check again AND find an already-expired cache entry,
// defeating replay protection for any request signed near the future edge
// of its window. Anchoring to max(now, timestamp) instead guarantees the
// entry outlives every instant the timestamp could still independently
// pass — this reasoning applies identically whether window is
// TimestampSkew or the much longer BackfillEventsTimestampWindow, which is
// exactly why both now share this one formula instead of the backfill
// case having its own, narrower (now-only) one.
func replayExpiry(now, timestamp time.Time, window time.Duration) time.Time {
	expiry := now.Add(window)
	if tsExpiry := timestamp.Add(window); tsExpiry.After(expiry) {
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
//
// R4 (round 2 fix round): a 401 from authenticateInner — genuinely
// unauthenticated, "anything about WHO is asking" — is charged against
// s.preAuthLimiter's per-IP bucket HERE, after the fact, and may come back
// as a 429 instead once that IP has failed too many times recently. A 403
// (forbidden: a correctly-signed, IDENTIFIED key merely lacking scope)
// never touches the bucket — it's a real, known caller's mistake, not
// anonymous flood traffic. This is also why a request that DOES
// authenticate successfully is never rate-limited by this bucket no
// matter how exhausted it is: success returns straight from
// authenticateInner without ever reaching this check.
func (s *Server) authenticate(r *http.Request, body []byte, requiredScope config.Scope) (authContext, *authError) {
	authCtx, authErr := s.authenticateInner(r, body, requiredScope)
	if authErr == nil || authErr.Status != http.StatusUnauthorized {
		return authCtx, authErr
	}
	if ok, retryAfter := s.preAuthLimiter.Allow(clientIP(r), s.now()); !ok {
		return authContext{}, rateLimitedAuth(retryAfter)
	}
	return authCtx, authErr
}

// authenticateInner is authenticate's actual credential check — split out
// so authenticate (above) can apply the per-IP failure bucket uniformly to
// every 401 this returns, from a single place, rather than each early
// return threading it through individually.
func (s *Server) authenticateInner(r *http.Request, body []byte, requiredScope config.Scope) (authContext, *authError) {
	keyID := r.Header.Get(HeaderKey)
	timestampStr := r.Header.Get(HeaderTimestamp)
	signature := r.Header.Get(HeaderSignature)
	nonce := r.Header.Get(HeaderNonce)
	if keyID == "" || timestampStr == "" || signature == "" || nonce == "" {
		s.logAuthFailure(r, "missing "+HeaderKey+"/"+HeaderTimestamp+"/"+HeaderSignature+"/"+HeaderNonce)
		return authContext{}, errInvalidCredentials
	}
	// R5 (round 2 fix round): bounded on BOTH ends now — MaxNonceHexLen
	// caps the cost of hex-validating/hashing an adversarially oversized
	// header value before the caller has proven anything at all.
	if len(nonce) < MinNonceHexLen || len(nonce) > MaxNonceHexLen || !nonceHexRe.MatchString(nonce) {
		s.logAuthFailure(r, HeaderNonce+" must be "+strconv.Itoa(MinNonceHexLen)+".."+strconv.Itoa(MaxNonceHexLen)+" hex characters")
		return authContext{}, errInvalidCredentials
	}

	key, ok := s.keys[keyID]
	if !ok {
		// S10 fix round: identical to the "signature mismatch" response
		// below — never reveal whether keyID itself exists.
		s.logAuthFailure(r, "unknown key id", "key_id", keyID)
		return authContext{}, errInvalidCredentials
	}

	timestamp, err := time.Parse(time.RFC3339, timestampStr)
	if err != nil {
		s.logAuthFailure(r, HeaderTimestamp+" must be RFC3339", "key_id", keyID)
		return authContext{}, errInvalidCredentials
	}

	// R5 (round 2 fix round): a backfill-scoped key's widened timestamp
	// window applies ONLY to POST /v1/events (requiredScope is that
	// endpoint's own config.ScopeEvents — every other handler passes a
	// different scope) — see BackfillEventsTimestampWindow's own doc
	// comment for why every other endpoint keeps the ordinary skew even
	// for a backfill key. window also feeds replayExpiry below, so the
	// replay-cache entry always covers exactly the window the timestamp
	// was just accepted against.
	now := s.now()
	window := TimestampSkew
	if key.HasScope(config.ScopeBackfill) && requiredScope == config.ScopeEvents {
		window = BackfillEventsTimestampWindow
	}
	delta := now.Sub(timestamp)
	if delta < 0 {
		delta = -delta
	}
	if delta > window {
		s.logAuthFailure(r, "timestamp outside accepted window", "key_id", keyID, "delta", delta.String(), "window", window.String())
		return authContext{}, errInvalidCredentials
	}

	expected := sign(key.Secret, r.Method, r.URL.RequestURI(), timestampStr, keyID, nonce, body)
	// Constant-time comparison (design §4.3) — and on a LENGTH mismatch,
	// hmac.Equal already returns false without a variable-time compare, so
	// no separate length check is needed before it.
	if !hmac.Equal([]byte(strings.ToLower(signature)), []byte(expected)) {
		s.logAuthFailure(r, "signature mismatch", "key_id", keyID)
		return authContext{}, errInvalidCredentials
	}

	// R5 (round 2 fix round): the scope check moved BEFORE replay
	// recording — a request that was always going to be rejected as
	// forbidden (a real, identified key just missing scope, not anonymous
	// flood traffic) never burns its nonce, so a caller can retry the same
	// nonce once properly scoped rather than being permanently told
	// "replay detected" for an attempt that never actually succeeded at
	// anything.
	if requiredScope != "" && !key.HasScope(requiredScope) {
		return authContext{}, forbidden("key lacks required scope " + string(requiredScope))
	}

	if !s.replay.checkAndRemember(keyID+"|"+nonce, now, replayExpiry(now, timestamp, window)) {
		return authContext{}, unauthenticated("replay detected")
	}

	return authContext{Key: key, RequestID: requestID(r)}, nil
}
