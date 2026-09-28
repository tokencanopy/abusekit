package serve

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
)

// apiError is design §4.3's one error envelope shape:
// {error:{code,message,details?}}. Every non-2xx response this package
// writes (auth failures, validation failures, not-found, rate limits)
// uses this exact shape — never a bare string, never a stack trace, never
// anything else that could leak internals (api-design's "narrow status
// codes... no internals leaked").
type apiError struct {
	Error apiErrorBody `json:"error"`
}

type apiErrorBody struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// writeError writes status with body {error:{code,message,details}}. It
// never includes a Go error's own .Error() text unless the caller
// explicitly passes it as message — every call site in this package
// supplies a short, deliberately-written, non-leaking message.
func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string, details map[string]any) {
	writeJSON(w, r, status, apiError{Error: apiErrorBody{Code: code, Message: message, Details: details}})
}

func writeAuthError(w http.ResponseWriter, r *http.Request, e *authError) {
	writeError(w, r, e.Status, e.Code, e.Message, nil)
}

// writeJSON marshals v as the response body with status, always setting
// Content-Type, X-Content-Type-Options: nosniff (api-design boundary
// hardening — a JSON API response is never sniffed as anything else), and
// echoing X-Request-Id. Marshal failure (should be unreachable for the
// types this package ever passes) falls back to a bare 500 with no body
// rather than a panic or a half-written response.
func writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set(HeaderRequestID, requestID(r))
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// requestIDCtxKey stores the resolved request id on the request context
// (set by withRequestID, read by requestID) so every handler and
// authenticate see the SAME id a middleware already validated/generated,
// rather than re-deriving it from the raw header each time.
type requestIDCtxKeyType struct{}

var requestIDCtxKey = requestIDCtxKeyType{}

// requestIDRe is a conservative allowlist for a CLIENT-supplied
// X-Request-Id (design: "accepted/generated"): ASCII letters, digits,
// dash, underscore, 1..128 bytes. A supplied value outside this shape is
// discarded in favor of a freshly generated one rather than echoed back
// verbatim — this header reaches logs, so it must never carry control
// characters, absurd length, or anything else a log/metrics pipeline
// wouldn't want reflected into it unsanitized.
var requestIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func requestID(r *http.Request) string {
	if v, ok := r.Context().Value(requestIDCtxKey).(string); ok && v != "" {
		return v
	}
	return "unknown"
}

func generateRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is essentially unreachable on any real OS;
		// a fixed fallback keeps the handler chain from ever panicking
		// over it, at the cost of a non-unique id for that one request.
		return "reqid-fallback"
	}
	return "req_" + hex.EncodeToString(b[:])
}
