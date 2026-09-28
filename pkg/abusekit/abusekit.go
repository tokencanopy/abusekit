// Package abusekit is a small, dependency-free Go client for abusekit's
// HTTP surface (design §4.1's "importable pkg/abusekit client"). It has no
// import of anything under internal/ — a public SDK is deliberately
// decoupled from the server's own wire types, so a change to internal/event
// or internal/core never forces a client API break, and the reverse: this
// package's own types are free to evolve on the client's own schedule as
// long as the WIRE contract (documented in docs/design's §4.3/§4.2 and this
// repo's README) stays the same.
//
// Every request is signed per design §4.3 (B1 fix round adds the nonce):
// X-Abusekit-Key, X-Abusekit-Timestamp (RFC3339), X-Abusekit-Nonce (a
// fresh random value per attempt — see do's own doc comment for why),
// X-Abusekit-Signature
// (hex(hmac-sha256(secret, method\npath?query\ntimestamp\nkey_id\nnonce\nsha256(body)))).
package abusekit

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	mathrand "math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Header names, matching internal/serve's own (design §4.3).
const (
	headerKey       = "X-Abusekit-Key"
	headerTimestamp = "X-Abusekit-Timestamp"
	headerNonce     = "X-Abusekit-Nonce"
	headerSignature = "X-Abusekit-Signature"
	headerRequestID = "X-Request-Id"
)

// nonceBytes matches internal/serve's MinNonceBytes (design §4.2's
// amended "≥16 random bytes") — kept as this package's own constant
// rather than importing internal/serve, per this package's own
// no-internal-imports rule (see the package doc comment).
const nonceBytes = 16

// maxResponseBodyBytes bounds how much of a response body this client will
// read into memory (S7 fix round) — a defensive cap against a
// misbehaving or compromised server sending an unbounded body; abusekit's
// own responses are all small JSON envelopes, so this is far larger than
// anything a real response needs.
const maxResponseBodyBytes = 4 << 20

// maxRetryAfter caps how long this client will honour a server-supplied
// Retry-After value (S7 fix round) — a misconfigured or hostile server
// telling the client to wait an absurd amount of time must not be able to
// stall it indefinitely.
const maxRetryAfter = 60 * time.Second

// Client is a signed abusekit HTTP client. Construct with New; safe for
// concurrent use (it holds no mutable state beyond its http.Client, which
// is itself safe for concurrent use).
type Client struct {
	baseURL    string
	keyID      string
	secret     string
	httpClient *http.Client
	now        func() time.Time
	maxRetries int
}

// Option configures a Client at construction time.
type Option func(*Client)

// WithHTTPClient overrides the default http.Client (a plain
// &http.Client{Timeout: 30 * time.Second}) — useful for a custom
// transport, proxy, or test double.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) { c.httpClient = hc }
}

// WithClock overrides the default time.Now for the signed request
// timestamp — a test convenience for a deterministic signature.
func WithClock(now func() time.Time) Option {
	return func(c *Client) { c.now = now }
}

// WithMaxRetries overrides the default retry count (2, i.e. up to 3
// attempts total) for idempotent calls (SendEvents, Subject — see each
// method's own doc comment). 0 disables retries entirely. A negative value
// is rejected at call time by doRetryable (S7 fix round: a caller passing
// -1 gets a clear error, not a silently-empty (nil, nil, nil) result).
func WithMaxRetries(n int) Option {
	return func(c *Client) { c.maxRetries = n }
}

