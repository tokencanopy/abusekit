package event

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Skeleton reduces s to a normalized form for homoglyph-resistant matching
// (design §5, "Homoglyph names → NFKC + confusables skeleton before brand
// matching"). It is computed for `resource.created`/`resource.deleted`'s
// `name` field (stored as `name_skeleton`) and for `content.sent`'s
// `subject_line` field (stored as `subject_line_skeleton`), which
// `internal/model/local`'s brand-match feature and the `lure_similarity`
// rule's text input both key off.
//
// The pipeline is: NFKC normalize (folds full-width forms, ligatures and
// most compatibility variants) -> strip combining marks left by a
// subsequent NFD pass (drops accents so "café" and "cafe" collapse) ->
// lower-case -> map each remaining rune through a small curated
// confusables table (Cyrillic/Greek Latin-lookalikes, digit-for-letter
// leetspeak) -> collapse runs of whitespace.
//
// This is a v0 placeholder, not a UTS #39 confusables implementation: the
// table below covers the lookalikes seen in real brand-impersonation
// attempts, not the full Unicode confusables list. Extend confusablesTable
// as new lookalikes show up in labelled examples; it is data, not logic,
// so extending it never touches callers.
func Skeleton(s string) string {
	folded := norm.NFKC.String(s)
	folded = stripDiacritics(folded)
	folded = strings.ToLower(folded)

	var b strings.Builder
	b.Grow(len(folded))
	lastWasSpace := false
	for _, r := range folded {
		if mapped, ok := confusablesTable[r]; ok {
			r = mapped
		}
		if unicode.IsSpace(r) {
			if lastWasSpace {
				continue
			}
			lastWasSpace = true
			b.WriteByte(' ')
			continue
		}
		lastWasSpace = false
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// stripDiacritics decomposes to NFD and drops combining marks (Unicode
// category Mn), so accented Latin letters collapse to their base letter
// before the confusables map runs.
func stripDiacritics(s string) string {
	decomposed := norm.NFD.String(s)
	var b strings.Builder
	b.Grow(len(decomposed))
	for _, r := range decomposed {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		b.WriteRune(r)
	}
	return norm.NFC.String(b.String())
}

// confusablesTable maps a curated set of Latin-lookalike code points (plus
// common leetspeak digit substitutions) to the Latin letter an attacker is
// impersonating. Keys are the *decomposed* rune the confusable maps to
// after NFKC+NFD folding above, not necessarily the original precomposed
// character.
var confusablesTable = map[rune]rune{
	// Cyrillic lookalikes.
	'а': 'a', // U+0430 CYRILLIC SMALL LETTER A
	'е': 'e', // U+0435 CYRILLIC SMALL LETTER IE
	'о': 'o', // U+043E CYRILLIC SMALL LETTER O
	'р': 'p', // U+0440 CYRILLIC SMALL LETTER ER
	'с': 'c', // U+0441 CYRILLIC SMALL LETTER ES
	'у': 'y', // U+0443 CYRILLIC SMALL LETTER U
	'х': 'x', // U+0445 CYRILLIC SMALL LETTER HA
	'і': 'i', // U+0456 CYRILLIC SMALL LETTER BYELORUSSIAN-UKRAINIAN I
	'ѕ': 's', // U+0455 CYRILLIC SMALL LETTER DZE
	'ј': 'j', // U+0458 CYRILLIC SMALL LETTER JE
	'ԁ': 'd', // U+0501 CYRILLIC SMALL LETTER KOMI DE
	'ѡ': 'w', // U+0461 CYRILLIC SMALL LETTER OMEGA
	// Greek lookalikes.
	'α': 'a', // U+03B1 GREEK SMALL LETTER ALPHA
	'ο': 'o', // U+03BF GREEK SMALL LETTER OMICRON
	'ρ': 'p', // U+03C1 GREEK SMALL LETTER RHO
	'ν': 'v', // U+03BD GREEK SMALL LETTER NU
	'κ': 'k', // U+03BA GREEK SMALL LETTER KAPPA
	// Leetspeak digit-for-letter substitutions, the other common
	// impersonation trick alongside script mixing.
	'0': 'o',
	'1': 'l',
	'3': 'e',
	'4': 'a',
	'5': 's',
	'7': 't',
	'$': 's',
	'@': 'a',
}
