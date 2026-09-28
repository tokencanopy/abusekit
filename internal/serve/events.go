package serve

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/tokencanopy/abusekit/internal/config"
	"github.com/tokencanopy/abusekit/internal/event"
)

// MaxEventBatch is design §4.3's "batch of 1–100".
const MaxEventBatch = 100

// wireEvent mirrors event.Event's wire shape but keeps `at` as a raw
// string rather than time.Time (api-design boundary decision, S3): Go's
// encoding/json aborts decoding the WHOLE request the instant any one
// array element's typed field fails to unmarshal, which would turn one
// malformed timestamp into a whole-batch 400 instead of design's own
// per-item `bad_timestamp` rejection. Parsing `at` by hand per item (see
// handleEvents) keeps a batch's other 99 valid events accepted.
type wireEvent struct {
	ID      string         `json:"id"`
	Subject string         `json:"subject"`
	Type    string         `json:"type"`
	At      string         `json:"at"`
	Links   event.Links    `json:"links"`
	Data    map[string]any `json:"data"`
}

type eventsRequest struct {
	Events []wireEvent `json:"events"`
}

type rejectedWire struct {
	Index   int    `json:"index"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type eventsResponse struct {
	Accepted   []string       `json:"accepted"`
	Duplicates []string       `json:"duplicates"`
	Rejected   []rejectedWire `json:"rejected"`
}

// codeInternalValidationError is N2 fix round: event.Event.Validate/Redact
// always return a *event.ValidationError in practice (never a bare error),
// so the "else" branches below should be unreachable — but if one ever
// isn't, the per-item code must say so honestly rather than GUESSING
// "bad_id" (the previous behavior) for a failure that might have nothing
// to do with the event's id at all.
const codeInternalValidationError = "internal_validation_error"

// handleEvents is POST /v1/events (design §4.3).
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if !acceptsJSON(r) {
		writeError(w, r, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json", nil)
		return
	}

	body, sizeErr := readBounded(r, MaxEventBatchBody)
	if sizeErr != nil {
		writeBodyReadError(w, r, sizeErr)
		return
	}

	authCtx, authErr := s.authenticate(r, body, config.ScopeEvents)
	if authErr != nil {
		writeAuthError(w, r, authErr)
		return
	}

	// S3 fix round: rate-limited on the AUTHENTICATED key id, after
	// authenticate succeeds — never on the raw, unverified X-Abusekit-Key
	// header value. Keying on the trusted header would let an
	// unauthenticated caller exhaust a REAL producer's bucket just by
	// sending its key id (no secret needed) in a flood of otherwise-
	// rejected requests, starving the real producer's own legitimate
	// traffic. The coarse per-IP preAuthMiddleware (server.go) is what
	// bounds pre-auth request volume instead.
	if ok, retryAfter := s.eventsLimiter.Allow(authCtx.Key.ID, s.now()); !ok {
		writeRateLimited(w, r, retryAfter)
		return
	}

	var req eventsRequest
	if err := decodeStrictJSON(body, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, "bad_request", "body must be {\"events\":[...]}: "+err.Error(), nil)
		return
	}
	if len(req.Events) == 0 || len(req.Events) > MaxEventBatch {
		writeError(w, r, http.StatusBadRequest, "bad_request", fmt.Sprintf("events must contain 1..%d items, got %d", MaxEventBatch, len(req.Events)), nil)
		return
	}

	now := s.now()
	backfill := authCtx.Key.HasScope(config.ScopeBackfill)

	valid := make([]event.Event, 0, len(req.Events))
	originalIndex := make([]int, 0, len(req.Events))
	var rejected []rejectedWire

	for i, we := range req.Events {
		at, err := time.Parse(time.RFC3339, we.At)
		if err != nil {
			rejected = append(rejected, rejectedWire{Index: i, Code: string(event.CodeBadTimestamp), Message: "at must be RFC3339"})
			continue
		}
		e := event.Event{ID: we.ID, Subject: we.Subject, Type: we.Type, At: at, Links: we.Links, Data: we.Data}
		if verr := e.Validate(event.ValidateOptions{Now: now, Backfill: backfill}); verr != nil {
			var ve *event.ValidationError
			if errors.As(verr, &ve) {
				rejected = append(rejected, rejectedWire{Index: i, Code: string(ve.Code), Message: ve.Message})
			} else {
				s.log().Error("serve: event.Validate returned a non-ValidationError", "error", verr, "request_id", authCtx.RequestID)
				rejected = append(rejected, rejectedWire{Index: i, Code: codeInternalValidationError, Message: "validation failed"})
			}
			continue
		}
		if rerr := e.Redact(); rerr != nil {
			var ve *event.ValidationError
			if errors.As(rerr, &ve) {
				rejected = append(rejected, rejectedWire{Index: i, Code: string(ve.Code), Message: ve.Message})
			} else {
				s.log().Error("serve: event.Redact returned a non-ValidationError", "error", rerr, "request_id", authCtx.RequestID)
				rejected = append(rejected, rejectedWire{Index: i, Code: codeInternalValidationError, Message: "redaction failed"})
			}
			continue
		}
		valid = append(valid, e)
		originalIndex = append(originalIndex, i)
	}

	var result eventsResponse
	if len(valid) > 0 {
		appended, err := s.store.AppendEvents(r.Context(), authCtx.Key.Tenant, authCtx.Key.Producer, valid)
		if err != nil {
			s.log().Error("serve: append events failed", "error", err, "request_id", authCtx.RequestID)
			writeError(w, r, http.StatusInternalServerError, "internal", "failed to record events", nil)
			return
		}
		result.Accepted = appended.Accepted
		result.Duplicates = appended.Duplicates
		for _, rej := range appended.Rejected {
			rejected = append(rejected, rejectedWire{Index: originalIndex[rej.Index], Code: rej.Code, Message: rej.Message})
		}
	}
	result.Rejected = rejected
	if result.Accepted == nil {
		result.Accepted = []string{}
	}
	if result.Duplicates == nil {
		result.Duplicates = []string{}
	}
	if result.Rejected == nil {
		result.Rejected = []rejectedWire{}
	}

	writeJSON(w, r, http.StatusAccepted, result)
}

// acceptsJSON reports whether r's Content-Type is application/json
// (ignoring parameters like charset).
func acceptsJSON(r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		return false
	}
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	return mt == "application/json"
}

// readBounded reads at most limit+1 bytes from r.Body, returning an error
// if the body is longer than limit (413) rather than silently truncating
// it — a truncated batch would otherwise validate/redact successfully
// against corrupted JSON or a body that just happens to still parse. The
// returned error is either *http.MaxBytesError (too large) or some other
// read failure (a stalled/reset connection) — see writeBodyReadError for
// how the two are told apart on the wire (S7 fix round).
func readBounded(r *http.Request, limit int64) ([]byte, error) {
	limited := http.MaxBytesReader(nil, r.Body, limit)
	b, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// writeBodyReadError maps readBounded's error to the wire (S7 fix round):
// specifically *http.MaxBytesError is 413 payload_too_large; anything else
// (the client stalled, reset the connection, or otherwise failed to
// deliver a body it claimed) is 400 bad_request, never mislabeled as "too
// large" when the real problem was a broken/slow client.
func writeBodyReadError(w http.ResponseWriter, r *http.Request, err error) {
	var mbErr *http.MaxBytesError
	if errors.As(err, &mbErr) {
		writeError(w, r, http.StatusRequestEntityTooLarge, "payload_too_large", fmt.Sprintf("body exceeds %d bytes", mbErr.Limit), nil)
		return
	}
	writeError(w, r, http.StatusBadRequest, "bad_request", "failed to read request body", nil)
}

func writeRateLimited(w http.ResponseWriter, r *http.Request, retryAfter time.Duration) {
	if retryAfter < 0 {
		retryAfter = 0
	}
	w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds()+0.999)))
	writeError(w, r, http.StatusTooManyRequests, "rate_limited", "too many requests", nil)
}
