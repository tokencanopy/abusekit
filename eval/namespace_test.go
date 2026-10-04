package eval

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCorpusRejectsFlatNamesWithReplacement(t *testing.T) {
	_, rows, err := LoadSnapshotCorpus(strings.NewReader(`{"id":"example","input":{"features":{"subject_age_h":2}},"label":"benign"}`))
	if err == nil || len(rows) != 1 || rows[0].Code != "feature_renamed" || !strings.Contains(err.Error(), "core.subject_age_h") {
		t.Fatalf("want explicit renamed feature error, got rows=%v err=%v", rows, err)
	}
}
func TestCorpusRequiresCurrentKeySpace(t *testing.T) {
	_, _, err := LoadSnapshotCorpus(strings.NewReader(`{"id":"example","input":{"features":{"core.subject_age_h":2}},"label":"benign"}`))
	if err == nil {
		t.Fatal("accepted corpus without key-space marker")
	}
}
func TestCassetteRejectsOldKeySpace(t *testing.T) {
	p := filepath.Join(t.TempDir(), "old.json")
	if err := os.WriteFile(p, []byte(`{"entries":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCassette(p); err == nil {
		t.Fatal("accepted cassette without key space")
	}
}

func TestCorpusRejectsTrailingJSON(t *testing.T) {
	_, _, err := LoadSnapshotCorpus(strings.NewReader(`{"feature_key_space":"ns-v1","id":"example","input":{"features":{"core.subject_age_h":2}},"label":"benign"} {"input":{"features":{"subject_age_h":3}}}`))
	if err == nil {
		t.Fatal("accepted second JSON value on a corpus line")
	}
}
