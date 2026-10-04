package main

import (
	"github.com/tokencanopy/abusekit/internal/feature"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScoreRejectsFlatFeatures(t *testing.T) {
	p := filepath.Join(t.TempDir(), "input.jsonl")
	if err := os.WriteFile(p, []byte(`{"id":"example","input":{"features":{"key_total":3}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	_, err = readScoreRows(f)
	if err == nil || !strings.Contains(err.Error(), "feature_renamed: key_total is now core.credential_total") {
		t.Fatalf("wrong error: %v", err)
	}
}

func TestNoProductionBeforeRename(t *testing.T) {
	if err := requireProductionKeySpace("production", "flat-v0"); err == nil {
		t.Fatal("pre-rename production accepted")
	}
	if err := requireProductionKeySpace("production", feature.KeySpace); err != nil {
		t.Fatal(err)
	}
	for _, adapter := range []string{"gemini", "jev", "laya"} {
		if _, err := os.Stat(filepath.Join(repoRoot(t), "internal", "model", adapter)); err == nil && feature.KeySpace != "ns-v1" {
			t.Fatalf("adapter %s landed before rename", adapter)
		}
	}
}

func TestScoreRejectsTrailingJSON(t *testing.T) {
	p := filepath.Join(t.TempDir(), "input.jsonl")
	if err := os.WriteFile(p, []byte(`{"id":"example","input":{"features":{"core.subject_age_h":3}}} {"input":{"features":{"subject_age_h":4}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err = readScoreRows(f); err == nil {
		t.Fatal("accepted second JSON value on a score line")
	}
}
