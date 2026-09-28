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
		{"no separator glue, all lowercase", "paypalsupport", false}, // documented v0 limitation: no case transition to split on

		// Zero-width obfuscation must not defeat the tokenizer.
		{"zero-width space inside the brand word", "pay​pal", true},

		// Glued compounds written with each component capitalized (R6
		// round 2): tokenizeCamel splits at the lower->upper transition,
		// so these match even with no separator at all.
		{"glued, each word capitalized", "PayPalSupport", true},
		{"glued multi-word brand name", "WellsFargo", true},
		{"glued three-word brand name", "BankOfAmerica", true},

		// R6 round 2: an integration-token word ANYWHERE in the candidate
		// text suppresses the whole match, even for a brand (PayPal) that
		// was never on the S3 exclusion list — proven necessary:
		// "PayPal integration" and "Coinbase Commerce webhook"-shaped
		// resource names are common, legitimate SaaS-integration names.
		{"integration-token gate suppresses an otherwise-clean match", "PayPal integration", false},
		{"integration-token gate is not positional", "Apple Calendar Sync", false},
		{"integration-token gate does not fire without one", "PayPal Rewards Program", true},
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

// TestCanonicalise is D1 round 3's own unit test for the shared fold: it
// must fold every "i" (already lower-cased by the time tokenize calls it)
// to 'l', with no other side effects.
func TestCanonicalise(t *testing.T) {
	tests := []struct{ in, want string }{
		{"microsoft", "mlcrosoft"},
		{"integration", "lntegratlon"},
		{"america", "amerlca"},
		{"paypal", "paypal"}, // no "i" at all: unchanged
		{"", ""},
	}
	for _, tt := range tests {
		if got := canonicalise(tt.in); got != tt.want {
			t.Errorf("canonicalise(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestBuildIntegrationTokens confirms integrationTokens is built THROUGH
// canonicalise, not from its own hand-typed already-folded strings (D1
// round 3) — so an all-caps candidate word that tokenize() also
// canonicalises ("INTEGRATION" -> "lntegratlon") is recognized.
func TestBuildIntegrationTokens(t *testing.T) {
	for _, raw := range integrationTokenWords {
		folded := canonicalise(raw)
		if _, ok := integrationTokens[folded]; !ok {
			t.Errorf("integrationTokens is missing %q (canonicalised from %q)", folded, raw)
		}
	}
	if _, ok := integrationTokens["lntegratlon"]; !ok {
		t.Errorf(`integrationTokens["lntegratlon"] missing — an all-caps "INTEGRATION" candidate would bypass the gate`)
	}
}

func TestTokenizeCamel(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"WellsFargo", []string{"wells", "fargo"}},
		{"BankOfAmerica", []string{"bank", "of", "amerlca"}}, // canonicalise (D1 round 3) folds the remaining lower-case i in "america" too — see TestCanonicalise
		{"PayPalSupport", []string{"pay", "pal", "support"}},
		{"paypal", []string{"paypal"}},              // all lowercase: no transition, unchanged
		{"Paypal", []string{"paypal"}},              // capitalized only at the start: no INTERNAL transition
		{"USPS", []string{"usps"}},                  // all caps: no lower->upper transition anywhere
		{"Wells Fargo", []string{"wells", "fargo"}}, // already separated: unaffected
		{"", nil},
	}
	for _, tt := range tests {
		got := tokenizeCamel(tt.in)
		if !equalStrings(got, tt.want) {
			t.Errorf("tokenizeCamel(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
