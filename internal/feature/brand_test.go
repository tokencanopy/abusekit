package feature

import "testing"

// mechanismBrands deliberately includes some of the generic single-word
// brands the shipped config/brands.yaml excludes (apple, amazon, stripe) —
// this file tests the MATCHING MECHANISM's word-boundary safety
// independent of which brands are curated into the real list, so a
// generic word makes the false-positive tests meaningful even though
// production would never carry it.
func mechanismBrands() BrandSet {
	return NewBrandSet([]BrandEntry{
		{Name: "PayPal", Aliases: []string{"Pay Pal"}},
		{Name: "Apple"},
		{Name: "Amazon"},
		{Name: "Stripe"},
		{Name: "Wells Fargo"},
		{Name: "Bank of America"},
	})
}

func TestBrandSet_WordBoundarySafety(t *testing.T) {
	brands := mechanismBrands()
	tests := []struct {
		name string
		text string
		want bool
	}{
		// True positives: exact word, any case, any of the common
		// obfuscation separators.
		{"exact word", "PayPal", true},
		{"all caps", "PAYPAL SUPPORT TEAM", true},
		{"trailing word", "Notifications from PayPal", true},
		{"hyphen-obfuscated", "pay-pal-verify", true},
		{"underscore-obfuscated", "pay_pal_support", true},
		{"dot-obfuscated", "pay.pal.verify", true},
		{"alias spelled with a space", "Pay Pal Rewards", true},
		{"multi-word brand, natural spacing", "Wells Fargo Alerts", true},
		{"multi-word brand, hyphenated", "wells-fargo-security", true},
		{"three-word brand", "Your Bank of America statement", true},

		// False positives a bare substring check would produce — must NOT
		// match now that matching is word/token-boundary-aware.
		{"pineapple", "Pineapple Analytics Bot", false},
		{"grapple", "Grapple Sync Agent", false},
		{"applebee's", "Applebee's Rewards Bot", false},
		{"amazonas", "Amazonas Logistics", false},
		{"striped", "Striped Shirt Co", false},
		{"no separator glue", "paypalsupport", false}, // documented v0 limitation

		// Zero-width obfuscation must not defeat the tokenizer.
		{"zero-width space inside the brand word", "pay​pal", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := brands.Matches(tt.text); got != tt.want {
				t.Errorf("Matches(%q) = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}

func TestBrandSet_EmptyMatchesNothing(t *testing.T) {
	if (BrandSet{}).Matches("PayPal") {
		t.Errorf("the zero BrandSet must never match anything")
	}
}

func TestBrandSet_EmptyTextNeverMatches(t *testing.T) {
	if mechanismBrands().Matches("") {
		t.Errorf("empty text must never match")
	}
}

func TestTokenize(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"Wells Fargo", []string{"wells", "fargo"}},
		{"wells-fargo", []string{"wells", "fargo"}},
		{"wells_fargo", []string{"wells", "fargo"}},
		{"wells.fargo", []string{"wells", "fargo"}},
		{"  Wells   Fargo  ", []string{"wells", "fargo"}},
		{"pay​pal", []string{"paypal"}},
		{"", nil},
	}
	for _, tt := range tests {
		got := tokenize(tt.in)
		if !equalStrings(got, tt.want) {
			t.Errorf("tokenize(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestContainsSequence(t *testing.T) {
	tests := []struct {
		haystack, needle []string
		want             bool
	}{
		{[]string{"wells", "fargo", "alerts"}, []string{"wells", "fargo"}, true},
		{[]string{"my", "wells", "fargo", "account"}, []string{"wells", "fargo"}, true},
		{[]string{"wellsfargo"}, []string{"wells", "fargo"}, false},
		{[]string{"fargo", "wells"}, []string{"wells", "fargo"}, false}, // order matters
		{[]string{"paypal"}, []string{"paypal"}, true},
		{[]string{"pineapple"}, []string{"apple"}, false},
		{nil, []string{"a"}, false},
		{[]string{"a"}, nil, false},
	}
	for _, tt := range tests {
		if got := containsSequence(tt.haystack, tt.needle); got != tt.want {
			t.Errorf("containsSequence(%v, %v) = %v, want %v", tt.haystack, tt.needle, got, tt.want)
		}
	}
}

func TestLoadBrandsFile_MissingFile(t *testing.T) {
	if _, err := LoadBrandsFile("does-not-exist.yaml"); err == nil {
		t.Fatalf("expected an error for a missing file")
	}
}
