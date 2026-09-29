package eval

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/tokencanopy/abusekit/internal/config"
	"github.com/tokencanopy/abusekit/internal/feature"
	"github.com/tokencanopy/abusekit/internal/model/fake"
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

// pointsByID is a small test helper: subject id -> its Points map, for
// DeepEqual comparisons across two loads of "the same dataset except one
// label value".
func pointsByID(ds Dataset) map[string]map[Slice]Point {
	out := make(map[string]map[Slice]Point, len(ds.Subjects))
	for _, s := range ds.Subjects {
		out[s.ID] = s.Points
	}
	return out
}

// relabelFixtureEvents is shared by the two relabelling tests below:
// acct_a and acct_b share an email_hash, each with one resource.created.
const relabelFixtureEvents = `
{"subject":"acct_a","type":"subject.created","at":"2031-02-01T00:00:00Z","links":{"email_hash":"` + "REPLACED" + `"},"data":{"channel":"signup"}}
{"subject":"acct_a","type":"resource.created","at":"2031-02-01T00:01:00Z","data":{"kind":"agent","name":"A"}}
{"subject":"acct_b","type":"subject.created","at":"2031-02-01T00:02:00Z","links":{"email_hash":"` + "REPLACED" + `"},"data":{"channel":"signup"}}
{"subject":"acct_b","type":"resource.created","at":"2031-02-01T00:03:00Z","data":{"kind":"agent","name":"B"}}
`

func relabelEvents() string {
	return strings.ReplaceAll(relabelFixtureEvents, "REPLACED", hash64("relabel-shared-email"))
}

// TestLoadReplayDataset_RelabellingGroundTruthDoesNotChangeFeatures is
// fix round B1's own acceptance test (task brief / review): relabelling
// acct_a's ground truth from "abusive" to "suspicious" — both POSITIVE
// under benign_label "benign", so nothing about the binary ground-truth
// split changes — must leave every subject's Points byte-for-byte
// DeepEqual, because linked_labelled_abusive_n (and every other feature)
// is computed ENTIRELY from the events file plus `label`-typed rows in
// it, never from labels.jsonl's own ground truth being evaluated.
func TestLoadReplayDataset_RelabellingGroundTruthDoesNotChangeFeatures(t *testing.T) {
	load := func(acctALabel string) Dataset {
		t.Helper()
		labels := strings.NewReader(`
{"subject":"acct_a","label":"` + acctALabel + `","source":"operator","decision_at":{"full":"2031-02-01T01:00:00Z"}}
{"subject":"acct_b","label":"abusive","source":"operator","decision_at":{"full":"2031-02-01T01:00:00Z"}}
`)
		ds, rowErrs, err := LoadReplayDataset(ReplayInput{EventsPath: "events.jsonl", Events: strings.NewReader(relabelEvents()), LabelsPath: "labels.jsonl", Labels: labels}, feature.BrandSet{}, "benign")
		if err != nil {
			t.Fatalf("LoadReplayDataset(%s): %v (rowErrs=%v)", acctALabel, err, rowErrs)
		}
		return ds
	}

	dsAbusive := load("abusive")
	dsSuspicious := load("suspicious")

	pa, ps := pointsByID(dsAbusive), pointsByID(dsSuspicious)
	if !reflect.DeepEqual(pa["acct_a"], ps["acct_a"]) {
		t.Errorf("acct_a's OWN Points changed when its own label changed:\n abusive=%+v\n suspicious=%+v", pa["acct_a"], ps["acct_a"])
	}
	if !reflect.DeepEqual(pa["acct_b"], ps["acct_b"]) {
		t.Errorf("acct_b's Points changed when acct_a's label changed:\n abusive=%+v\n suspicious=%+v", pa["acct_b"], ps["acct_b"])
	}
}

