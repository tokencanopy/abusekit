package serve

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tokencanopy/abusekit/internal/config"
	"github.com/tokencanopy/abusekit/internal/core"
	"github.com/tokencanopy/abusekit/internal/event"
	"github.com/tokencanopy/abusekit/internal/store"
)

type wireSignal struct {
	Rule        string   `json:"rule"`
	Mode        string   `json:"mode,omitempty"`
	Status      string   `json:"status"`
	Risk        *float64 `json:"risk,omitempty"`
	Flagged     *bool    `json:"flagged,omitempty"`
	Model       string   `json:"model,omitempty"`
	Checkpoint  string   `json:"checkpoint,omitempty"`
	Calibration string   `json:"calibration,omitempty"`
	Reason      string   `json:"reason,omitempty"`
	ErrorCode   string   `json:"error_code,omitempty"`
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
	}
	return ws
}

func signalFromCoreSignal(sig core.Signal) wireSignal {
	ws := wireSignal{Rule: sig.Rule, Mode: string(sig.Mode), Status: sig.Status, ErrorCode: sig.ErrorCode}
	if sig.Status == "scored" {
		risk := sig.Risk
		flagged := sig.Flagged
		ws.Risk = &risk
		ws.Flagged = &flagged
		ws.Model = sig.Model
		ws.Checkpoint = sig.Checkpoint
		ws.Calibration = sig.Calibration
		ws.Reason = sig.Reason
	}
	return ws
}

// verdictsETag hashes the verdict ids behind signals (design §4.4:
// "ETag = hash of the verdict ids in the response"), in the order
// signals is given — SubjectView's own query already orders
// deterministically by rule, so this never depends on Go map iteration.
func verdictsETag(signals []store.SubjectSignal) string {
	h := sha256.New()
	for _, sig := range signals {
		h.Write([]byte(sig.Rule))
		h.Write([]byte{0})
		var idBuf [8]byte
		id := sig.ID
		for i := 7; i >= 0; i-- {
			idBuf[i] = byte(id)
			id >>= 8
		}
		h.Write(idBuf[:])
	}
	return `"` + hex.EncodeToString(h.Sum(nil)) + `"`
}

// currentRuleNames returns s.cfg's rule names, for SubjectView's
// currentRules filter (S14: excludes a retired rule's stale verdicts from
// the live view).
func (s *Server) currentRuleNames() []string {
	names := make([]string, len(s.cfg.Rules))
	for i, r := range s.cfg.Rules {
		names[i] = r.Name
	}
	return names
}

// handleGetSubject is GET /v1/subjects/{subject} (design §4.4).
func (s *Server) handleGetSubject(w http.ResponseWriter, r *http.Request) {
	authCtx, authErr := s.authenticate(r, nil, config.ScopeRead)
	if authErr != nil {
		writeAuthError(w, r, authErr)
		return
	}

	subject := r.PathValue("subject")
	if !validSubjectParam(subject) {
		writeError(w, r, http.StatusBadRequest, "bad_request", "invalid subject", nil)
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
	etag := verdictsETag(view.Signals)
	w.Header().Set("ETag", etag)
	if inm := r.Header.Get("If-None-Match"); inm != "" && inm == etag {
		w.Header().Set(HeaderRequestID, requestID(r))
		w.WriteHeader(http.StatusNotModified)
		return
	}

	resp := wireSubject{
		Subject:          view.Subject,
		Score:            view.Score,
		Tier:             view.Tier,
		Degraded:         view.Degraded,
		Stale:            view.Stale,
		EventsSinceScore: view.EventsSinceScore,
		ScoredAt:         view.ScoredAt,
		Signals:          make([]wireSignal, len(view.Signals)),
	}
	for i, sig := range view.Signals {
		resp.Signals[i] = signalFromStoreSignal(sig)
	}
	writeJSON(w, r, http.StatusOK, resp)
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
	body, sizeErr := readBounded(r, MaxOtherBody)
	if sizeErr != nil {
		writeError(w, r, http.StatusRequestEntityTooLarge, "payload_too_large", "body too large", nil)
		return
	}

	authCtx, authErr := s.authenticate(r, body, config.ScopeRead)
	if authErr != nil {
		writeAuthError(w, r, authErr)
		return
	}

	subject := r.PathValue("subject")
	if !validSubjectParam(subject) {
		writeError(w, r, http.StatusBadRequest, "bad_request", "invalid subject", nil)
		return
	}

	deadlineMS := defaultEvaluateDeadlineMS
	if len(strings.TrimSpace(string(body))) > 0 {
		var req evaluateRequest
		if err := json.Unmarshal(body, &req); err != nil {
			writeError(w, r, http.StatusBadRequest, "bad_request", "body must be {\"deadline_ms\":<int>}", nil)
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

	verdict, err := s.worker.EvaluateSubject(r.Context(), authCtx.Key.Tenant, subject, time.Duration(deadlineMS)*time.Millisecond)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, r, http.StatusNotFound, "not_found", "subject not found", nil)
			return
		}
		if errors.Is(err, store.ErrAlreadyClaimed) {
			writeRateLimited(w, r, EvaluateRateLimit)
			return
		}
		s.log().Error("serve: evaluate failed", "error", err, "request_id", authCtx.RequestID, "subject", subject)
		writeError(w, r, http.StatusInternalServerError, "internal", "failed to evaluate subject", nil)
		return
	}

	scoredAt := s.now()
	resp := wireSubject{
		Subject:          subject,
		Score:            verdict.Score,
		Tier:             verdict.Tier,
		Degraded:         verdict.Degraded,
		Stale:            false,
		EventsSinceScore: 0,
		ScoredAt:         &scoredAt,
		Signals:          make([]wireSignal, len(verdict.Signals)),
		EvaluatedNow:     true,
	}
	for i, sig := range verdict.Signals {
		resp.Signals[i] = signalFromCoreSignal(sig)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, r, http.StatusOK, resp)
}

