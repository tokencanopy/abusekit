package serve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tokencanopy/abusekit/internal/config"
	"github.com/tokencanopy/abusekit/internal/event"
	"github.com/tokencanopy/abusekit/internal/store"
)

type wireSignal struct {
	Rule          string   `json:"rule"`
	Mode          string   `json:"mode,omitempty"`
	Status        string   `json:"status"`
	Risk          *float64 `json:"risk,omitempty"`
	Flagged       *bool    `json:"flagged,omitempty"`
	Model         string   `json:"model,omitempty"`
	Checkpoint    string   `json:"checkpoint,omitempty"`
	Calibration   string   `json:"calibration,omitempty"`
	Reason        string   `json:"reason,omitempty"`
	ReasonVersion int      `json:"reason_version,omitempty"`
	ErrorCode     string   `json:"error_code,omitempty"`
}

type wireSubject struct {
	Subject          string       `json:"subject"`
	Score            float64      `json:"score"`
	Tier             string       `json:"tier"`
	Degraded         bool         `json:"degraded"`
	Stale            bool         `json:"stale"`
	EventsSinceScore int64        `json:"events_since_score"`
	ScoredAt         *time.Time   `json:"scored_at,omitempty"`
	Signals          []wireSignal `json:"signals"`
	EvaluatedNow     bool         `json:"evaluated_now,omitempty"`
	// EvaluateNote carries a short, machine-readable reason evaluate
	// couldn't produce a fresh round this call (S1 fix round) — currently
	// only "deadline_exceeded". Never set on GET's own response, and never
	// set on evaluate's response when EvaluatedNow is true.
	EvaluateNote string `json:"evaluate_note,omitempty"`
}

func signalFromStoreSignal(sig store.SubjectSignal) wireSignal {
	ws := wireSignal{Rule: sig.Rule, Mode: sig.Mode, Status: sig.Status, ErrorCode: sig.ErrorCode}
	if sig.Status == "scored" {
		risk := sig.Risk
		flagged := sig.Flagged
		ws.Risk = &risk
		ws.Flagged = &flagged
		ws.Model = sig.Model
		ws.Checkpoint = sig.Checkpoint
		ws.Calibration = sig.Calibration
		ws.Reason = sig.Reason
		ws.ReasonVersion = sig.ReasonVersion
	}
	return ws
}

// sortSignalsByRule gives every response's signals array the same
// deterministic order (S8 fix round), independent of whatever order the
// underlying store query or map iteration happened to produce.
func sortSignalsByRule(signals []wireSignal) {
	sort.Slice(signals, func(i, j int) bool { return signals[i].Rule < signals[j].Rule })
}

// verdictsETag hashes the verdict ids AND the subject's dirty/scored
// sequence counters (S2 fix round, proven: hashing only the verdict ids
// gave every never-scored subject the SAME constant ETag, and gave a
// freshly-dirtied-but-not-yet-rescored subject the SAME ETag as before the
// new event landed, since verdicts themselves hadn't changed yet — a
// caller relying on If-None-Match to notice "this got stale" never would).
// Order is always by rule (sortSignalsByRule is applied to the RESPONSE;
// this hashes the store's own signals slice, whose query already orders by
// rule — see store.SubjectView), so this never depends on Go map order.
func verdictsETag(signals []store.SubjectSignal, dirtySeq, scoredSeq int64) string {
	h := sha256.New()
	for _, sig := range signals {
		h.Write([]byte(sig.Rule))
		h.Write([]byte{0})
		var idBuf [8]byte
		putUint64(idBuf[:], uint64(sig.ID))
		h.Write(idBuf[:])
	}
	var seqBuf [16]byte
	putUint64(seqBuf[0:8], uint64(dirtySeq))
	putUint64(seqBuf[8:16], uint64(scoredSeq))
	h.Write(seqBuf[:])
	return `"` + hex.EncodeToString(h.Sum(nil)) + `"`
}

func putUint64(b []byte, v uint64) {
	for i := 7; i >= 0; i-- {
		b[i] = byte(v)
		v >>= 8
	}
}

// etagMatches reports whether the client's If-None-Match value matches
// etag. Accepts a weak validator prefix (S11 fix round: `W/"..."`) as
// equivalent to its strong form — this server never generates a weak
// ETag itself, but a caching proxy or a client library commonly rewrites
// a cached strong ETag into a weak one on revalidation, and a literal
// byte-for-byte comparison would then never match. Does not implement the
// list form (If-None-Match: "a", "b") or "*" — GET /v1/subjects/{subject}
// only ever compares against the one ETag it just computed, so those
// forms have no meaningful target here; documented, not silently ignored.
func etagMatches(ifNoneMatch, etag string) bool {
	v := strings.TrimSpace(ifNoneMatch)
	v = strings.TrimPrefix(v, "W/")
	return v == etag
}

