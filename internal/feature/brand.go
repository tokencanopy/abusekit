package feature

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"

	"github.com/tokencanopy/abusekit/internal/event"
)

// BrandEntry is one curated brand name plus optional spelling aliases
// (e.g. "Pay Pal" for PayPal) — config/brands.yaml's shape (S3 fix round).
type BrandEntry struct {
	Name    string
	Aliases []string
}

// BrandSet is a loaded, ready-to-match set of brand names (config/brands.yaml).
// The zero value matches nothing — a caller that hasn't loaded a brand list
// simply gets name_brand_match held at 0, never a panic or an error.
//
// Matching is WORD/TOKEN-boundary-aware (S3 fix round — the original v0
// substring check produced both false positives, "Pineapple"/"Grapple"/
// "Applebee's"/"Amazonas"/"Striped" all matching a bare "apple"/"amazon"/
// "stripe" substring, and false negatives, "Wells Fargo"/"Bank of America"
// never matching their smashed-together "wellsfargo"/"bankofamerica"
// dictionary entries because a real display name has a space the entry
// didn't). A candidate string and every brand name/alias are both folded
// through internal/event.Skeleton (NFKC, confusables, lower-case) and then
// split into words on whitespace and common separators (-, _, .) — so
// "Wells Fargo", "wells-fargo" and "WELLS FARGO" all tokenize to the same
// ["wells","fargo"], and "PAYPAL SUPPORT" tokenizes to ["paypal",
// "support"]. A brand's word sequence must appear as a CONTIGUOUS run of
// the candidate's words; a single-word brand must match a whole word, not
// a substring of one — "Pineapple" is one token that is never equal to
// "apple", so it never matches even if "apple" were still in the list.
//
// This does not catch every obfuscation (e.g. "paypalsupport" glued into
// one word with no separator tokenizes as a single word that doesn't
// equal "paypal"): trading a little recall for the word-boundary safety
// two independent reviews required is the deliberate v0 choice.
// brandWords is one pre-tokenized name/alias word sequence, tagged with
// the CANONICAL brand name (BrandEntry.Name) it belongs to — [S2b]:
// subject_brand_match needs to count DISTINCT brands, so a match has to
// be traceable back to which brand identity fired, not just "something
// matched" (Matches' original bool-only contract).
type brandWords struct {
	words []string
	name  string
}

type BrandSet struct {
	entries []brandWords
}

// NewBrandSet builds a BrandSet from entries, pre-tokenizing every name and
// alias once rather than per Matches call.
func NewBrandSet(entries []BrandEntry) BrandSet {
	var all []brandWords
	for _, e := range entries {
		if words := tokenize(e.Name); len(words) > 0 {
			all = append(all, brandWords{words, e.Name})
		}
		for _, a := range e.Aliases {
			if words := tokenize(a); len(words) > 0 {
				all = append(all, brandWords{words, e.Name})
			}
		}
	}
	return BrandSet{entries: all}
}

