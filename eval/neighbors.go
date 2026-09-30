package eval

import (
	"context"
	"sort"
	"time"

	"github.com/tokencanopy/abusekit/internal/event"
	"github.com/tokencanopy/abusekit/internal/feature"
)

// neighborCapPerKey / neighborCapTotal mirror internal/store's own fan-in
// caps (design §4.2: "a fan-in cap of 50 per key and a total cap of
// 200"). A private/future slice could make these configurable per
// dataset; S4 hard-codes the shipped production defaults since nothing
// here needs to differ from them.
const (
	neighborCapPerKey = 50
	neighborCapTotal  = 200
)

// defaultNeighborKinds mirrors internal/feature.Config{}'s zero-value
// default candidate kinds (email_hash, card_fingerprint_hash,
// device_hash) — ip24_hash/ua_hash/asn stay excluded by default (design
// §4.2, S1/S2 fix rounds). This in-memory Neighbors has no per-tenant
// override knob; it always uses the shipped default policy.
var defaultNeighborKinds = []string{"email_hash", "card_fingerprint_hash", "device_hash"}

// fingerprintKind is the one link kind FingerprintSeenOnOtherSubjects
// always checks, regardless of defaultNeighborKinds (design §4.2's own
// carve-out, mirrored from internal/feature/store_neighbors.go's
// cardFingerprintKind).
const fingerprintKind = "card_fingerprint_hash"

// linkRef is one (subject, instant) touch of a link key — kept as a
// per-EVENT record (not deduped/aggregated per subject) so evidenceAsOf
// can ask "was subject visibly linked under this key before asOf",
// not just "is it linked at all, ever" (see evidenceAsOf's doc comment
// on why this chronology matters).
type linkRef struct {
	subject string
	at      time.Time
}

// datasetNeighbors implements a time-aware feature.Neighbors entirely
// from a dataset's own events and labels (task brief: "an in-memory
// Neighbors built from the dataset's links") — no store, no network.
//
// evidenceAsOf(subject, asOf) — NOT feature.Neighbors' own
// Evidence(ctx, tenant, subject) signature, which has no room for a
// per-call cutoff — is what LoadReplayDataset actually calls, through
// the asOfNeighbors adapter below, once per (subject, slice) pair: a
// same-tenant neighbour's link visibility and permanent-deletion status
// are evaluated AS OF the scored subject's own decision_at, not as of
// the end of the whole dataset. This matters concretely for churn: an
// EARLIER incarnation in a chain must not see a LATER incarnation's
// link rows or deletion at all (they haven't happened yet, from the
// earlier incarnation's own vantage point) — evaluating every
// incarnation against the dataset's final state instead would make even
// the FIRST incarnation see every later one already deleted, trivially
// inflating linked_deleted_n for a case design §1.2(c) specifically
// expects to be hard for exactly the first two.
//
// labelledAt holds every "label" event (see LoadReplayDataset's own doc
// comment on this convention) seen for one subject, each paired with the
// instant it was recorded — evidenceAsOf only counts one whose `at`
// precedes the asked-for asOf, and only when its value is "positive"
// relative to whichever rule is being scored (see labelledAsOf).
//
// Fix round B1: this is DELIBERATELY never derived from labels.jsonl's
// ground truth (the ANSWER being evaluated) — only from `label`-typed
// rows in the EVENTS file, the same way a real deployment would only
// ever see a neighbour's history through events/labels it actually
// recorded, never through the corpus's own held-out answer key. Reading
// the evaluated ground truth here would let one subject's neighbour
// evidence "know" a fact (another subject IS abusive) that no real
// scoring pass could have known at that same wall-clock instant, quietly
// inflating recall on every densely-linked family (worst on churn, where
// every incarnation in a chain would trivially see every other
// incarnation's held-out answer).
type labelledAt struct {
	producer string
	id       string
	at       time.Time
	value    string
}

type datasetNeighbors struct {
	// byKindHash holds every (kind, hash) -> every linkRef that ever
	// touched it, UNDEDUPED: a subject that used the same link key twice
	// (at two different instants) needs both instants on record so
	// evidenceAsOf can find whichever occurrence (if any) precedes a
	// given asOf.
	byKindHash map[string]map[string][]linkRef
	// permanentlyDeletedAt is the EARLIEST permanent subject.deleted
	// event's At per subject (a subject deleted more than once — not
	// possible in this vocabulary, but defensive either way — would keep
	// the first).
	permanentlyDeletedAt map[string]time.Time
	// labelEvents is keyed by subject; see labelledAt's own doc comment.
	labelEvents map[string][]labelledAt
}

// newDatasetNeighbors indexes eventsBySubject's link keys, permanent
// subject.deleted events, and "label"-typed events (fix round B1 — see
// labelledAt's doc comment). eventsBySubject must NOT include "label"
// events themselves (LoadReplayDataset routes them separately — a
// label-posting action was never a real account-activity event, and
// leaving it in the slice feature.Extract reads would let it corrupt
// e.g. subject_age_h's first-seen anchor).
func newDatasetNeighbors(eventsBySubject map[string][]event.Event, labelEventsBySubject map[string][]labelledAt) *datasetNeighbors {
	n := &datasetNeighbors{
		byKindHash:           map[string]map[string][]linkRef{},
		permanentlyDeletedAt: map[string]time.Time{},
		labelEvents:          labelEventsBySubject,
	}
	for subject, events := range eventsBySubject {
		for _, e := range events {
			for _, kh := range e.Links.Kinds() {
				if n.byKindHash[kh.Kind] == nil {
					n.byKindHash[kh.Kind] = map[string][]linkRef{}
				}
				n.byKindHash[kh.Kind][kh.Hash] = append(n.byKindHash[kh.Kind][kh.Hash], linkRef{subject: subject, at: e.At})
			}
			if e.Type == "subject.deleted" {
				if mode, ok := dataStringField(e.Data, "mode"); ok && mode == "permanent" {
					if cur, ok := n.permanentlyDeletedAt[subject]; !ok || e.At.Before(cur) {
						n.permanentlyDeletedAt[subject] = e.At
					}
				}
			}
		}
	}
	return n
}

