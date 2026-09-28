package eval

import (
	"strings"
	"testing"

	"github.com/tokencanopy/abusekit/internal/feature"
)

// TestLoadSnapshotCorpus_Happy checks the basic label-snapshot shape
// parses and lands under SliceFull.
func TestLoadSnapshotCorpus_Happy(t *testing.T) {
	in := strings.NewReader(`
{"id":"c1","input":{"features":{"subject_age_h":1.0}},"label":"benign","split":"train","source":"operator"}
{"id":"c2","input":{"text":{"subject_line_skeleton":["hello"]}},"label":"abusive"}
`)
	ds, rowErrs, err := LoadSnapshotCorpus(in)
	if err != nil {
		t.Fatalf("LoadSnapshotCorpus: %v (rowErrs=%v)", err, rowErrs)
	}
	if len(rowErrs) != 0 {
		t.Fatalf("rowErrs = %v, want none", rowErrs)
	}
	if len(ds.Subjects) != 2 {
		t.Fatalf("len(Subjects) = %d, want 2", len(ds.Subjects))
	}
	if ds.Replay {
		t.Fatalf("Replay = true, want false for a label-snapshot corpus")
	}
	pt, ok := ds.Subjects[0].Points[SliceFull]
	if !ok {
		t.Fatalf("subject 0 has no SliceFull point")
	}
	if pt.Features["subject_age_h"] != 1.0 {
		t.Errorf("subject_age_h = %v, want 1.0", pt.Features["subject_age_h"])
	}
}

// TestLoadSnapshotCorpus_SchemaRejection exercises every row-level
// validation rule and checks the reported line number matches the
// actually-bad line (task brief: "report per-row errors with the line
// number").
func TestLoadSnapshotCorpus_SchemaRejection(t *testing.T) {
	in := strings.NewReader(`{"id":"ok","input":{"features":{"x":1}},"label":"benign"}
not json at all
{"id":"","input":{"features":{"x":1}},"label":"benign"}
{"id":"dup","input":{"features":{"x":1}},"label":"benign"}
{"id":"dup","input":{"features":{"x":1}},"label":"benign"}
{"id":"nolabel","input":{"features":{"x":1}}}
{"id":"badsplit","input":{"features":{"x":1}},"label":"benign","split":"bogus"}
{"id":"empty","input":{},"label":"benign"}
`)
	_, rowErrs, err := LoadSnapshotCorpus(in)
	if err == nil {
		t.Fatalf("expected a *SchemaError, got nil")
	}
	wantLines := map[int]bool{2: true, 3: true, 5: true, 6: true, 7: true, 8: true}
	gotLines := map[int]bool{}
	for _, re := range rowErrs {
		gotLines[re.Line] = true
	}
	for line := range wantLines {
		if !gotLines[line] {
			t.Errorf("expected a RowError at line %d, got none (rowErrs=%v)", line, rowErrs)
		}
	}
	if len(rowErrs) != len(wantLines) {
		t.Errorf("got %d row errors %v, want exactly %d", len(rowErrs), rowErrs, len(wantLines))
	}
}

// TestLoadReplayDataset_SchemaRejection exercises the replay pair's own
// validation: a bad event (fails internal/event.Validate), a label
// naming an unknown subject, and an empty decision_at.
func TestLoadReplayDataset_SchemaRejection(t *testing.T) {
	events := strings.NewReader(`
{"subject":"acct_1","type":"subject.created","at":"2031-01-01T00:00:00Z","data":{}}
{"subject":"acct_1","type":"NOT VALID TYPE!!","at":"2031-01-01T00:01:00Z","data":{}}
`)
	labels := strings.NewReader(`
{"subject":"acct_1","label":"benign","source":"operator","decision_at":{"full":"2031-01-01T01:00:00Z"}}
{"subject":"acct_missing","label":"abusive","source":"operator","decision_at":{"full":"2031-01-01T01:00:00Z"}}
{"subject":"acct_1","label":"benign","source":"operator","decision_at":{}}
`)
	_, rowErrs, err := LoadReplayDataset(ReplayInput{EventsPath: "events.jsonl", Events: events, LabelsPath: "labels.jsonl", Labels: labels}, feature.BrandSet{}, "benign")
	if err == nil {
		t.Fatalf("expected a *SchemaError, got nil")
	}
	if len(rowErrs) == 0 {
		t.Fatalf("expected row errors, got none")
	}
	var sawBadEvent, sawUnknownSubject, sawDuplicateSubject bool
	for _, re := range rowErrs {
		msg := re.Err.Error()
		if strings.Contains(msg, "bad_type") {
			sawBadEvent = true
		}
		if strings.Contains(msg, "zero events") {
			sawUnknownSubject = true
		}
		if strings.Contains(msg, "duplicate subject") {
			sawDuplicateSubject = true
		}
	}
	if !sawBadEvent {
		t.Errorf("expected a bad_type RowError, got %v", rowErrs)
	}
	if !sawUnknownSubject {
		t.Errorf("expected a 'zero events' RowError for acct_missing, got %v", rowErrs)
	}
	if !sawDuplicateSubject {
		t.Errorf("expected a 'duplicate subject' RowError for the second acct_1 label row, got %v", rowErrs)
	}
}

func TestLoadReplayDataset_MissingDecisionAtField(t *testing.T) {
	events := strings.NewReader(`{"subject":"acct_1","type":"subject.created","at":"2031-01-01T00:00:00Z","data":{}}`)
	labels := strings.NewReader(`{"subject":"acct_1","label":"benign","source":"operator"}`)
	_, rowErrs, err := LoadReplayDataset(ReplayInput{EventsPath: "events.jsonl", Events: events, LabelsPath: "labels.jsonl", Labels: labels}, feature.BrandSet{}, "benign")
	if err == nil || len(rowErrs) == 0 {
		t.Fatalf("expected a RowError for a labels row with no decision_at at all")
	}
}