// integrationTokens are words whose presence ANYWHERE in a candidate text
// mean it is very likely naming a legitimate third-party integration/
// webhook/sync feature ("Stripe Webhook Relay", "Google Calendar Sync",
// "PayPal integration", "Coinbase Commerce webhook") rather than
// impersonating the brand it mentions — R6 round 2. Applied to every
// brand uniformly (not just the generic single-word ones re-included
// below), since an already-shipped, unambiguous brand like PayPal or
// Coinbase needs exactly the same protection ("PayPal integration" is a
// reviewed false-positive case on its own).
//
// Checked against the WHOLE tokenized candidate text, not just the
// token(s) immediately touching the brand match: the reviewed
// "Google Calendar Sync" false positive places its integration keyword
// ("sync") two words away from the brand mention ("google"), not
// adjacent to it, so a strictly positional immediate-neighbor check would
// miss it. A resource/subject-line name is short by construction
// (internal/event's redaction schema caps it at 200 bytes), so "anywhere
// in this text" and "adjacent to this brand mention" coincide for every
// case this rule exists to catch, without the fragility of picking a
// fixed adjacency window that happens to cover today's examples but not
// tomorrow's.
//
// Documented tradeoff: "tracking" suppresses a genuine DHL/USPS
// impersonation lure too ("DHL Package Tracking") — accepted
// deliberately, since the same rule can't special-case one brand without
// reopening the false positive it exists to close for every other one.
//
// integrationTokenWords lists each word in its natural spelling;
// integrationTokens (built by buildIntegrationTokens, below) canonicalises
// every one of them the IDENTICAL way tokenize() canonicalises brand
// definitions and candidates (D1 round 3) — otherwise an all-caps
// candidate ("PAYPAL INTEGRATION") folds its own "INTEGRATION" to
// "lntegratlon" (event.Skeleton's I->l fold) while this map's hand-typed
// "integration" key never would, letting the gate silently miss its own
// all-caps obfuscation.
var integrationTokenWords = []string{
	"integration", "webhook", "sync", "relay", "notifier",
	"tracking", "bot", "connector", "api", "import", "export",
}

var integrationTokens = buildIntegrationTokens()

func buildIntegrationTokens() map[string]struct{} {
	out := make(map[string]struct{}, len(integrationTokenWords))
	for _, w := range integrationTokenWords {
		out[canonicalise(w)] = struct{}{}
	}
	return out
}

func hasIntegrationToken(words []string) bool {
	for _, w := range words {
		if _, ok := integrationTokens[w]; ok {
			return true
		}
	}
	return false
}

// Matches reports whether text contains any brand's word sequence, per the
// word/token-boundary rule documented on BrandSet, gated by
// integrationTokens (R6 round 2). Tries both the plain (separator-only)
// tokenization and the camelCase-aware one (see tokenizeCamel) — a brand
// whose own correctly-cased spelling already contains an internal
// lower->upper transition (PayPal, FedEx) still matches its plain
// single-token form via the FIRST pass; a glued compound written with
// each component capitalized but no separator (WellsFargo, PayPalSupport)
// only tokenizes into the right words via the SECOND. Checking both
// independently — rather than only ever using the camelCase-aware one —
// is deliberate: camelCase-splitting a brand's OWN canonical spelling at
// definition time (NewBrandSet never does this) would turn "PayPal" into
// a needle of ["pay","pal"], which would stop matching a candidate that
// simply writes it in plain lower-case ("paypal") with no case transition
// to split on at all.
func (b BrandSet) Matches(text string) bool {
	if len(b.entries) == 0 {
		return false
	}
	return len(b.MatchedBrandNames(text)) > 0
}

// MatchedBrandNames [S2b] returns the set of DISTINCT curated brand names
// (BrandEntry.Name — an entry matched via an alias still reports its
// canonical name, never the alias text) whose word sequence appears in
// text, per Matches' own word/token-boundary rule and integration-token
// gate. Used by subject_brand_match to count how many different brands a
// subject_line mentions, not merely whether any did — Matches itself is
// now defined in terms of this (len(...) > 0), so the two can never drift
// apart on which candidates count as a match.
//
// Returns nil (never a non-nil empty map) when nothing matched, matching
// Go's normal "ranging over a nil map is a no-op, len(nil map) is 0"
// idiom — callers never need a special nil check before iterating.
func (b BrandSet) MatchedBrandNames(text string) map[string]struct{} {
	if len(b.entries) == 0 {
		return nil
	}
	out := b.matchedNames(tokenize(text))
	for name := range b.matchedNames(tokenizeCamel(text)) {
		if out == nil {
			out = make(map[string]struct{})
		}
		out[name] = struct{}{}
	}
	return out
}