// labelledAsOf reports whether subject has a "label" event, recorded
// strictly before asOf, whose value is "positive" — i.e. not
// benignLabel (fix round B1: "take the positive/negative class from the
// rule's labels, not the hardcoded 'abusive'" — a rule's own vocabulary
// need not even contain the literal string "abusive" at all, design
// §4.5's `lure_similarity` example uses "phishing"/"brand_impersonation"/
// "scam" instead).
func (n *datasetNeighbors) labelledAsOf(subject string, asOf time.Time, benignLabel string) bool {
	for _, le := range n.labelEvents[subject] {
		if le.at.Before(asOf) && le.value != benignLabel {
			return true
		}
	}
	return false
}

// neighborsByKinds returns subject's same-tenant neighbours sharing any
// of kinds, restricted to link touches strictly before asOf on BOTH
// sides (subject's own qualifying touch, and the neighbour's) — capped
// per-key/total, deduplicated across keys, excluding subject itself.
// Among a key's candidates past the per-key cap, the ones kept are
// whichever touched it MOST RECENTLY before asOf (mirroring
// internal/store.Neighbors' own "keeps the most recently active
// neighbours" tie-break, just evaluated as of asOf instead of as of
// query time).
func (n *datasetNeighbors) neighborsByKinds(subject string, kinds []string, asOf time.Time) (subjects []string, truncated bool) {
	seen := map[string]bool{subject: true}
	for _, kind := range kinds {
		for _, refs := range n.byKindHash[kind] {
			selfVisible := false
			for _, r := range refs {
				if r.subject == subject && r.at.Before(asOf) {
					selfVisible = true
					break
				}
			}
			if !selfVisible {
				continue
			}

			latest := map[string]time.Time{}
			for _, r := range refs {
				if r.subject == subject || !r.at.Before(asOf) {
					continue
				}
				if cur, ok := latest[r.subject]; !ok || r.at.After(cur) {
					latest[r.subject] = r.at
				}
			}
			type candidate struct {
				subject string
				at      time.Time
			}
			candidates := make([]candidate, 0, len(latest))
			for s, t := range latest {
				candidates = append(candidates, candidate{s, t})
			}
			sort.Slice(candidates, func(i, j int) bool {
				if candidates[i].at.Equal(candidates[j].at) {
					return candidates[i].subject < candidates[j].subject // deterministic tie-break
				}
				return candidates[i].at.After(candidates[j].at)
			})

			count := 0
			for _, c := range candidates {
				count++
				if count > neighborCapPerKey {
					truncated = true
					continue
				}
				if seen[c.subject] {
					continue
				}
				if len(subjects) >= neighborCapTotal {
					truncated = true
					continue
				}
				seen[c.subject] = true
				subjects = append(subjects, c.subject)
			}
		}
	}
	sort.Strings(subjects)
	return subjects, truncated
}

// evidenceAsOf resolves NeighborEvidence for subject as of asOf (see this
// type's own doc comment for why "as of" matters). benignLabel is
// whichever rule is currently being scored's BenignLabel (fix round B1
// — see labelledAsOf).
func (n *datasetNeighbors) evidenceAsOf(subject string, asOf time.Time, benignLabel string) (feature.NeighborEvidence, error) {
	general, generalTruncated := n.neighborsByKinds(subject, defaultNeighborKinds, asOf)
	fingerprintNeighbors, fpTruncated := n.neighborsByKinds(subject, []string{fingerprintKind}, asOf)

	var deleted, labelled int
	for _, s := range general {
		if t, ok := n.permanentlyDeletedAt[s]; ok && t.Before(asOf) {
			deleted++
		}
		if n.labelledAsOf(s, asOf, benignLabel) {
			labelled++
		}
	}
	return feature.NeighborEvidence{
		DeletedCount:         deleted,
		LabelledAbusiveCount: labelled,
		FingerprintShared:    len(fingerprintNeighbors) > 0,
		Truncated:            generalTruncated || fpTruncated,
	}, nil
}

// asOfNeighbors adapts a fixed (datasetNeighbors, asOf, benignLabel)
// triple to feature.Neighbors' interface (Evidence has no cutoff
// parameter of its own) — replay.go constructs one of these per
// (subject, slice) pair, all sharing the same underlying
// datasetNeighbors index. benignLabel is threaded through from whichever
// rule the CALLER (LoadReplayDataset, via its own benignLabel parameter)
// is building features for — see fix round B1.
type asOfNeighbors struct {
	n           *datasetNeighbors
	asOf        time.Time
	benignLabel string
}

func (a asOfNeighbors) Evidence(_ context.Context, _, subject string) (feature.NeighborEvidence, error) {
	return a.n.evidenceAsOf(subject, a.asOf, a.benignLabel)
}

// dataStringField reads a possibly-absent, possibly-wrong-typed string
// key out of an already-redacted event's Data map — mirrors
// internal/feature's own private dataString helper (not exported, so
// duplicated here rather than imported).
func dataStringField(data map[string]any, key string) (string, bool) {
	v, ok := data[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}
