package serve

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"net/http"
	"time"

	"github.com/tokencanopy/abusekit/internal/config"
	"github.com/tokencanopy/abusekit/internal/event"
	"github.com/tokencanopy/abusekit/internal/feature"
	"github.com/tokencanopy/abusekit/internal/store"
)

// maxLabelNoteBytes and maxLabelEvidenceRefBytes bound design §4.9's
// `note` ("<=500, retained like events") and `evidence_ref`. Unlike
// internal/event.Redact's truncate-over-cap policy for a high-volume
// producer feed, an over-cap label field is REJECTED outright (400) —
// design's own §4.9 wording ("note? (≤500...)") reads as a hard limit for
// this low-volume, deliberate, human/outcome-originated action, not
// something to silently and lossily shorten.
const (
	maxLabelNoteBytes        = 500
	maxLabelEvidenceRefBytes = 500
)

// labelRequest is POST /v1/labels' body (design §4.9).
type labelRequest struct {
	Subject     string `json:"subject"`
	Rule        string `json:"rule"`
	Label       string `json:"label"`
	Source      string `json:"source"`
	Actor       string `json:"actor"`
	Note        string `json:"note"`
	EvidenceRef string `json:"evidence_ref"`
}

// wireEventSlice is one event's shape as stored in a corpus example's
// event_slice (design §4.9): the already-redacted data a label snapshots.
type wireEventSlice struct {
	ID   string         `json:"id"`
	Type string         `json:"type"`
	At   string         `json:"at"`
	Data map[string]any `json:"data"`
}

// handleLabels is POST /v1/labels (design §4.9).
func (s *Server) handleLabels(w http.ResponseWriter, r *http.Request) {
	if !acceptsJSON(r) {
		writeError(w, r, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json", nil)
		return
	}
	body, sizeErr := readBounded(r, MaxOtherBody)
	if sizeErr != nil {
		writeBodyReadError(w, r, sizeErr)
		return
	}

	authCtx, authErr := s.authenticate(r, body, config.ScopeLabels)
	if authErr != nil {
		writeAuthError(w, r, authErr)
		return
	}

	var req labelRequest
	if err := decodeStrictJSON(body, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, "bad_request", "invalid body: "+err.Error(), nil)
		return
	}

	if err := validateSubjectParam(req.Subject); err != nil {
		writeError(w, r, http.StatusBadRequest, "bad_request", err.Error(), nil)
		return
	}
	if req.Label == "" {
		writeError(w, r, http.StatusBadRequest, "bad_request", "label is required", nil)
		return
	}
	if req.Source != "operator" && req.Source != "outcome" {
		writeError(w, r, http.StatusBadRequest, "bad_request", "source must be \"operator\" or \"outcome\"", nil)
		return
	}
	if req.Actor == "" {
		writeError(w, r, http.StatusBadRequest, "bad_request", "actor is required", nil)
		return
	}
	if len(req.Note) > maxLabelNoteBytes {
		writeError(w, r, http.StatusBadRequest, "bad_request", "note must be <= 500 bytes", nil)
		return
	}
	if len(req.EvidenceRef) > maxLabelEvidenceRefBytes {
		writeError(w, r, http.StatusBadRequest, "bad_request", "evidence_ref must be <= 500 bytes", nil)
		return
	}

	// Label vocabulary per rule is the rule's own `labels`; with rule
	// omitted, the label applies to the subject level, whose vocabulary is
	// the fixed benign|abusive pair (design §4.9).
	if req.Rule == "" {
		if req.Label != "benign" && req.Label != "abusive" {
			writeError(w, r, http.StatusBadRequest, "bad_request", "a subject-level label (no rule) must be \"benign\" or \"abusive\"", nil)
			return
		}
	} else {
		labels, ok := s.ruleLabels(req.Rule)
		if !ok {
			writeError(w, r, http.StatusBadRequest, "bad_request", "unknown rule \""+req.Rule+"\"", nil)
			return
		}
		if !containsLabel(labels, req.Label) {
			writeError(w, r, http.StatusBadRequest, "bad_request", "label \""+req.Label+"\" is not in rule \""+req.Rule+"\"'s label vocabulary", nil)
			return
		}
	}

	tenant := authCtx.Key.Tenant
	ctx := r.Context()

	storedEvents, err := s.store.EventsForSubject(ctx, tenant, req.Subject)
	if err != nil {
		s.log().Error("serve: load events for label failed", "error", err, "request_id", authCtx.RequestID)
		writeError(w, r, http.StatusInternalServerError, "internal", "failed to load subject history", nil)
		return
	}
	if len(storedEvents) == 0 {
		writeError(w, r, http.StatusNotFound, "not_found", "subject not found", nil)
		return
	}

	labelID, err := s.store.PutLabel(ctx, tenant, store.Label{
		Subject: req.Subject, Rule: req.Rule, Label: req.Label, Source: req.Source,
		Actor: req.Actor, Note: req.Note, EvidenceRef: req.EvidenceRef,
	})
	if err != nil {
		s.log().Error("serve: put label failed", "error", err, "request_id", authCtx.RequestID)
		writeError(w, r, http.StatusInternalServerError, "internal", "failed to record label", nil)
		return
	}

	corpusID, cerr := s.snapshotCorpusExample(ctx, tenant, req.Subject, labelID, storedEvents)
	if cerr != nil {
		// The label itself is already durably recorded — a corpus-snapshot
		// failure is logged, not surfaced as this request's own failure
		// (design's corpus mechanism exists for the harness/S4, not for
		// the operator/outcome caller posting the label; a caller that
		// gets a 201 has successfully recorded their label regardless).
		s.log().Error("serve: corpus snapshot failed", "error", cerr, "request_id", authCtx.RequestID, "label_id", labelID)
	}

	resp := map[string]any{"id": labelID}
	if cerr == nil {
		resp["corpus_example_id"] = corpusID
	}
	writeJSON(w, r, http.StatusCreated, resp)
}

