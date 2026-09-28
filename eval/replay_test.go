package eval

import (
	"strings"
	"testing"

	"github.com/tokencanopy/abusekit/internal/feature"
)

// TestLoadReplayDataset_StrictlyBeforeDecisionAt proves an event AT or
// AFTER decision_at never contributes to that slice's feature vector
// (task brief: "rebuild each subject's feature vector from events
// strictly before the chosen decision_at slice"). acct_1 has three
// resource.created events one minute apart; decision_at.early_15m is set
// to the EXACT instant of the second one, and decision_at.full defaults
// to "last event + 1ns" (so the full slice sees all three) — the
// contrast between early_15m's count (must be 1: only the first event,
// strictly earlier) and full's count (must be 3) is the proof no
// leakage occurred.
func TestLoadReplayDataset_StrictlyBeforeDecisionAt(t *testing.T) {
	events := strings.NewReader(`
{"subject":"acct_1","type":"subject.created","at":"2031-01-01T00:00:00Z","data":{"channel":"signup"}}
{"subject":"acct_1","type":"resource.created","at":"2031-01-01T00:01:00Z","data":{"kind":"agent","name":"A"}}
{"subject":"acct_1","type":"resource.created","at":"2031-01-01T00:02:00Z","data":{"kind":"agent","name":"B"}}
{"subject":"acct_1","type":"resource.created","at":"2031-01-01T00:03:00Z","data":{"kind":"agent","name":"C"}}
`)
	labels := strings.NewReader(`
{"subject":"acct_1","label":"benign","source":"operator","decision_at":{"early_15m":"2031-01-01T00:02:00Z"}}
`)
	ds, rowErrs, err := LoadReplayDataset(ReplayInput{EventsPath: "events.jsonl", Events: events, LabelsPath: "labels.jsonl", Labels: labels}, feature.BrandSet{}, "benign")
	if err != nil {
		t.Fatalf("LoadReplayDataset: %v (rowErrs=%v)", err, rowErrs)
	}
	if len(ds.Subjects) != 1 {
		t.Fatalf("len(Subjects) = %d, want 1", len(ds.Subjects))
	}
	subj := ds.Subjects[0]

	early, ok := subj.Points[SliceEarly15m]
	if !ok {
		t.Fatalf("no SliceEarly15m point")
	}
	if got := early.Features["resource_total"]; got != 1 {
		t.Errorf("early_15m resource_total = %v, want 1 (the event AT decision_at must not leak in)", got)
	}

	full, ok := subj.Points[SliceFull]
	if !ok {
		t.Fatalf("no SliceFull point")
	}
	if got := full.Features["resource_total"]; got != 3 {
		t.Errorf("full resource_total = %v, want 3 (all three events)", got)
	}
}

