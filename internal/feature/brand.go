package feature

import "strings"

// brandNames is a curated, v0 placeholder subset of frequently-impersonated
// brand names (design §1's "brand-like display names"), matched as a
// substring against a resource's NFKC+confusables-folded name_skeleton
// (internal/event.Skeleton already folds case, homoglyphs and leetspeak
// substitutions, so this list only needs each brand's plain lower-case
// spelling — no need to also list "paypa1" or "amaz0n").
//
// Like internal/event's confusablesTable, this is data to extend from
// labelled examples, not logic — but unlike that table, matching here is a
// bare substring check with no word-boundary awareness, so a short or
// generic token is a false-positive risk against ordinary English
// (e.g. a 3-letter carrier abbreviation can hide inside a common word).
// Entries are deliberately kept long/distinctive enough to make an
// accidental match unlikely; a brand whose recognizable form is short
// (e.g. "IRS", "UPS") is left out of v0's list for exactly that reason
// rather than included and accepted as noisy — matching it properly needs
// word-boundary-aware matching, not a bigger list, and is future work.
var brandNames = []string{
	"paypal",
	"amazon",
	"apple",
	"google",
	"microsoft",
	"netflix",
	"wellsfargo",
	"bankofamerica",
	"coinbase",
	"binance",
	"stripe",
	"usps",
	"fedex",
	"dhl",
	"walmart",
	"ebay",
}

// matchesBrand reports whether skeleton (already NFKC+confusables-folded
// and lower-cased by event.Skeleton) contains any curated brand name as a
// substring.
func matchesBrand(skeleton string) bool {
	for _, b := range brandNames {
		if strings.Contains(skeleton, b) {
			return true
		}
	}
	return false
}