// TestLoadReplayDataset_FlippingLaterSubjectsLabelChangesNothing is the
// review's second B1 acceptance test: acct_a's decision point (00:03) is
// EARLIER than acct_b's underlying activity — flipping acct_b's ground
// truth (abusive <-> benign) must not change acct_a's features at all,
// since acct_a's own Point was already fully resolved before acct_b's
// label (or even acct_b's later events) could possibly matter, and
// because ground truth never feeds features in the first place (B1).
func TestLoadReplayDataset_FlippingLaterSubjectsLabelChangesNothing(t *testing.T) {
	load := func(acctBLabel string) Point {
		t.Helper()
		labels := strings.NewReader(`
{"subject":"acct_a","label":"abusive","source":"operator","decision_at":{"full":"2031-02-01T00:01:30Z"}}
{"subject":"acct_b","label":"` + acctBLabel + `","source":"operator","decision_at":{"full":"2031-02-01T01:00:00Z"}}
`)
		ds, rowErrs, err := LoadReplayDataset(ReplayInput{EventsPath: "events.jsonl", Events: strings.NewReader(relabelEvents()), LabelsPath: "labels.jsonl", Labels: labels}, feature.BrandSet{}, "benign")
		if err != nil {
			t.Fatalf("LoadReplayDataset(%s): %v (rowErrs=%v)", acctBLabel, err, rowErrs)
		}
		return pointsByID(ds)["acct_a"][SliceFull]
	}

	withAbusive := load("abusive")
	withBenign := load("benign")
	if !reflect.DeepEqual(withAbusive, withBenign) {
		t.Errorf("acct_a's Points changed when acct_b's (a LATER subject's) label flipped:\n abusive=%+v\n benign=%+v", withAbusive, withBenign)
	}
}

// TestLoadReplayDataset_LabelEventChronologyPerSlice proves
// linked_labelled_abusive_n comes from a `label`-typed EVENT (fix round
// B1), visible only strictly before the asked-for slice's own
// decision_at: "poster" posts a label event at 00:20; "target" (linked by
// email) has an early_15m decision point at 00:15 (BEFORE the label
// event: must NOT see it) and a full decision point at 00:30 (AFTER it:
// must see it).
func TestLoadReplayDataset_LabelEventChronologyPerSlice(t *testing.T) {
	shared := hash64("label-event-shared-email")
	events := strings.NewReader(`
{"subject":"poster","type":"subject.created","at":"2031-03-01T00:00:00Z","links":{"email_hash":"` + shared + `"},"data":{"channel":"signup"}}
{"subject":"poster","type":"label","at":"2031-03-01T00:20:00Z","data":{"label":"abusive"}}
{"subject":"target","type":"subject.created","at":"2031-03-01T00:01:00Z","links":{"email_hash":"` + shared + `"},"data":{"channel":"signup"}}
{"subject":"target","type":"resource.created","at":"2031-03-01T00:02:00Z","data":{"kind":"agent","name":"X"}}
`)
	labels := strings.NewReader(`
{"subject":"target","label":"benign","source":"operator","decision_at":{"early_15m":"2031-03-01T00:15:00Z","full":"2031-03-01T00:30:00Z"}}
`)
	ds, rowErrs, err := LoadReplayDataset(ReplayInput{EventsPath: "events.jsonl", Events: events, LabelsPath: "labels.jsonl", Labels: labels}, feature.BrandSet{}, "benign")
	if err != nil {
		t.Fatalf("LoadReplayDataset: %v (rowErrs=%v)", err, rowErrs)
	}
	pts := pointsByID(ds)["target"]

	if got := pts[SliceEarly15m].Features["linked_labelled_abusive_n"]; got != 0 {
		t.Errorf("early_15m (BEFORE poster's label event) linked_labelled_abusive_n = %v, want 0", got)
	}
	if got := pts[SliceFull].Features["linked_labelled_abusive_n"]; got != 1 {
		t.Errorf("full (AFTER poster's label event) linked_labelled_abusive_n = %v, want 1", got)
	}
}

