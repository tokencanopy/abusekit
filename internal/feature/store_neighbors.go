package feature

import (
	"context"
	"fmt"

	"github.com/tokencanopy/abusekit/internal/store"
)

// cardFingerprintKind is the one link kind FingerprintSeenOnOtherSubjects
// checks, regardless of Config.IncludeASN — see its doc comment on
// NeighborEvidence.FingerprintShared.
const cardFingerprintKind = "card_fingerprint_hash"

// linkKindsExceptASN is internal/event.Links.Kinds' five non-ASN link
// kinds, listed explicitly (asn is deliberately left out — see Config).
var linkKindsExceptASN = []string{
	"email_hash",
	"card_fingerprint_hash",
	"ip24_hash",
	"ua_hash",
	"device_hash",
}

// Config controls feature-extraction policy that isn't part of a rule's
// YAML (internal/config) but still affects what a feature computes.
type Config struct {
	// IncludeASN, when false (the default), excludes the "asn" link kind
	// from same-tenant neighbour discovery for LinkedDeletedN and
	// LinkedLabelledAbusiveN (FingerprintSeenOnOtherSubjects always checks
	// card_fingerprint_hash specifically and is unaffected by this).
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

// StoreNeighbors implements Neighbors over a real *store.Store, applying
// cfg's ASN-inclusion policy.
type StoreNeighbors struct {
	store *store.Store
	cfg   Config
}

// NewStoreNeighbors returns a Neighbors backed by s, per cfg.
func NewStoreNeighbors(s *store.Store, cfg Config) *StoreNeighbors {
	return &StoreNeighbors{store: s, cfg: cfg}
}

func (n *StoreNeighbors) Evidence(ctx context.Context, tenant, subject string) (NeighborEvidence, error) {
	var (
		neighbors []string
		err       error
	)
	if n.cfg.IncludeASN {
		neighbors, _, err = n.store.Neighbors(ctx, tenant, subject, 0, 0)
	} else {
		neighbors, _, err = n.store.NeighborsByKinds(ctx, tenant, subject, linkKindsExceptASN, 0, 0)
	}
	if err != nil {
		return NeighborEvidence{}, fmt.Errorf("feature: resolve neighbors for %s: %w", subject, err)
	}

	deletedCount, labelledAbusiveCount, err := n.store.NeighborOutcomes(ctx, tenant, neighbors)
	if err != nil {
		return NeighborEvidence{}, fmt.Errorf("feature: resolve neighbor outcomes for %s: %w", subject, err)
	}

	fingerprintNeighbors, _, err := n.store.NeighborsByKinds(ctx, tenant, subject, []string{cardFingerprintKind}, 0, 0)
	if err != nil {
		return NeighborEvidence{}, fmt.Errorf("feature: resolve fingerprint neighbors for %s: %w", subject, err)
	}

	return NeighborEvidence{
		DeletedCount:         deletedCount,
		LabelledAbusiveCount: labelledAbusiveCount,
		FingerprintShared:    len(fingerprintNeighbors) > 0,
	}, nil
}