func (b BrandSet) matchedNames(words []string) map[string]struct{} {
	if len(words) == 0 || hasIntegrationToken(words) {
		return nil
	}
	var out map[string]struct{}
	for _, brand := range b.entries {
		if containsSequence(words, brand.words) {
			if out == nil {
				out = make(map[string]struct{})
			}
			out[brand.name] = struct{}{}
		}
	}
	return out
}

// tokenize folds s through event.Skeleton (NFKC, confusables, lower-case,
// whitespace-collapse), strips zero-width characters Skeleton doesn't
// touch, canonicalises the I/l confusable the rest of the way (see
// canonicalise), and splits on whitespace plus the common
// name-obfuscation separators hyphen/underscore/period, dropping empty
// tokens. "pay-pal", "pay_pal", "pay.pal" and "pay pal" all tokenize
// identically to ["pay","pal"].
//
// This is the SAME function NewBrandSet uses to tokenize every brand
// definition and Matches uses to tokenize every candidate — canonicalise
// is applied to both sides identically for exactly that reason (D1 round
// 3): a fold that only ran on one side (or was hand-applied ad hoc to
// integrationTokens' literal strings instead of through this shared
// path) silently reintroduced the exact divergence it was meant to close.
func tokenize(s string) []string {
	folded := canonicalise(stripZeroWidth(event.Skeleton(s)))
	return strings.FieldsFunc(folded, func(r rune) bool {
		return unicode.IsSpace(r) || r == '-' || r == '_' || r == '.'
	})
}

// canonicalise folds every remaining lower-case "i" to 'l' (D1 round 3).
// event.Skeleton already folds an UPPER-case "I" (and dotless "ı") to 'l'
// pre-lowercase, specifically to catch "PayPaI"-style impersonation — but
// it never touches an ORDINARY lower-case "i", since by itself that's
// just a letter, not a lookalike. That asymmetry is exactly the bug this
// closes: a brand written in its natural mixed-case spelling
// ("Microsoft", "Netflix", "Coinbase", "Binance", "Bank of America" — all
// with a lower-case i) tokenizes with that i untouched, while the
// IDENTICAL brand mentioned in a candidate written in ALL CAPS
// ("MICROSOFT SUPPORT") has its i already folded to 'l' by Skeleton
// before this ever runs — so the two sides silently diverged. Folding
// every remaining i to 'l' here, on BOTH sides (brand definitions via
// NewBrandSet, candidates via Matches, and integrationTokens via
// buildIntegrationTokens — all three go through this same function),
// makes them converge again: "integration" and an all-caps candidate's
// "INTEGRATION" (which Skeleton already turns into "lntegratlon") now
// compare equal too.
func canonicalise(s string) string {
	return strings.ReplaceAll(s, "i", "l")
}

// tokenizeCamel is tokenize plus one more split point (R6 round 2): a
// boundary is inserted at every transition from a lower-case letter or
// digit to an upper-case letter, computed against text's ORIGINAL casing
// and applied BEFORE event.Skeleton — which lower-cases everything, and
// so would otherwise destroy the very case information this needs — so a
// glued compound written the conventional way, with each word
// capitalized and no separator ("WellsFargo", "BankOfAmerica",
// "PayPalSupport"), tokenizes into the same words as its separated form.
//
// This can only ever produce MORE tokens than tokenize, never fewer: a
// string with no such transition (all lower-case, all upper-case, or
// capitalized only at its very first letter — the ordinary way a single
// brand word is written) tokenizes identically either way. Matches tries
// both (see its own doc comment) rather than using this exclusively, so a
// brand whose own correct spelling already contains an internal
// transition (PayPal, FedEx) still matches a plain lower-case candidate
// through the OTHER tokenization.
func tokenizeCamel(s string) []string {
	return tokenize(insertCamelBoundaries(s))
}

