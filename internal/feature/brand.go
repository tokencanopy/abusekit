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
//
// CaseSensitive (S2b's N1 fix round) is for the narrow case of a short
// brand token that also happens to be an ordinary English word or
// abbreviation (a shipping brand's all-caps initialism is the common
// example): when true, a match additionally requires the ORIGINAL
// candidate text to spell this brand's Name with the identical case as a
// standalone token, not merely fold-equal to it — see
// containsExactCaseToken. Meaningful only for a single-word Name; a
// multi-word brand should rely on the ordinary word-boundary match
// instead.
type BrandEntry struct {
	Name          string
	Aliases       []string
	CaseSensitive bool
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
// split into words on whitespace and any Unicode punctuation/symbol rune
// (S2b's B3 fix round widened this from a hand-picked separator list —
// see tokenize) — so "Wells Fargo", "wells-fargo" and "WELLS FARGO" all
// tokenize to the same ["wells","fargo"], and "PAYPAL SUPPORT" tokenizes
// to ["paypal", "support"]. A brand's word sequence must appear as a
// CONTIGUOUS run of the candidate's words; a single-word brand must match
// a whole word, not a substring of one — "Pineapple" is one token that is
// never equal to "apple", so it never matches even if "apple" were still
// in the list.
//
// This does not catch every obfuscation (e.g. "paypalsupport" glued into
// one word with no separator tokenizes as a single word that doesn't
// equal "paypal"): trading a little recall for the word-boundary safety
// two independent reviews required is the deliberate v0 choice.
//
// brandWords is one pre-tokenized name/alias word sequence, tagged with
// the CANONICAL brand name (BrandEntry.Name) it belongs to — S2b:
// subject_brand_match needs to count DISTINCT brands, so a match has to
// be traceable back to which brand identity fired, not just "something
// matched" (the original bool-only Matches contract).
type brandWords struct {
	words         []string
	name          string
	caseSensitive bool
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
			all = append(all, brandWords{words, e.Name, e.CaseSensitive})
		}
		for _, a := range e.Aliases {
			// CaseSensitive is deliberately NOT propagated to an alias:
			// every shipped case-sensitive entry so far is a single bare
			// word with no alias of its own: extending the case check to
			// an alias nobody has defined yet is speculative complexity
			// with nothing to verify it against.
			if words := tokenize(a); len(words) > 0 {
				all = append(all, brandWords{words, e.Name, false})
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
// S2b's S1 fix round narrows where this gate applies: it still governs a
// resource/agent NAME match (MatchedBrandNames), but a subject_line match
// (MatchedBrandNamesForSubject) is no longer gated by words inside the
// SUBJECT LINE itself — an ordinary bulk-phishing subject routinely
// contains "tracking" or "api" on purpose ("Your package tracking update
// failed"), and gating subject-line matching on the subject's own words
// silently defeated the very rule meant to catch that shape. Round 2's R2
// fix round: whether a MATCHED brand should be exempted is decided
// per-brand by windows.go's exemptSubjectBrands (the SENDING ACCOUNT's own
// live, agent-kind resource names), not by this function — an earlier
// round exempted subject-line matching outright, for every brand, the
// instant ANY resource name anywhere carried an integration token; that
// swept away a genuinely different brand's lure in the same subject line.
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

// communityPhraseWords are contiguous word-SEQUENCES (never a bare single
// word — round 2's R3 fix round) whose presence anywhere in a candidate
// SUBJECT LINE mean it is very likely describing an ordinary community/
// social gathering around the brand it mentions ("<brand> group meetup",
// "<brand> fan club", "<brand> community event") rather than
// impersonating it. R3 replaced an earlier, bare-single-word version of
// this gate ("chat", "group", "fans", "club", "community", "meetup"
// individually) — proven too broad: an everyday subject like "<brand>:
// chat with support" or "Join the <brand> group today" has nothing to do
// with a community gathering, but tripped the old gate anyway on a single
// word.
//
// Applied ONLY to subject-line matching (MatchedBrandNamesForSubject),
// NEVER to a resource/agent NAME (MatchedBrandNames) — R3: an earlier
// round applied it to both, which suppressed name_brand_match for an
// ordinary agent name like "<brand> Support Chat".
var communityPhraseWords = [][]string{
	{"group", "meetup"},
	{"fan", "club"},
	{"community", "event"},
}

var communityPhrases = buildCommunityPhrases()

func buildCommunityPhrases() [][]string {
	out := make([][]string, len(communityPhraseWords))
	for i, phrase := range communityPhraseWords {
		words := make([]string, len(phrase))
		for j, w := range phrase {
			words[j] = canonicalise(w)
		}
		out[i] = words
	}
	return out
}

func hasCommunityPhrase(words []string) bool {
	for _, phrase := range communityPhrases {
		if containsSequence(words, phrase) {
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
	return len(b.MatchedBrandNames(text)) > 0
}

// MatchedBrandNames returns the set of DISTINCT curated brand names
// (BrandEntry.Name — an entry matched via an alias still reports its
// canonical name, never the alias text) whose word sequence appears in
// text, applying the integration-token gate — this is the matcher
// name_brand_match uses against a resource/agent's raw name. The
// community-phrase gate (R3 fix round) does NOT apply here — see
// MatchedBrandNamesForSubject for the subject-line-specific variant that
// does.
//
// Returns nil (never a non-nil empty map) when nothing matched, matching
// Go's normal "ranging over a nil map is a no-op, len(nil map) is 0"
// idiom — callers never need a special nil check before iterating.
func (b BrandSet) MatchedBrandNames(text string) map[string]struct{} {
	return b.matched(text, true, false)
}

// MatchedBrandNamesForSubject is subject_brand_match's matcher (S2b's S1
// fix round): brands are matched WITHOUT gating on words inside the
// subject line itself the way MatchedBrandNames' integration-token gate
// does — a bulk-phishing subject routinely contains "tracking" or "api"
// on purpose, and the OLD behaviour of gating on the subject's own words
// silently defeated the rule for exactly the subjects it exists to catch.
// The community-PHRASE gate (R3 fix round) DOES apply here, and only
// here — it addresses a different false-positive shape (a social brand
// mentioned in the course of describing an ordinary community gathering)
// that is unique to subject lines.
//
// Deciding WHICH matched brand(s) to then exempt (round 2's R2 fix round:
// only the brand adjacent to an integration token in the SENDING
// ACCOUNT's own live, agent-kind resource name — see windows.go's
// exemptSubjectBrands) is the caller's job, not this function's: it also
// reuses this exact matcher to identify which brand an integration-named
// resource is ABOUT in the first place, so it can't itself decide the
// exemption without becoming circular.
func (b BrandSet) MatchedBrandNamesForSubject(text string) map[string]struct{} {
	return b.matched(text, false, true)
}

// matched is Matches/MatchedBrandNames/MatchedBrandNamesForSubject's
// shared implementation: applyIntegrationGate selects whether
// integrationTokens suppresses a match (true for a resource/agent name,
// false for a subject line already cleared by
// MatchedBrandNamesForSubject's own account-level check);
// applyCommunityGate selects whether communityPhrases does (false for a
// name, true for a subject line — R3 fix round).
func (b BrandSet) matched(text string, applyIntegrationGate, applyCommunityGate bool) map[string]struct{} {
	if len(b.entries) == 0 {
		return nil
	}
	out := b.matchedNames(tokenize(text), text, applyIntegrationGate, applyCommunityGate)
	for name := range b.matchedNames(tokenizeCamel(text), text, applyIntegrationGate, applyCommunityGate) {
		if out == nil {
			out = make(map[string]struct{})
		}
		out[name] = struct{}{}
	}
	return out
}

func (b BrandSet) matchedNames(words []string, original string, applyIntegrationGate, applyCommunityGate bool) map[string]struct{} {
	if len(words) == 0 {
		return nil
	}
	if applyCommunityGate && hasCommunityPhrase(words) {
		return nil
	}
	if applyIntegrationGate && hasIntegrationToken(words) {
		return nil
	}
	var out map[string]struct{}
	for _, brand := range b.entries {
		if !containsSequence(words, brand.words) {
			continue
		}
		if brand.caseSensitive && !containsExactCaseToken(original, brand.name) {
			continue
		}
		if out == nil {
			out = make(map[string]struct{})
		}
		out[brand.name] = struct{}{}
	}
	return out
}

// containsExactCaseToken reports whether text contains word as a
// case-SENSITIVE standalone token, split the same way tokenize splits its
// folded copy (any whitespace/punctuation/symbol rune) but on text's
// ORIGINAL, un-folded casing — S2b's N1 fix round: a short brand token
// that doubles as an ordinary English word or abbreviation (a shipping
// brand's all-caps initialism is the common example) should not fire on
// the word used in everyday lower-case prose; requiring the identical
// case as a whole token lets the initialism still match while the
// ordinary word does not.
func containsExactCaseToken(text, word string) bool {
	for _, tok := range strings.FieldsFunc(text, isWordSeparator) {
		if tok == word {
			return true
		}
	}
	return false
}

// tokenize folds s through event.Skeleton (NFKC, confusables, lower-case,
// whitespace-collapse), strips zero-width/invisible-formatting characters
// Skeleton doesn't touch, canonicalises the I/l confusable the rest of the
// way (see canonicalise), and splits on whitespace plus any Unicode
// punctuation or symbol rune (isWordSeparator — S2b's B3 fix round,
// proven: a bare separator list of hyphen/underscore/period missed a
// brand immediately followed by a colon, comma, exclamation mark, closing
// parenthesis, quotation mark or slash, e.g. "PayPal:" or "(PayPal)", and
// never recognised a possessive apostrophe-s, e.g. "PayPal's" — every one
// of those punctuation runes is itself Unicode punctuation or a symbol,
// so a single category-based predicate closes all of them at once rather
// than hand-enumerating an ever-growing separator list one report at a
// time), dropping empty tokens. "pay-pal", "pay_pal", "pay.pal", "pay:pal"
// and "pay pal" all tokenize identically to ["pay","pal"], and "PayPal's"
// tokenizes to ["paypal","s"] — the possessive suffix becomes its own
// harmless token, never glued onto the brand word.
//
// This is the SAME function NewBrandSet uses to tokenize every brand
// definition and Matches uses to tokenize every candidate — canonicalise
// is applied to both sides identically for exactly that reason (D1 round
// 3): a fold that only ran on one side (or was hand-applied ad hoc to
// integrationTokens' literal strings instead of through this shared
// path) silently reintroduced the exact divergence it was meant to close.
func tokenize(s string) []string {
	folded := canonicalise(stripZeroWidth(event.Skeleton(s)))
	return strings.FieldsFunc(folded, isWordSeparator)
}

// isWordSeparator reports whether r splits tokenize's candidate/brand
// text into words: any whitespace rune, or any rune Unicode classifies as
// punctuation or a symbol (S2b's B3 fix round — see tokenize's doc
// comment for the punctuation shapes this specifically closes).
func isWordSeparator(r rune) bool {
	return unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r)
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
// boundary is inserted at every transition from a lower-case letter or a
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
// BOM), wordJoiner (U+2060), softHyphen (U+00AD, S2b's N3 fix round),
// invisibleSeparator (U+2063, S2b's N3 fix round).
const (
	zeroWidthSpace        = 0x200B
	zeroWidthNonJoiner    = 0x200C
	zeroWidthJoiner       = 0x200D
	zeroWidthNoBreakSpace = 0xFEFF
	wordJoiner            = 0x2060
	softHyphen            = 0x00AD
	invisibleSeparator    = 0x2063
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
		case zeroWidthSpace, zeroWidthNonJoiner, zeroWidthJoiner, zeroWidthNoBreakSpace, wordJoiner, softHyphen, invisibleSeparator:
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
	Name          string   `yaml:"name"`
	Aliases       []string `yaml:"aliases"`
	CaseSensitive bool     `yaml:"case_sensitive"`
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
		entries = append(entries, BrandEntry{Name: rb.Name, Aliases: rb.Aliases, CaseSensitive: rb.CaseSensitive})
	}
	return NewBrandSet(entries), nil
}

// MergeBrandSets combines the entries of several BrandSets into one — the
// scope's "optional private brands_extra file" requirement: an operator
// can keep a private brand list outside this public repo (config
// `brands_extra` / `--brands-extra`, cmd/abusekit) and have it matched
// alongside the shipped public config/brands.yaml, without LoadBrandsFile
// itself needing to know how many files it's loading. A zero-value/empty
// argument contributes nothing (MergeBrandSets(a, BrandSet{}) == a in
// behavior), so a caller can always merge in an optional set
// unconditionally rather than branching on whether it was actually loaded.
func MergeBrandSets(sets ...BrandSet) BrandSet {
	var all []brandWords
	for _, s := range sets {
		all = append(all, s.entries...)
	}
	return BrandSet{entries: all}
}
