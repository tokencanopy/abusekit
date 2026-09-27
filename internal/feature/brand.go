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
type BrandSet struct {
	entries [][]string // one pre-tokenized word sequence per name/alias
}

// NewBrandSet builds a BrandSet from entries, pre-tokenizing every name and
// alias once rather than per Matches call.
func NewBrandSet(entries []BrandEntry) BrandSet {
	var all [][]string
	for _, e := range entries {
		if words := tokenize(e.Name); len(words) > 0 {
			all = append(all, words)
		}
		for _, a := range e.Aliases {
			if words := tokenize(a); len(words) > 0 {
				all = append(all, words)
			}
		}
	}
	return BrandSet{entries: all}
}

// Matches reports whether text contains any brand's word sequence, per the
// word/token-boundary rule documented on BrandSet.
func (b BrandSet) Matches(text string) bool {
	if len(b.entries) == 0 {
		return false
	}
	words := tokenize(text)
	if len(words) == 0 {
		return false
	}
	for _, brand := range b.entries {
		if containsSequence(words, brand) {
			return true
		}
	}
	return false
}

// tokenize folds s through event.Skeleton (NFKC, confusables, lower-case,
// whitespace-collapse), strips zero-width characters Skeleton doesn't
// touch, and splits on whitespace plus the common name-obfuscation
// separators hyphen/underscore/period, dropping empty tokens. "pay-pal",
// "pay_pal", "pay.pal" and "pay pal" all tokenize identically to
// ["pay","pal"].
func tokenize(s string) []string {
	folded := stripZeroWidth(event.Skeleton(s))
	return strings.FieldsFunc(folded, func(r rune) bool {
		return unicode.IsSpace(r) || r == '-' || r == '_' || r == '.'
	})
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