// ruleLabels returns the label vocabulary for a currently-configured rule
// named name.
func (s *Server) ruleLabels(name string) ([]string, bool) {
	for _, r := range s.cfg.Rules {
		if r.Name == name {
			return r.Labels, true
		}
	}
	return nil, false
}

func containsLabel(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}

// snapshotCorpusExample implements design §4.9's "a label writes a corpus
// example that stores the redacted event slice up to decision_at
// (default: the first content.sent after signup, else the label time)
// plus the features as extracted at that time."
//
// Split (design: "by link cluster (fallback subject) hashed 80/20") is
// simplified here to a KEYED hash (N1 fix round: HMAC with
// s.corpusSplitSecret, never a bare sha256 — an unkeyed hash lets anyone
// who can guess/enumerate subject ids predict, and therefore game, which
// split a given id lands in) of the subject id alone — resolving a
// subject's full link-cluster identity (its same-tenant Neighbors) into
// one stable cluster key is deferred; see the S3 PR body's
// interpretations. This is a documented gap, not a silent shortcut: a
// later slice can swap in a real cluster key without changing
// corpus_examples' schema or this method's signature.
func (s *Server) snapshotCorpusExample(ctx context.Context, tenant, subject string, labelID int64, storedEvents []store.StoredEvent) (int64, error) {
	decisionAt := firstContentSentAt(storedEvents)
	if decisionAt.IsZero() {
		decisionAt = s.now()
	}

	slice := make([]wireEventSlice, 0, len(storedEvents))
	rawEvents := make([]event.Event, 0, len(storedEvents))
	for _, se := range storedEvents {
		if se.Event.At.After(decisionAt) {
			continue
		}
		slice = append(slice, wireEventSlice{
			ID:   se.Event.ID,
			Type: se.Event.Type,
			At:   se.Event.At.UTC().Format(time.RFC3339Nano),
			Data: se.Event.Data,
		})
		rawEvents = append(rawEvents, se.Event)
	}

	windows := feature.DefaultWindows(decisionAt)
	fr, err := feature.Extract(ctx, tenant, subject, rawEvents, s.neighbors, windows, s.brands)
	if err != nil {
		return 0, err
	}

	return s.store.InsertCorpusExample(ctx, tenant, store.CorpusExample{
		Subject:    subject,
		LabelID:    labelID,
		DecisionAt: decisionAt,
		EventSlice: slice,
		Features:   fr.Features.Map(),
		Split:      s.splitFor(subject),
	})
}

// firstContentSentAt returns the earliest content.sent event's At among
// storedEvents (design §4.9's decision_at default), or the zero time.Time
// if there is none.
func firstContentSentAt(storedEvents []store.StoredEvent) time.Time {
	var first time.Time
	for _, se := range storedEvents {
		if se.Event.Type != "content.sent" {
			continue
		}
		if first.IsZero() || se.Event.At.Before(first) {
			first = se.Event.At
		}
	}
	return first
}

// splitFor assigns subject to "train" (80%) or "test" (20%) via an
// HMAC-SHA256 keyed on s.corpusSplitSecret (N1 fix round) — see
// snapshotCorpusExample's own doc comment for why this must be keyed.
func (s *Server) splitFor(subject string) string {
	mac := hmac.New(sha256.New, s.corpusSplitSecret)
	mac.Write([]byte(subject))
	sum := mac.Sum(nil)
	bucket := binary.BigEndian.Uint64(sum[:8]) % 100
	if bucket < 80 {
		return "train"
	}
	return "test"
}