// currentRuleNames returns s.cfg's rules for SubjectView's currentRules
// filter (S14: excludes a retired rule's stale verdicts from the live
// view; T4 round 3: also tells SubjectView which of those are advise-mode,
// so it can flag one that's missing entirely — never scored, not even as
// unscored — as degraded too).
func (s *Server) currentRuleNames() []store.CurrentRule {
	rules := make([]store.CurrentRule, len(s.cfg.Rules))
	for i, r := range s.cfg.Rules {
		rules[i] = store.CurrentRule{Name: r.Name, Advise: r.Mode == config.ModeAdvise}
	}
	return rules
}

// wireSubjectFromView builds the common response shape GET and evaluate
// both render (S1/S8 fix round: evaluate ALWAYS finishes with the same
// store.SubjectView read GET uses — for a genuinely fresh round, for the
// stored-view fallback when evaluate couldn't produce one, and for the
// synthetic/not-scorable case — so both endpoints can never disagree about
// what a subject's current state looks like on the wire).
func wireSubjectFromView(view *store.SubjectView, evaluatedNow bool, note string) wireSubject {
	resp := wireSubject{
		Subject:          view.Subject,
		Score:            view.Score,
		Tier:             view.Tier,
		Degraded:         view.Degraded,
		Stale:            view.Stale,
		EventsSinceScore: view.EventsSinceScore,
		ScoredAt:         utcOrNil(view.ScoredAt),
		Signals:          make([]wireSignal, len(view.Signals)),
		EvaluatedNow:     evaluatedNow,
		EvaluateNote:     note,
	}
	for i, sig := range view.Signals {
		resp.Signals[i] = signalFromStoreSignal(sig)
	}
	sortSignalsByRule(resp.Signals)
	return resp
}

// handleGetSubject is GET /v1/subjects/{subject} (design §4.4).
func (s *Server) handleGetSubject(w http.ResponseWriter, r *http.Request) {
	authCtx, authErr := s.authenticate(r, nil, config.ScopeRead)
	if authErr != nil {
		writeAuthError(w, r, authErr)
		return
	}

	subject := r.PathValue("subject")
	if err := validateSubjectParam(subject); err != nil {
		writeError(w, r, http.StatusBadRequest, "bad_request", err.Error(), nil)
		return
	}

	view, err := s.store.SubjectView(r.Context(), authCtx.Key.Tenant, subject, s.currentRuleNames())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, r, http.StatusNotFound, "not_found", "subject not found", nil)
			return
		}
		s.log().Error("serve: subject view failed", "error", err, "request_id", authCtx.RequestID)
		writeError(w, r, http.StatusInternalServerError, "internal", "failed to load subject", nil)
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	etag := verdictsETag(view.Signals, view.DirtySeq, view.ScoredSeq)
	w.Header().Set("ETag", etag)
	if inm := r.Header.Get("If-None-Match"); inm != "" && etagMatches(inm, etag) {
		w.Header().Set(HeaderRequestID, requestID(r))
		w.WriteHeader(http.StatusNotModified)
		return
	}

	writeJSON(w, r, http.StatusOK, wireSubjectFromView(view, false, ""))
}

// evaluateRequest is POST /v1/subjects/{subject}/evaluate's optional body
// (design §4.4: "{deadline_ms ≤ 3000}").
type evaluateRequest struct {
	DeadlineMS *int `json:"deadline_ms"`
}

const defaultEvaluateDeadlineMS = 3000
const maxEvaluateDeadlineMS = 3000