// TestLoadReplayDataset_NeighborEvidenceRespectsChronology proves
// linked_deleted_n is evaluated AS OF the scored subject's own
// decision_at, not as of the end of the whole dataset (neighbors.go's
// evidenceAsOf) — this is what makes an early churn incarnation
// legitimately hard to catch (design §1.2(c)): "subject_early" is scored
// BEFORE its linked neighbor's deletion happens, so it must see
// linked_deleted_n=0; "subject_late" is scored AFTER that same deletion,
// so it must see linked_deleted_n=1.
func TestLoadReplayDataset_NeighborEvidenceRespectsChronology(t *testing.T) {
	events := strings.NewReader(`
{"subject":"neighbor","type":"subject.created","at":"2031-01-01T00:00:00Z","links":{"email_hash":"` + hash64("shared-email") + `"},"data":{"channel":"signup"}}
{"subject":"neighbor","type":"subject.deleted","at":"2031-01-01T00:05:00Z","data":{"mode":"permanent"}}
{"subject":"subject_early","type":"subject.created","at":"2031-01-01T00:01:00Z","links":{"email_hash":"` + hash64("shared-email") + `"},"data":{"channel":"signup"}}
{"subject":"subject_early","type":"resource.created","at":"2031-01-01T00:02:00Z","data":{"kind":"agent","name":"X"}}
{"subject":"subject_late","type":"subject.created","at":"2031-01-01T00:10:00Z","links":{"email_hash":"` + hash64("shared-email") + `"},"data":{"channel":"signup"}}
{"subject":"subject_late","type":"resource.created","at":"2031-01-01T00:11:00Z","data":{"kind":"agent","name":"Y"}}
`)
	// "neighbor" deliberately has NO labels-file row at all: the
	// neighbour index is built from the EVENTS file alone (see
	// newDatasetNeighbors), so its permanent deletion is visible to
	// subject_early/subject_late regardless of whether "neighbor" itself
	// is ever scored.
	labels := strings.NewReader(`
{"subject":"subject_early","label":"abusive","source":"operator","decision_at":{"full":"2031-01-01T00:03:00Z"}}
{"subject":"subject_late","label":"abusive","source":"operator","decision_at":{"full":"2031-01-01T00:12:00Z"}}
`)
	ds, rowErrs, err := LoadReplayDataset(ReplayInput{EventsPath: "events.jsonl", Events: events, LabelsPath: "labels.jsonl", Labels: labels}, feature.BrandSet{}, "benign")
	if err != nil {
		t.Fatalf("LoadReplayDataset: %v (rowErrs=%v)", err, rowErrs)
	}

	byID := map[string]Subject{}
	for _, s := range ds.Subjects {
		byID[s.ID] = s
	}
	early := byID["subject_early"].Points[SliceFull].Features["linked_deleted_n"]
	late := byID["subject_late"].Points[SliceFull].Features["linked_deleted_n"]
	if early != 0 {
		t.Errorf("subject_early (scored BEFORE neighbor's deletion) linked_deleted_n = %v, want 0", early)
	}
	if late != 1 {
		t.Errorf("subject_late (scored AFTER neighbor's deletion) linked_deleted_n = %v, want 1", late)
	}
}

// TestLoadReplayDataset_TextFieldExtraction proves a content.sent
// event's redacted subject_line_skeleton/first_link_host reach a Point's
// Text map, keyed by field name (the same convention a rule's `text:`
// list selects from).
func TestLoadReplayDataset_TextFieldExtraction(t *testing.T) {
	events := strings.NewReader(`
{"subject":"acct_1","type":"subject.created","at":"2031-01-01T00:00:00Z","data":{"channel":"signup"}}
{"subject":"acct_1","type":"content.sent","at":"2031-01-01T00:01:00Z","data":{"subject_line":"Account Verification Required","recipient_domain":"customer.example.test","recipient_is_own_identity":false,"first_link_host":"verify.example.test"}}
`)
	labels := strings.NewReader(`{"subject":"acct_1","label":"abusive","source":"operator","decision_at":{"full":"2031-01-01T01:00:00Z"}}`)
	ds, rowErrs, err := LoadReplayDataset(ReplayInput{EventsPath: "events.jsonl", Events: events, LabelsPath: "labels.jsonl", Labels: labels}, feature.BrandSet{}, "benign")
	if err != nil {
		t.Fatalf("LoadReplayDataset: %v (rowErrs=%v)", err, rowErrs)
	}
	pt := ds.Subjects[0].Points[SliceFull]
	if got := pt.Text["subject_line_skeleton"]; len(got) != 1 {
		t.Fatalf("subject_line_skeleton = %v, want exactly one value", got)
	}
	if got := pt.Text["first_link_host"]; len(got) != 1 || got[0] != "verify.example.test" {
		t.Fatalf("first_link_host = %v, want [\"verify.example.test\"]", got)
	}
}

// hash64 returns a syntactically valid (64 lower-case hex characters)
// but otherwise arbitrary link hash for test fixtures — internal/event's
// Validate only checks shape, never that it's a real HMAC.
func hash64(seed string) string {
	const hex = "0123456789abcdef"
	var b []byte
	h := 0
	for _, c := range seed {
		h = h*131 + int(c)
	}
	for i := 0; i < 64; i++ {
		b = append(b, hex[(h+i*7)%16])
	}
	return string(b)
}