type wireSubjectListItem struct {
	Subject   string     `json:"subject"`
	Tier      string     `json:"tier"`
	Score     float64    `json:"score"`
	VerdictID int64      `json:"verdict_id,omitempty"`
	ScoredAt  *time.Time `json:"scored_at,omitempty"`
}

type wireSubjectList struct {
	Subjects   []wireSubjectListItem `json:"subjects"`
	NextCursor *string               `json:"next_cursor"`
}

// handleListSubjects is GET /v1/subjects (design §4.4's list endpoint,
// with the task's additional `since` filter — see internal/store.ListSubjects'
// own doc comment).
func (s *Server) handleListSubjects(w http.ResponseWriter, r *http.Request) {
	authCtx, authErr := s.authenticate(r, nil, config.ScopeRead)
	if authErr != nil {
		writeAuthError(w, r, authErr)
		return
	}

	q := r.URL.Query()
	opts := store.ListSubjectsOptions{Tier: q.Get("tier"), Class: q.Get("class")}

	if tier := q.Get("tier"); tier != "" && !validTier(tier) {
		writeError(w, r, http.StatusBadRequest, "bad_request", "tier must be one of unknown, low, medium, high", nil)
		return
	}
	if class := q.Get("class"); class != "" && !validClass(class) {
		writeError(w, r, http.StatusBadRequest, "bad_request", "class must be one of customer, internal, synthetic", nil)
		return
	}
	if sinceStr := q.Get("since"); sinceStr != "" {
		since, err := time.Parse(time.RFC3339, sinceStr)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "bad_request", "since must be RFC3339", nil)
			return
		}
		opts.Since = since
	}
	if limitStr := q.Get("limit"); limitStr != "" {
		limit, err := strconv.Atoi(limitStr)
		if err != nil || limit <= 0 {
			writeError(w, r, http.StatusBadRequest, "bad_request", "limit must be a positive integer", nil)
			return
		}
		opts.Limit = limit
	}
	if cursorStr := q.Get("cursor"); cursorStr != "" {
		cursor, err := decodeCursor(cursorStr)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "bad_request", "invalid cursor", nil)
			return
		}
		opts.Cursor = cursor
	}

	items, next, err := s.store.ListSubjects(r.Context(), authCtx.Key.Tenant, opts)
	if err != nil {
		s.log().Error("serve: list subjects failed", "error", err, "request_id", authCtx.RequestID)
		writeError(w, r, http.StatusInternalServerError, "internal", "failed to list subjects", nil)
		return
	}

	resp := wireSubjectList{Subjects: make([]wireSubjectListItem, len(items))}
	for i, it := range items {
		resp.Subjects[i] = wireSubjectListItem{Subject: it.Subject, Tier: it.Tier, Score: it.Score, VerdictID: it.VerdictID, ScoredAt: it.ScoredAt}
	}
	if next != nil {
		enc := encodeCursor(*next)
		resp.NextCursor = &enc
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, r, http.StatusOK, resp)
}

// handleEraseSubject is DELETE /v1/subjects/{subject} (design §4.4's
// erasure request).
func (s *Server) handleEraseSubject(w http.ResponseWriter, r *http.Request) {
	authCtx, authErr := s.authenticate(r, nil, config.ScopeErase)
	if authErr != nil {
		writeAuthError(w, r, authErr)
		return
	}

	subject := r.PathValue("subject")
	if !validSubjectParam(subject) {
		writeError(w, r, http.StatusBadRequest, "bad_request", "invalid subject", nil)
		return
	}

	result, err := s.store.EraseSubject(r.Context(), authCtx.Key.Tenant, subject, s.now())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, r, http.StatusNotFound, "not_found", "subject not found", nil)
			return
		}
		s.log().Error("serve: erase subject failed", "error", err, "request_id", authCtx.RequestID, "subject", subject)
		writeError(w, r, http.StatusInternalServerError, "internal", "failed to erase subject", nil)
		return
	}

	writeJSON(w, r, http.StatusOK, map[string]any{
		"subject":        subject,
		"erased":         true,
		"mode":           string(result.Mode),
		"erased_at":      result.ErasedAt,
		"already_erased": result.AlreadyErased,
	})
}

func validSubjectParam(subject string) bool {
	return subject != "" && len(subject) <= event.MaxSubjectLen
}

func validTier(tier string) bool {
	switch tier {
	case "unknown", "low", "medium", "high":
		return true
	}
	return false
}

func validClass(class string) bool {
	switch class {
	case "customer", "internal", "synthetic":
		return true
	}
	return false
}

// cursorWire is the JSON shape encoded into the opaque, base64url cursor
// token (api-design: a cursor is an opaque token to the client, never a
// raw sortable value it could construct or tamper with meaningfully).
type cursorWire struct {
	ScoredAt *time.Time `json:"scored_at"`
	Subject  string     `json:"subject"`
}

func encodeCursor(c store.ListCursor) string {
	b, _ := json.Marshal(cursorWire{ScoredAt: c.ScoredAt, Subject: c.Subject})
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s string) (*store.ListCursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	var cw cursorWire
	if err := json.Unmarshal(b, &cw); err != nil {
		return nil, err
	}
	if cw.Subject == "" {
		return nil, errBadCursor
	}
	return &store.ListCursor{ScoredAt: cw.ScoredAt, Subject: cw.Subject}, nil
}

var errBadCursor = errors.New("serve: cursor missing subject")