// handleEvaluate is POST /v1/subjects/{subject}/evaluate (design §4.4).
func (s *Server) handleEvaluate(w http.ResponseWriter, r *http.Request) {
	// S4 fix round: same content-type discipline as events/labels once a
	// body is actually present — an evaluate call with no body at all
	// (the common case) needs no Content-Type, but a caller that DOES send
	// one must send JSON, not silently have it ignored.
	if r.ContentLength > 0 && !acceptsJSON(r) {
		writeError(w, r, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json", nil)
		return
	}
	body, sizeErr := readBounded(r, MaxOtherBody)
	if sizeErr != nil {
		writeBodyReadError(w, r, sizeErr)
		return
	}

	authCtx, authErr := s.authenticate(r, body, config.ScopeRead)
	if authErr != nil {
		writeAuthError(w, r, authErr)
		return
	}

	subject := r.PathValue("subject")
	if err := validateSubjectParam(subject); err != nil {
		writeError(w, r, http.StatusBadRequest, "bad_request", err.Error(), nil)
		return
	}

	deadlineMS := defaultEvaluateDeadlineMS
	if len(strings.TrimSpace(string(body))) > 0 {
		var req evaluateRequest
		if err := decodeStrictJSON(body, &req); err != nil {
			writeError(w, r, http.StatusBadRequest, "bad_request", "body must be {\"deadline_ms\":<int>}: "+err.Error(), nil)
			return
		}
		if req.DeadlineMS != nil {
			deadlineMS = *req.DeadlineMS
		}
	}
	if deadlineMS <= 0 || deadlineMS > maxEvaluateDeadlineMS {
		writeError(w, r, http.StatusBadRequest, "bad_request", "deadline_ms must be in 1.."+strconv.Itoa(maxEvaluateDeadlineMS), nil)
		return
	}

	rlKey := authCtx.Key.Tenant + "/" + subject
	if ok, retryAfter := s.evalLimiter.Allow(rlKey, s.now()); !ok {
		writeRateLimited(w, r, retryAfter)
		return
	}

	ctx := r.Context()
	tenant := authCtx.Key.Tenant
	evaluatedNow, evalErr := s.worker.EvaluateSubject(ctx, tenant, subject, time.Duration(deadlineMS)*time.Millisecond)
	if evalErr != nil {
		var busy *store.ErrBusy
		switch {
		case errors.Is(evalErr, store.ErrNotFound):
			writeError(w, r, http.StatusNotFound, "not_found", "subject not found", nil)
			return
		case errors.As(evalErr, &busy):
			// S1 fix round: a REAL Retry-After derived from the actual
			// lease/backoff remaining, not a fixed guess — and 409
			// subject_busy, distinct from 429 rate_limited (that's the
			// client calling faster than 1/s; this is "something else is
			// already mid-round for this exact subject").
			writeSubjectBusy(w, r, busy.RetryAt.Sub(s.now()))
			return
		case errors.Is(evalErr, store.ErrNotScorable), errors.Is(evalErr, context.DeadlineExceeded), errors.Is(evalErr, context.Canceled):
			// Both fall through to the stored-view render below —
			// ErrNotScorable (S1: e2a's own prober accounts are synthetic)
			// and a blown deadline are normal, expected outcomes, never a
			// 500.
		default:
			// S10 fix round: never log the raw subject id at a level an
			// operator's aggregate log view routinely surfaces — the
			// request id is enough to correlate with a caller's own
			// report, and the tenant is already low-cardinality/non-
			// identifying on its own.
			s.log().Error("serve: evaluate failed", "error", evalErr, "request_id", authCtx.RequestID, "tenant", tenant)
			writeError(w, r, http.StatusInternalServerError, "internal", "failed to evaluate subject", nil)
			return
		}
	}

	view, err := s.store.SubjectView(ctx, tenant, subject, s.currentRuleNames())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Only reachable if the subject vanished between the claim
			// attempt above and this read (e.g. a concurrent erasure in a
			// future slice) — 404 is still correct, not a 500.
			writeError(w, r, http.StatusNotFound, "not_found", "subject not found", nil)
			return
		}
		// T5 (round 3): ctx here is the REQUEST's own context, which a
		// client disconnecting mid-evaluate already cancelled — reliably
		// so whenever evalErr above was itself context.Canceled (the exact
		// same ctx), but also possible on its own in the narrow window
		// between EvaluateSubject returning and this call starting. A
		// client that already left is not a server failure: writing a
		// response is pointless (nothing is listening any more) and
		// logging it at ERROR level would misrepresent a routine
		// disconnect as an operational incident an operator needs to look
		// into.
		if errors.Is(err, context.Canceled) {
			s.log().Debug("serve: client disconnected before subject view could load", "request_id", authCtx.RequestID)
			return
		}
		s.log().Error("serve: subject view after evaluate failed", "error", err, "request_id", authCtx.RequestID)
		writeError(w, r, http.StatusInternalServerError, "internal", "failed to load subject", nil)
		return
	}

	note := ""
	if errors.Is(evalErr, context.DeadlineExceeded) || errors.Is(evalErr, context.Canceled) {
		note = "deadline_exceeded"
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, r, http.StatusOK, wireSubjectFromView(view, evaluatedNow, note))
}

// writeSubjectBusy is 409 subject_busy (S1 fix round), with a Retry-After
// derived from the actual claim lease/backoff remaining.
func writeSubjectBusy(w http.ResponseWriter, r *http.Request, retryAfter time.Duration) {
	if retryAfter < 0 {
		retryAfter = 0
	}
	w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds()+0.999)))
	writeError(w, r, http.StatusConflict, "subject_busy", "subject is currently being scored elsewhere", nil)
}

// utcOrNil normalizes t to UTC for the wire (S8 fix round: every timestamp
// this package returns is UTC RFC3339, never whatever zone a DB session
// or a local clock happened to carry), or returns nil for a nil/zero
// input.
func utcOrNil(t *time.Time) *time.Time {
	if t == nil || t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

// validateSubjectParam checks a path-parameter subject against the SAME
// structural rules internal/event.Event.Validate applies to a subject id
// on ingest (S5 fix round, proven: an unvalidated NUL or invalid-UTF8
// byte in the path reached Postgres and came back as a raw driver error,
// logged and 500'd instead of a clean 400 — a malformed path is a client
// mistake, never a server failure).
func validateSubjectParam(subject string) error {
	if subject == "" || len(subject) > event.MaxSubjectLen {
		return errInvalidSubject
	}
	if !utf8.ValidString(subject) {
		return errInvalidSubject
	}
	for _, r := range subject {
		if r < 0x20 || r == 0x7f {
			return errInvalidSubject
		}
	}
	return nil
}

var errInvalidSubject = errors.New("subject must be 1..256 bytes of valid UTF-8 with no control characters")
