package feature

import "testing"

func TestWebmailSet_ContainsIsCaseAndWhitespaceInsensitive(t *testing.T) {
	w := NewWebmailSet([]string{"gmail.com", " Outlook.COM "})
	tests := []struct {
		domain string
		want   bool
	}{
		{"gmail.com", true},
		{"GMAIL.COM", true},
		{"  gmail.com  ", true},
		{"outlook.com", true},
		{"corp-example.test", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := w.Contains(tt.domain); got != tt.want {
			t.Errorf("Contains(%q) = %v, want %v", tt.domain, got, tt.want)
		}
	}
}

func TestWebmailSet_EmptyMatchesNothing(t *testing.T) {
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
	w, err := LoadWebmailFile("../../config/webmail.yaml")
	if err != nil {
		t.Fatalf("LoadWebmailFile(config/webmail.yaml): %v", err)
	}
	// A representative sample from every provider family S8 requires,
	// including the country-variant domains.
	for _, domain := range []string{
		"gmail.com", "outlook.com", "hotmail.com", "yahoo.com",
		"hotmail.co.uk", "outlook.fr", "live.co.uk", "yahoo.fr",
		"yahoo.de", "yahoo.co.jp", "mail.ru", "gmx.de", "t-online.de",
		"libero.it",
	} {
		if !w.Contains(domain) {
			t.Errorf("config/webmail.yaml must list %q", domain)
		}
	}
}
