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
// signup -> (occasionally a decline or two first) -> a prepaid success ->
// a fast paid upgrade -> one or two resources -> a SEND OR TWO (fix round
// S3: "churn families must include sends" — the operator rehearses/
// blasts before abandoning the incarnation, not just creates a resource
// and vanishes) -> a PERMANENT delete, all within a couple of minutes —
// then the chain moves on to the next incarnation. Every count and every
// gap is seeded jitter (fix round S3: "add jitter/variance to every
// abusive family"), so two chains (or the same chain under a different
// --seed) are never byte-identical in shape.
//
// Ground truth labels every incarnation "abusive" (including the first
// two, before linked_deleted_n has saturated) — design §1.2(c)'s "every
// subject from the third onward at high" is a claim about the SCORER's
// recall, not about ground truth; the corpus's job is to let eval.Run
// measure that honestly, including wherever the first two incarnations
// are legitimately missed.
func genChurnChain(rng *rand.Rand, kind string, chainIdx, length int) []incarnation {
	sharedHash := linkHash(fmt.Sprintf("churn-%s-chain-%d-shared", kind, chainIdx))
	baseStart := epoch.AddDate(0, 0, 40+chainIdx*3).Add(time.Duration(chainIdx) * time.Hour)

	incs := make([]incarnation, 0, length)
	gap := time.Duration(0)
	for n := 0; n < length; n++ {
		subject := fmt.Sprintf("acct_gen_churn_%s_%d_%d", kind, chainIdx, n+1)
		start := baseStart.Add(gap)
		gap += time.Duration(60+rng.Intn(120)) * time.Second // 60..180s between incarnations, jittered
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

		t := 5 * time.Second
		declines := rng.Intn(2) // 0..1
		for i := 0; i < declines; i++ {
			b.add(t, "payment.attempt", paymentLinks, map[string]any{"outcome": "declined", "reason": "card_declined", "funding": "prepaid", "amount_minor": float64(1500), "currency": "usd"})
			t += time.Duration(5+rng.Intn(10)) * time.Second
		}
		b.add(t, "payment.attempt", paymentLinks, map[string]any{"outcome": "succeeded", "funding": "prepaid", "amount_minor": float64(1500), "currency": "usd"})
		t += time.Duration(5+rng.Intn(10)) * time.Second
		b.add(t, "subscription.changed", event.Links{}, map[string]any{"plan": "pro", "status": "active", "amount_minor": float64(1500)})
		t += time.Duration(5+rng.Intn(10)) * time.Second

		resources := 1 + rng.Intn(2) // 1..2
		domain := subject + ".example.test"
		for i := 0; i < resources; i++ {
			b.add(t, "resource.created", event.Links{}, map[string]any{"kind": "agent", "name": fmt.Sprintf("Agent %d", i+1), "address_domain": domain})
			t += time.Duration(5+rng.Intn(10)) * time.Second
		}

		sends := 1 + rng.Intn(2) // 1..2 external sends before the operator abandons this incarnation
		for i := 0; i < sends; i++ {
			b.add(t, "content.sent", event.Links{}, map[string]any{"recipient_domain": domainName(subject, i), "recipient_is_own_identity": false, "recipient_count": float64(1)})
			t += time.Duration(5+rng.Intn(10)) * time.Second
		}

		b.add(t, "subject.deleted", event.Links{}, map[string]any{"mode": "permanent"})
		t += time.Duration(10+rng.Intn(20)) * time.Second
		// Fix round T4: an operator/outcome label recorded shortly after
		// takedown, so linked_labelled_abusive_n (design §4.2, weight
		// 1.5) is actually exercised by this corpus at all — B1 closed
		// off ground-truth leakage into this feature, so without an
		// EVENT like this one, nothing anywhere in the generated corpus
		// would ever set it. gen.go's decisionAtMap excludes `label`
		// events from this incarnation's own "last event" anchor, so
		// this never trips the B2 decision_after_deletion check.
		b.add(t, labelEventType, event.Links{}, map[string]any{"label": "abusive"})

		incs = append(incs, incarnation{
			events: b.events,
			label:  labelFor(subject, "abusive", "churn_"+kind, "operator", b.events),
		})
	}
	return incs
}