// TestLoadReplayDataset_SliceOrderRejected proves fix round B2: a labels
// row whose first_send is chronologically AFTER its early_15m is
// rejected with Code "slice_order", naming the labels file and its own
// 1-based line number (fix round P1) — not the events file, and not some
// other line.
func TestLoadReplayDataset_SliceOrderRejected(t *testing.T) {
	events := strings.NewReader(`{"subject":"acct_1","type":"subject.created","at":"2031-01-01T00:00:00Z","data":{"channel":"signup"}}`)
	labels := strings.NewReader("\n" + `{"subject":"acct_1","label":"benign","source":"operator","decision_at":{"first_send":"2031-01-01T00:20:00Z","early_15m":"2031-01-01T00:10:00Z","full":"2031-01-01T01:00:00Z"}}`)
	_, rowErrs, err := LoadReplayDataset(ReplayInput{EventsPath: "events.jsonl", Events: events, LabelsPath: "labels.jsonl", Labels: labels}, feature.BrandSet{}, "benign")
	if err == nil {
		t.Fatalf("expected a slice_order RowError, got nil")
	}
	if len(rowErrs) != 1 || rowErrs[0].Code != "slice_order" {
		t.Fatalf("rowErrs = %v, want exactly one slice_order violation", rowErrs)
	}
	if rowErrs[0].Source != "labels.jsonl" || rowErrs[0].Line != 2 {
		t.Fatalf("rowErrs[0] = %+v, want Source=labels.jsonl Line=2", rowErrs[0])
	}
}

// TestLoadReplayDataset_DecisionAfterDeletionRejected proves fix round
// B2: a decision_at meaningfully (not just the 1ns "this event WAS the
// deletion" allowance) after the subject's own permanent deletion is
// rejected with Code "decision_after_deletion".
func TestLoadReplayDataset_DecisionAfterDeletionRejected(t *testing.T) {
	events := strings.NewReader(`
{"subject":"acct_1","type":"subject.created","at":"2031-01-01T00:00:00Z","data":{"channel":"signup"}}
{"subject":"acct_1","type":"subject.deleted","at":"2031-01-01T00:05:00Z","data":{"mode":"permanent"}}
`)
	labels := strings.NewReader(`{"subject":"acct_1","label":"abusive","source":"operator","decision_at":{"full":"2031-01-01T01:00:00Z"}}`)
	_, rowErrs, err := LoadReplayDataset(ReplayInput{EventsPath: "events.jsonl", Events: events, LabelsPath: "labels.jsonl", Labels: labels}, feature.BrandSet{}, "benign")
	if err == nil {
		t.Fatalf("expected a decision_after_deletion RowError, got nil")
	}
	if len(rowErrs) != 1 || rowErrs[0].Code != "decision_after_deletion" {
		t.Fatalf("rowErrs = %v, want exactly one decision_after_deletion violation", rowErrs)
	}
}

