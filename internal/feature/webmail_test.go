package feature

import (
	"path/filepath"
	"testing"
)

func TestWebmailSet_Contains(t *testing.T) {
	w := NewWebmailSet([]string{"gmail.com", " Yahoo.COM ", "outlook.com"})
	tests := []struct {
		domain string
		want   bool
	}{
		{"gmail.com", true},
		{"GMAIL.COM", true},
		{" gmail.com ", true},
		{"yahoo.com", true},
		{"outlook.com", true},
		{"corp.example.test", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := w.Contains(tt.domain); got != tt.want {
			t.Errorf("Contains(%q) = %v, want %v", tt.domain, got, tt.want)
		}
	}
}

func TestWebmailSet_ZeroValueMatchesNothing(t *testing.T) {
	if (WebmailSet{}).Contains("gmail.com") {
		t.Errorf("the zero WebmailSet must never match anything")
	}
}

func TestLoadWebmailFile_MissingFile(t *testing.T) {
	if _, err := LoadWebmailFile("does-not-exist.yaml"); err == nil {
		t.Fatalf("expected an error for a missing file")
	}
}

func TestLoadWebmailFile_Shipped(t *testing.T) {
	w, err := LoadWebmailFile(filepath.Join(repoRoot(t), "config", "webmail.yaml"))
	if err != nil {
		t.Fatalf("LoadWebmailFile: %v", err)
	}
	mustContain := []string{"gmail.com", "yahoo.com", "outlook.com", "icloud.com"}
	for _, d := range mustContain {
		if !w.Contains(d) {
			t.Errorf("shipped config/webmail.yaml: Contains(%q) = false, want true", d)
		}
	}
	if w.Contains("corp.example.test") {
		t.Errorf("shipped config/webmail.yaml: Contains(%q) = true, want false", "corp.example.test")
	}
}
