// Package abusekit is a small, dependency-free Go client for abusekit's
// HTTP surface (design §4.1's "importable pkg/abusekit client"). It has no
// import of anything under internal/ — a public SDK is deliberately
// decoupled from the server's own wire types, so a change to internal/event
// or internal/core never forces a client API break, and the reverse: this
// package's own types are free to evolve on the client's own schedule as
// long as the WIRE contract (documented in docs/design's §4.3/§4.4/§4.9 and
// this repo's README) stays the same.
//
// Every request is signed per design §4.3: X-Abusekit-Key,
// X-Abusekit-Timestamp (RFC3339), X-Abusekit-Signature
// (hex(hmac-sha256(secret, method\npath?query\ntimestamp\nkey_id\nsha256(body)))).
package abusekit

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Header names, matching internal/serve's own (design §4.3).
const (
	headerKey       = "X-Abusekit-Key"
	headerTimestamp = "X-Abusekit-Timestamp"
	headerSignature = "X-Abusekit-Signature"
	headerRequestID = "X-Request-Id"
)

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
// attempts total) for idempotent calls (SendEvents, Subject, ListSubjects,
// Delete — see each method's own doc comment). 0 disables retries
// entirely.
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
// "not_found", "rate_limited", "forbidden") without parsing Message.
type APIError struct {
	StatusCode int
	Code       string
	Message    string
	Details    map[string]any
	// RetryAfter is set from a 429's Retry-After header (seconds), 0
	// otherwise.
	RetryAfter time.Duration
	// RequestID is the server's X-Request-Id for this response, for
	// support/log correlation.
	RequestID string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("abusekit: %s (http %d): %s", e.Code, e.StatusCode, e.Message)
}

// sign computes design §4.3's signature over the exact bytes being sent.
func sign(secret, method, requestURI, timestamp, keyID string, body []byte) string {
	bodyHash := sha256.Sum256(body)
	canonical := method + "\n" + requestURI + "\n" + timestamp + "\n" + keyID + "\n" + hex.EncodeToString(bodyHash[:])
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

// do performs spec once: build, sign, send, and either return the response
// body (2xx) or a decoded *APIError (non-2xx) / a transport error.
func (c *Client) do(ctx context.Context, spec requestSpec) ([]byte, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, spec.method, c.baseURL+spec.path, bytes.NewReader(spec.body))
	if err != nil {
		return nil, nil, fmt.Errorf("abusekit: build request: %w", err)
	}
	timestamp := c.now().Format(time.RFC3339)
	sig := sign(c.secret, spec.method, req.URL.RequestURI(), timestamp, c.keyID, spec.body)
	req.Header.Set(headerKey, c.keyID)
	req.Header.Set(headerTimestamp, timestamp)
	req.Header.Set(headerSignature, sig)
	if len(spec.body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("abusekit: request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
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
			apiErr.RetryAfter = time.Duration(secs) * time.Second
		}
	}
	return nil, resp.Header, apiErr
}

// doRetryable wraps do with retries for IDEMPOTENT calls only (design's
// own contract per call — see SendEvents/Subject/ListSubjects/Delete's doc
// comments): a transport error or a 429/5xx response is retried up to
// c.maxRetries additional times, honouring a 429's Retry-After when
// present, otherwise a short exponential backoff with jitter. A
// non-retryable failure (4xx other than 429, or a successful decode)
// returns immediately.
func (c *Client) doRetryable(ctx context.Context, spec requestSpec) ([]byte, http.Header, error) {
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
	jitter := time.Duration(rand.Int63n(int64(base) / 2))
	return base + jitter
}

// buildQuery is a small helper for building a "?k=v&..." suffix from
// non-empty values, in a fixed key order (deterministic, easier to test/
// log than url.Values' own map iteration would be).
func buildQuery(pairs [][2]string) string {
	var kept [][2]string
	for _, kv := range pairs {
		if kv[1] != "" {
			kept = append(kept, kv)
		}
	}
	if len(kept) == 0 {
		return ""
	}
	v := url.Values{}
	for _, kv := range kept {
		v.Set(kv[0], kv[1])
	}
	return "?" + v.Encode()
}