// TestLoadReplayDataset_MissingSliceBeforeAnyEvent proves fix round B2:
// a decision point strictly before the subject's first event produces NO
// Point for that slice (not a spurious all-zero-feature one) — and, end
// to end through eval.Run, that subject×slice comes back Unscored with
// ErrorCode "missing_slice": excluded from precision, counted as a miss
// for recall, never scored as a confident "low".
func TestLoadReplayDataset_MissingSliceBeforeAnyEvent(t *testing.T) {
	events := strings.NewReader(`
{"subject":"acct_1","type":"subject.created","at":"2031-01-01T00:10:00Z","data":{"channel":"signup"}}
{"subject":"acct_1","type":"resource.created","at":"2031-01-01T00:11:00Z","data":{"kind":"agent","name":"X"}}
`)
	// early_15m is set BEFORE the subject's very first event; full
	// resolves normally (auto: last event + 1ns).
	labels := strings.NewReader(`{"subject":"acct_1","label":"abusive","source":"operator","decision_at":{"early_15m":"2031-01-01T00:00:00Z"}}`)
	ds, rowErrs, err := LoadReplayDataset(ReplayInput{EventsPath: "events.jsonl", Events: events, LabelsPath: "labels.jsonl", Labels: labels}, feature.BrandSet{}, "benign")
	if err != nil {
		t.Fatalf("LoadReplayDataset: %v (rowErrs=%v)", err, rowErrs)
	}
	pts := ds.Subjects[0].Points
	if _, ok := pts[SliceEarly15m]; ok {
		t.Fatalf("early_15m has a Point despite zero qualifying events; want none")
	}
	if _, ok := pts[SliceFull]; !ok {
		t.Fatalf("full has no Point; want one (it resolves normally)")
	}

	rule := config.Rule{Name: "r", Mode: config.ModeAdvise, Scorer: "fake", Labels: []string{"benign", "abusive"}, BenignLabel: "benign", Threshold: 0.5, Inputs: []string{"resource_total"}}
	scorer := fake.New()
	run, err := Run(context.Background(), ds, rule, scorer, Options{Slice: SliceEarly15m})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(run.Verdicts) != 1 {
		t.Fatalf("len(Verdicts) = %d, want 1", len(run.Verdicts))
	}
	v := run.Verdicts[0]
	if !v.Unscored || v.ErrorCode != "missing_slice" {
		t.Fatalf("Verdict = %+v, want Unscored=true ErrorCode=missing_slice", v)
	}
	// Fix round T6: Metrics.MissingSliceCount tallies this end to end
	// through Run, not just computeMetrics in isolation.
	if run.Metrics.MissingSliceCount != 1 {
		t.Fatalf("Metrics.MissingSliceCount = %d, want 1", run.Metrics.MissingSliceCount)
	}
}

// TestLoadReplayDataset_SkippedEventTaintsWholeSubject is fix round T2's
// own acceptance test: acct_1 has one GOOD event and one event row that
// fails validation (an invalid type) — the subject must be
// HasSkippedEvents=true, and Run must score it as unscored with
// ErrorCode "skipped_events" rather than silently scoring it on the one
// event that DID survive.
func TestLoadReplayDataset_SkippedEventTaintsWholeSubject(t *testing.T) {
	events := strings.NewReader(`
{"subject":"acct_1","type":"subject.created","at":"2031-01-01T00:00:00Z","data":{"channel":"signup"}}
{"subject":"acct_1","type":"NOT VALID","at":"2031-01-01T00:01:00Z","data":{}}
{"subject":"acct_1","type":"resource.created","at":"2031-01-01T00:02:00Z","data":{"kind":"agent","name":"A"}}
`)
	labels := strings.NewReader(`{"subject":"acct_1","label":"benign","source":"operator","decision_at":{"full":"2031-01-01T01:00:00Z"}}`)
	ds, rowErrs, err := LoadReplayDataset(ReplayInput{EventsPath: "events.jsonl", Events: events, LabelsPath: "labels.jsonl", Labels: labels}, feature.BrandSet{}, "benign")
	if err == nil {
		t.Fatalf("expected a RowError for the invalid event type, got nil")
	}
	if len(rowErrs) != 1 || rowErrs[0].Code != "validate_failed" {
		t.Fatalf("rowErrs = %v, want exactly one validate_failed violation", rowErrs)
	}
	if len(ds.Subjects) != 1 {
		t.Fatalf("len(Subjects) = %d, want 1 (the skip is per-EVENT, not per-subject)", len(ds.Subjects))
	}
	if !ds.Subjects[0].HasSkippedEvents {
		t.Fatalf("HasSkippedEvents = false, want true")
	}

	rule := config.Rule{Name: "r", Mode: config.ModeAdvise, Scorer: "fake", Labels: []string{"benign", "abusive"}, BenignLabel: "benign", Threshold: 0.5, Inputs: []string{"resource_total"}}
	run, err := Run(context.Background(), ds, rule, fake.New(), Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(run.Verdicts) != 1 {
		t.Fatalf("len(Verdicts) = %d, want 1", len(run.Verdicts))
	}
	v := run.Verdicts[0]
	if !v.Unscored || v.ErrorCode != "skipped_events" {
		t.Fatalf("Verdict = %+v, want Unscored=true ErrorCode=skipped_events", v)
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