// insertCamelBoundaries inserts a literal space immediately before every
// rune that is upper-case and immediately follows a lower-case letter or a
// digit, in s's original casing. A single inserted space is safe to feed
// into event.Skeleton afterward — Skeleton collapses whitespace runs but
// never removes a lone space between two words.
func insertCamelBoundaries(s string) string {
	runes := []rune(s)
	if len(runes) == 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(runes) + 8)
	for i, r := range runes {
		if i > 0 {
			prev := runes[i-1]
			if unicode.IsUpper(r) && (unicode.IsLower(prev) || unicode.IsDigit(prev)) {
				b.WriteRune(' ')
			}
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Zero-width and other invisible formatting characters stripZeroWidth
// removes, spelled as numeric rune literals (not literal Unicode escapes)
// so this file's bytes stay unambiguous regardless of editor/tool
// encoding: zeroWidthSpace (U+200B), zeroWidthNonJoiner (U+200C),
// zeroWidthJoiner (U+200D), zeroWidthNoBreakSpace (U+FEFF, also the UTF-8
// BOM), wordJoiner (U+2060).
const (
	zeroWidthSpace        = 0x200B
	zeroWidthNonJoiner    = 0x200C
	zeroWidthJoiner       = 0x200D
	zeroWidthNoBreakSpace = 0xFEFF
	wordJoiner            = 0x2060
)

// stripZeroWidth removes the invisible formatting characters listed above
// that would otherwise silently split a brand name's letters apart (e.g.
// "pay" + zeroWidthSpace + "pal") and defeat both the word-boundary
// tokenizer above and a naive substring check alike. Kept local to this
// package rather than folded into event.Skeleton itself: Skeleton is
// shared by subject_line matching too, and this fix round's scope is
// brand matching specifically.
func stripZeroWidth(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case zeroWidthSpace, zeroWidthNonJoiner, zeroWidthJoiner, zeroWidthNoBreakSpace, wordJoiner:
			return -1
		}
		return r
	}, s)
}

// containsSequence reports whether needle appears as a contiguous run
// inside haystack.
func containsSequence(haystack, needle []string) bool {
	if len(needle) == 0 || len(needle) > len(haystack) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j, w := range needle {
			if haystack[i+j] != w {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// rawBrandsFile / rawBrand mirror config/brands.yaml's shape.
type rawBrandsFile struct {
	Brands []rawBrand `yaml:"brands"`
}

type rawBrand struct {
	Name    string   `yaml:"name"`
	Aliases []string `yaml:"aliases"`
}

// LoadBrandsFile reads and parses a config/brands.yaml-shaped file (S3: the
// curated brand list is data, not a Go literal, so it can be extended and
// reviewed like any other config, independent of a code release).
func LoadBrandsFile(path string) (BrandSet, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return BrandSet{}, fmt.Errorf("feature: read brands file %s: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	var raw rawBrandsFile
	if err := dec.Decode(&raw); err != nil {
		return BrandSet{}, fmt.Errorf("feature: parse brands file %s: %w", path, err)
	}
	entries := make([]BrandEntry, 0, len(raw.Brands))
	for _, rb := range raw.Brands {
		if rb.Name == "" {
			return BrandSet{}, fmt.Errorf("feature: %s: a brands entry is missing name", path)
		}
		entries = append(entries, BrandEntry{Name: rb.Name, Aliases: rb.Aliases})
	}
	return NewBrandSet(entries), nil
}

// MergeBrandSets [S2b] combines the entries of several BrandSets into one
// — F5's "optional second brands file" requirement: an operator can keep
// a private brand list outside this public repo (config `brands_extra` /
// `--brands-extra`, cmd/abusekit) and have it matched alongside the
// shipped public config/brands.yaml, without LoadBrandsFile itself needing
// to know how many files it's loading. A zero-value/empty argument
// contributes nothing (MergeBrandSets(a, BrandSet{}) == a in behavior),
// so a caller can always merge in an optional set unconditionally rather
// than branching on whether it was actually loaded.
func MergeBrandSets(sets ...BrandSet) BrandSet {
	var all []brandWords
	for _, s := range sets {
		all = append(all, s.entries...)
	}
	return BrandSet{entries: all}
}
