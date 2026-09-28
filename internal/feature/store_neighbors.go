package feature

import (
	"context"
	"fmt"

	"github.com/tokencanopy/abusekit/internal/store"
)

// cardFingerprintKind is the one link kind FingerprintSeenOnOtherSubjects
// checks, regardless of Config's inclusion knobs — see its doc comment on
// NeighborEvidence.FingerprintShared.
const cardFingerprintKind = "card_fingerprint_hash"

// allLinkKinds enumerates every link kind internal/event.Links can produce
// (event.Links.Kinds' possible kind values), the master list Config's
// candidateKinds derives from by exclusion (S1 fix round) rather than
// hand-maintaining a second "the good ones" list that could drift from the
// first if event.Links ever grows a new kind.
var allLinkKinds = []string{
	"email_hash",
	"card_fingerprint_hash",
	"ip24_hash",
	"asn",
	"ua_hash",
	"device_hash",
}

// Config controls feature-extraction policy that isn't part of a rule's
// YAML (internal/config) but still affects what a feature computes: which
// link kinds count as same-tenant linking evidence for LinkedDeletedN and
// LinkedLabelledAbusiveN (FingerprintSeenOnOtherSubjects always checks
// card_fingerprint_hash specifically and is unaffected by any of these).
//
// S1 fix round: the default candidate set is now {email_hash,
// card_fingerprint_hash, device_hash} — email_hash and card_fingerprint_hash
// are the two the design's own churn narrative names ("sharing a signup-
// email hash or a card fingerprint"), and device_hash is comparably
// specific. ip24_hash, ua_hash and asn are opt-in: proven, a single shared
// ua_hash ALONE (e.g. two subjects that both used the same email client or
// SDK — extremely common, not evidence of anything) produced
// linked_deleted_n=1 for a totally unrelated subject. asn was already
// excluded by design for the coarser reason that a large ISP/cloud
// provider's ASN can be shared by thousands of subjects.
type Config struct {
	// IncludeIP24Hash opts the "ip24_hash" link kind into neighbour
	// discovery. Off by default: a /24 can be a large NAT/carrier-grade
	// address block shared by many unrelated subjects.
	IncludeIP24Hash bool
	// IncludeUAHash opts the "ua_hash" link kind into neighbour discovery.
	// Off by default: proven, a shared user-agent hash alone (the same
	// email client or SDK version) is not evidence of a shared operator.
	IncludeUAHash bool
	// IncludeASN opts the "asn" link kind into neighbour discovery. Off by
	// default (design's own open question — see below).
	//
	// TODO(design open question): whether ASN should ever count as linking
	// evidence at all is still open. A large ISP's or cloud provider's ASN
	// can be shared by thousands of unrelated subjects, and reviewers of
	// this slice expect an ASN-inclusive neighbour set to saturate the
	// fan-in cap (design §4.2) with noise long before it says anything
	// about abuse. Defaulting to false is the conservative choice until
	// that's settled against real labelled data; a future slice may want
	// this per-tenant rather than a single process-wide flag. Decision
	// owner: Josh.
	IncludeASN bool
}

// candidateKinds derives the link kinds neighbour discovery should
// consider from allLinkKinds minus whichever of ip24_hash/ua_hash/asn c
// doesn't opt into (S1 fix round) — email_hash, card_fingerprint_hash and
// device_hash are always included.
func (c Config) candidateKinds() []string {
	exclude := map[string]bool{}
	if !c.IncludeIP24Hash {
		exclude["ip24_hash"] = true
	}
	if !c.IncludeUAHash {
		exclude["ua_hash"] = true
	}
	if !c.IncludeASN {
		exclude["asn"] = true
	}
	out := make([]string, 0, len(allLinkKinds))
	for _, k := range allLinkKinds {
		if !exclude[k] {
			out = append(out, k)
		}
	}
	return out
}

// StoreNeighbors implements Neighbors over a real *store.Store, applying
// cfg's link-kind-inclusion policy.
type StoreNeighbors struct {
	store *store.Store
	cfg   Config
}

// NewStoreNeighbors returns a Neighbors backed by s, per cfg.
func NewStoreNeighbors(s *store.Store, cfg Config) *StoreNeighbors {
	return &StoreNeighbors{store: s, cfg: cfg}
}

// Evidence resolves NeighborEvidence for subject. The design's fan-in cap
// (§4.2) "keeps the most recently active neighbours" when a link key fans
// out past it — internal/store.Neighbors/NeighborsByKinds already order by
// last_seen DESC per key for exactly that reason (S1 fix round: documented
// here since it's this method's caller-visible behavior, not just an
// implementation detail of the store query).
func (n *StoreNeighbors) Evidence(ctx context.Context, tenant, subject string) (NeighborEvidence, error) {
	neighbors, truncated, err := n.store.NeighborsByKinds(ctx, tenant, subject, n.cfg.candidateKinds(), 0, 0)
	if err != nil {
		return NeighborEvidence{}, fmt.Errorf("feature: resolve neighbors for %s: %w", subject, err)
	}

	deletedCount, labelledAbusiveCount, err := n.store.NeighborOutcomes(ctx, tenant, neighbors)
	if err != nil {
		return NeighborEvidence{}, fmt.Errorf("feature: resolve neighbor outcomes for %s: %w", subject, err)
	}

	fingerprintNeighbors, fpTruncated, err := n.store.NeighborsByKinds(ctx, tenant, subject, []string{cardFingerprintKind}, 0, 0)
	if err != nil {
		return NeighborEvidence{}, fmt.Errorf("feature: resolve fingerprint neighbors for %s: %w", subject, err)
	}

	return NeighborEvidence{
		DeletedCount:         deletedCount,
		LabelledAbusiveCount: labelledAbusiveCount,
		FingerprintShared:    len(fingerprintNeighbors) > 0,
		Truncated:            truncated || fpTruncated,
	}, nil
}