// New returns a Client for baseURL (e.g. "https://abusekit.internal"),
// signing every request with (keyID, secret) — one of design §4.3's
// configured keys.
func New(baseURL, keyID, secret string, opts ...Option) *Client {
	c := &Client{
		baseURL:    strings.TrimSuffix(baseURL, "/"),
		keyID:      keyID,
		secret:     secret,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		now:        func() time.Time { return time.Now().UTC() },
		maxRetries: 2,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// APIError is returned for any non-2xx response, decoded from the server's
// one error envelope (design §4.3: {error:{code,message,details}}).
// Callers can switch on Code for the enumerated wire codes (e.g.
// "not_found", "rate_limited", "forbidden", "subject_busy") without
// parsing Message.
type APIError struct {
	StatusCode int
	Code       string
	Message    string
	Details    map[string]any
	// RetryAfter is set from a 429/409's Retry-After header (seconds),
	// capped at maxRetryAfter (S7 fix round); 0 otherwise.
	RetryAfter time.Duration
	// RequestID is the server's X-Request-Id for this response, for
	// support/log correlation.
	RequestID string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("abusekit: %s (http %d): %s", e.Code, e.StatusCode, e.Message)
}

// generateNonce returns a fresh, hex-encoded random nonce (B1 fix round) —
// generated freshly for every attempt (including a retry of the "same"
// logical request), never reused: a client-side retry that reused the
// same nonce would be indistinguishable from a genuine replay to the
// server's own replay cache.
func generateNonce() (string, error) {
	b := make([]byte, nonceBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("abusekit: generate nonce: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// sign computes design §4.3's signature (B1 fix round: nonce included)
// over the exact bytes being sent.
func sign(secret, method, requestURI, timestamp, keyID, nonce string, body []byte) string {
	bodyHash := sha256.Sum256(body)
	canonical := method + "\n" + requestURI + "\n" + timestamp + "\n" + keyID + "\n" + nonce + "\n" + hex.EncodeToString(bodyHash[:])
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(canonical))
	return hex.EncodeToString(mac.Sum(nil))
}

// requestSpec is one HTTP call's shape, independent of retry policy.
type requestSpec struct {
	method string
	path   string // path + "?query", not including baseURL
	body   []byte // nil for no body
}

// do performs spec once: build, sign (with a FRESH nonce — B1 fix round:
// generated here, per call, so every attempt of a retried request signs
// independently and can never collide with a previous attempt in the
// server's replay cache), send, and either return the response body (2xx)
// or a decoded *APIError (non-2xx) / a transport error.
func (c *Client) do(ctx context.Context, spec requestSpec) ([]byte, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, spec.method, c.baseURL+spec.path, bytes.NewReader(spec.body))
	if err != nil {
		return nil, nil, fmt.Errorf("abusekit: build request: %w", err)
	}
	nonce, err := generateNonce()
	if err != nil {
		return nil, nil, err
	}
	timestamp := c.now().Format(time.RFC3339)
	sig := sign(c.secret, spec.method, req.URL.RequestURI(), timestamp, c.keyID, nonce, spec.body)
	req.Header.Set(headerKey, c.keyID)
	req.Header.Set(headerTimestamp, timestamp)
	req.Header.Set(headerNonce, nonce)
	req.Header.Set(headerSignature, sig)
	if len(spec.body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("abusekit: request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodyBytes))
	if err != nil {
		return nil, nil, fmt.Errorf("abusekit: read response: %w", err)
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return respBody, resp.Header, nil
	}

	apiErr := &APIError{StatusCode: resp.StatusCode, RequestID: resp.Header.Get(headerRequestID)}
	var envelope struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if jerr := json.Unmarshal(respBody, &envelope); jerr == nil && envelope.Error.Code != "" {
		apiErr.Code = envelope.Error.Code
		apiErr.Message = envelope.Error.Message
		apiErr.Details = envelope.Error.Details
	} else {
		apiErr.Code = "unknown"
		apiErr.Message = "non-2xx response with no decodable error envelope"
	}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, perr := strconv.Atoi(ra); perr == nil {
			wait := time.Duration(secs) * time.Second
			if wait > maxRetryAfter {
				wait = maxRetryAfter
			}
			apiErr.RetryAfter = wait
		}
	}
	return nil, resp.Header, apiErr
}

// doRetryable wraps do with retries for IDEMPOTENT calls only (design's
// own contract per call — see SendEvents/Subject's doc comments): a
// transport error or a 429/5xx response is retried up to c.maxRetries
// additional times, honouring a 429's Retry-After when present (capped at
// maxRetryAfter), otherwise a short exponential backoff with jitter. A
// non-retryable failure (4xx other than 429, or a successful decode)
// returns immediately.
func (c *Client) doRetryable(ctx context.Context, spec requestSpec) ([]byte, http.Header, error) {
	if c.maxRetries < 0 {
		// S7 fix round: a negative maxRetries must be a clear error, not a
		// silently-empty (nil, nil, nil) result from a loop that never runs.
		return nil, nil, fmt.Errorf("abusekit: maxRetries must be >= 0, got %d", c.maxRetries)
	}
	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		body, headers, err := c.do(ctx, spec)
		if err == nil {
			return body, headers, nil
		}
		lastErr = err

		var apiErr *APIError
		retryable := false
		if apiErr = asAPIError(err); apiErr != nil {
			retryable = apiErr.StatusCode == http.StatusTooManyRequests || apiErr.StatusCode >= 500
		} else {
			retryable = true // transport-level error
		}
		if !retryable || attempt == c.maxRetries {
			return nil, headers, lastErr
		}

		wait := backoff(attempt)
		if apiErr != nil && apiErr.RetryAfter > 0 {
			wait = apiErr.RetryAfter
		}
		select {
		case <-ctx.Done():
			return nil, headers, ctx.Err()
		case <-time.After(wait):
		}
	}
	return nil, nil, lastErr
}

func asAPIError(err error) *APIError {
	apiErr, ok := err.(*APIError)
	if !ok {
		return nil
	}
	return apiErr
}

// backoff returns a short exponential delay with jitter for retry attempt
// (0-indexed): ~100ms, ~200ms, ~400ms, capped at 2s.
func backoff(attempt int) time.Duration {
	base := 100 * time.Millisecond
	for i := 0; i < attempt; i++ {
		base *= 2
		if base > 2*time.Second {
			base = 2 * time.Second
			break
		}
	}
	jitter := time.Duration(mathrand.Int63n(int64(base) / 2))
	return base + jitter
}
