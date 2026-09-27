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
// fold the I/l/ı/1 confusable group (case-sensitively, before lowercasing
// — see foldIAndDigitOne) -> lower-case -> map each remaining rune through
// a small curated confusables table (Cyrillic/Greek Latin-lookalikes,
// digit-for-letter leetspeak, only for a digit run judged "leet-eligible"
// — see markLeetEligibleDigits) -> collapse runs of whitespace.
//
// This is a v0 placeholder, not a UTS #39 confusables implementation: the
// table below covers the lookalikes seen in real brand-impersonation
// attempts, not the full Unicode confusables list. Extend confusablesTable
// as new lookalikes show up in labelled examples; it is data, not logic,
// so extending it never touches callers.
func Skeleton(s string) string {
	folded := norm.NFKC.String(s)
	folded = stripDiacritics(folded)

	runes := []rune(folded)
	eligible := markLeetEligibleDigits(runes)
	foldIAndDigitOne(runes, eligible)

	lowered := strings.ToLower(string(runes))
	// Lower-casing an ASCII/digit/letter rune never changes its byte-for-
	// rune position relative to this slice, so eligible (computed above,
	// before lowering) still lines up with runes below.
	runes = []rune(lowered)

	var b strings.Builder
	b.Grow(len(lowered))
	lastWasSpace := false
	for i, r := range runes {
		if mapped, ok := confusablesTable[r]; ok {
			if isLeetDigit(r) && !eligible[i] {
				// Not leet-eligible (no substantial adjacent letter run
				// on either side) — leave it as a literal digit rather
				// than leet-decoding a standalone number or identifier
				// suffix ("Order 12345", "ORDER #1", "v1") into letters.
			} else {
				r = mapped
			}
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

// foldIAndDigitOne folds the "I"/"l"/"ı"/"1" confusable group onto 'l', in
// place, before Skeleton lowercases the rest of the string. This must
// happen pre-lowercase: lowercasing a capital "I" (impersonating a
// lowercase "l", e.g. "PayPaI") turns it into a lowercase "i" — a
// perfectly ordinary, unrelated letter — which would erase the very
// look-alike this function exists to catch. A "1" is folded only when
// eligible says its digit run is leet-eligible (see
// markLeetEligibleDigits) — the same adjacency rule the second
// (post-lowercase) confusables pass uses for 0/3/4/5/7, so "1" isn't a
// special case: "supp1y" folds, but a bare "1" or "v1" does not.
func foldIAndDigitOne(runes []rune, eligible []bool) {
	for i, r := range runes {
		switch r {
		case 'I', 'ı':
			runes[i] = 'l'
		case '1':
			if eligible[i] {
				runes[i] = 'l'
			}
		}
	}
}

// isLeetDigit reports whether r is one of the digits confusablesTable maps
// to a letter (excluding '1', which foldIAndDigitOne already handles
// case-sensitively before lowercasing).
func isLeetDigit(r rune) bool {
	switch r {
	case '0', '3', '4', '5', '7':
		return true
	}
	return false
}

// markLeetEligibleDigits returns, for each index in runes, whether
// runes[i] is an ASCII digit that is part of a leet-eligible digit run
// (R10, round 2). A maximal run of consecutive digits is eligible when
// the sum of the immediately-adjacent letter runs on either side (the
// contiguous unicode.IsLetter characters right before the digit run, plus
// the ones right after) is >= 2 — chosen, from the reviewer's four proven
// cases, to separate genuine letter-substitution from a standalone
// number or a short identifier prefix:
//
//   - "g00gle": before="g" (1) + after="gle" (3) = 4 >= 2 -> eligible
//     (the digit run sits inside what is unambiguously a word).
//   - "paypa1"/"supp1y"/"supp0rt": a substantial letter run on at least
//     one side (>= 2 by itself) -> eligible.
//   - "am4z0n": "4" has before="am" (2) + after="z" (1) = 3 >= 2;
//     "0" has before="z" (1) + after="n" (1) = 2 >= 2 -> both eligible,
//     even though each side alone is short — together they still read as
//     one impersonated word.
//   - "Order 12345": before/after are a space and end-of-string (0 + 0)
//     -> not eligible: an isolated number.
//   - "ORDER #1": before is "#" (0) and after is end-of-string (0) ->
//     not eligible: punctuation-isolated, not letter-adjacent at all.
//   - "v1": before="v" (1) + after="" (0) = 1 < 2 -> not eligible: a
//     single-letter prefix (a version/identifier idiom) isn't enough
//     letter context to call this letter substitution rather than a
//     genuine "v1", "v2", ... label.
func markLeetEligibleDigits(runes []rune) []bool {
	out := make([]bool, len(runes))
	isDigit := func(r rune) bool { return r >= '0' && r <= '9' }
	n := len(runes)
	for i := 0; i < n; {
		if !isDigit(runes[i]) {
			i++
			continue
		}
		j := i
		for j < n && isDigit(runes[j]) {
			j++
		}
		beforeLen := 0
		for k := i - 1; k >= 0 && unicode.IsLetter(runes[k]); k-- {
			beforeLen++
		}
		afterLen := 0
		for k := j; k < n && unicode.IsLetter(runes[k]); k++ {
			afterLen++
		}
		eligible := beforeLen+afterLen >= 2
		for k := i; k < j; k++ {
			out[k] = eligible
		}
		i = j
	}
	return out
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
	// impersonation trick alongside script mixing. '1' is handled
	// separately by foldIAndDigitOne (case-sensitively, before
	// lowercasing, alongside 'I' and 'ı') and is deliberately not a key
	// here.
	'0': 'o',
	'3': 'e',
	'4': 'a',
	'5': 's',
	'7': 't',
	'$': 's',
	'@': 'a',
}
