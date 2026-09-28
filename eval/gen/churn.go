package main

import (
	"fmt"
	"math/rand"
	"time"

	"github.com/tokencanopy/abusekit/eval"
	"github.com/tokencanopy/abusekit/internal/event"
)

// incarnation is one churn chain member: its full event history and its
// ground-truth label row.
type incarnation struct {
	events []event.Event
	label  eval.LabelRow
}

// genChurnChain builds one churn chain of length incarnations, all
// sharing a single link hash of the given kind ("email", "card", or
// "device") — task brief's "churn (email/card/device-linked variants)".
// Every OTHER link kind gets a distinct, per-incarnation hash, so each
// variant isolates exactly one kind's contribution to
// internal/feature.LinkedDeletedN/FingerprintSeenOnOtherSubjects, all
// three of which are in internal/feature.defaultNeighborKinds (S1's
// default candidate set: email_hash, card_fingerprint_hash, device_hash)
// — see eval/neighbors.go's own defaultNeighborKinds.
//
// Each incarnation mirrors the existing eval/fixtures/churn.jsonl shape:
// signup -> a prepaid success -> a fast paid upgrade -> one resource ->
// a PERMANENT delete, all within under a minute — then the chain moves
// on to the next incarnation. Ground truth labels every incarnation
// "abusive" (including the first two, before linked_deleted_n has
// saturated) — design §1.2(c)'s "every subject from the third onward at
// high" is a claim about the SCORER's recall, not about ground truth;
// the corpus's job is to let eval.Run measure that honestly, including
// wherever the first two incarnations are legitimately missed.
func genChurnChain(rng *rand.Rand, kind string, chainIdx, length int) []incarnation {
	sharedHash := linkHash(fmt.Sprintf("churn-%s-chain-%d-shared", kind, chainIdx))
	baseStart := epoch.AddDate(0, 0, 40+chainIdx*3).Add(time.Duration(chainIdx) * time.Hour)

	incs := make([]incarnation, 0, length)
	for n := 0; n < length; n++ {
		subject := fmt.Sprintf("acct_gen_churn_%s_%d_%d", kind, chainIdx, n+1)
		start := baseStart.Add(time.Duration(n) * 90 * time.Second)
		b := newBuilder(subject, start)
		perIncSeed := fmt.Sprintf("churn-%s-chain-%d-inc-%d", kind, chainIdx, n)

		createLinks := event.Links{}
		paymentLinks := event.Links{}
		switch kind {
		case "email":
			createLinks.EmailHash = sharedHash
			paymentLinks.CardFingerprintHash = linkHash(perIncSeed + "-card")
		case "card":
			createLinks.EmailHash = linkHash(perIncSeed + "-email")
			paymentLinks.CardFingerprintHash = sharedHash
		case "device":
			createLinks.EmailHash = linkHash(perIncSeed + "-email")
			createLinks.DeviceHash = sharedHash
			paymentLinks.CardFingerprintHash = linkHash(perIncSeed + "-card")
		}

		b.add(0, "subject.created", createLinks, map[string]any{"channel": "signup", "email_domain_class": "disposable", "identity_kind": "individual"})
		b.add(10*time.Second, "payment.attempt", paymentLinks, map[string]any{"outcome": "succeeded", "funding": "prepaid", "amount_minor": float64(1500), "currency": "usd"})
		b.add(20*time.Second, "subscription.changed", event.Links{}, map[string]any{"plan": "pro", "status": "active", "amount_minor": float64(1500)})
		b.add(30*time.Second, "resource.created", event.Links{}, map[string]any{"kind": "agent", "name": fmt.Sprintf("Agent %d", n+1), "address_domain": subject + ".example.test"})
		b.add(40*time.Second, "subject.deleted", event.Links{}, map[string]any{"mode": "permanent"})

		incs = append(incs, incarnation{
			events: b.events,
			label:  labelFor(subject, "abusive", "churn_"+kind, "operator", b.events),
		})
	}
	return incs
}
