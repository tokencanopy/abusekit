package main

import (
	"fmt"
	"math/rand"
	"time"

	"github.com/tokencanopy/abusekit/eval"
	"github.com/tokencanopy/abusekit/internal/event"
)

// integrationAgentNames pairs a config/brands.yaml brand with an
// integration-adjacent word (internal/feature.BrandSet's own suppression
// list: integration, webhook, sync, relay, notifier, tracking, bot,
// connector, api, import, export) — a legitimate integration name that
// must NOT trip brand.name_match, matching the design's own reviewed
// false-positive cases ("Stripe Webhook Relay", "PayPal integration")
// generalized across every brand in the shipped list, not just the two
// or three the hand-written fixtures happened to cover.
var integrationAgentNames = []string{
	"Stripe Webhook Relay",
	"PayPal Sync Bot",
	"Google Calendar Sync",
	"Amazon Order Importer",
	"Microsoft Teams Connector",
	"DHL Tracking Relay",
	"Coinbase API Notifier",
	"Binance Webhook Relay",
	"Wells Fargo Sync Bot",
	"FedEx Tracking Connector",
	"USPS Import Relay",
	"Netflix Billing Notifier",
	"Walmart Order Sync",
	"Bank of America Export Bot",
}

// genFastDevOnboarding: a developer setting up a few agents and keys
// quickly, sending several self-test emails, rarely (if ever) sending
// externally the same day — task brief's "fast developer onboarding with
// self-tests".
func genFastDevOnboarding(rng *rand.Rand, idx int) ([]event.Event, eval.LabelRow) {
	subject := fmt.Sprintf("acct_gen_fastdev_%03d", idx)
	start := epoch.AddDate(0, 0, idx%40).Add(time.Duration(idx)*time.Minute + secondJitter(rng))
	b := newBuilder(subject, start)
	domain := subject + ".example.test"

	b.add(0, "subject.created", event.Links{EmailHash: linkHash("fastdev-" + subject + "-email")},
		map[string]any{"channel": "signup", "email_domain_class": "corporate", "identity_kind": "individual"})

	agents := 3 + rng.Intn(3) // 3..5
	t := time.Duration(0)
	for i := 0; i < agents; i++ {
		t += time.Duration(60+rng.Intn(120)) * time.Second
		b.add(t, "resource.created", event.Links{}, map[string]any{"kind": "agent", "name": fmt.Sprintf("Dev Agent %d", i+1), "address_domain": domain})
	}
	t += time.Duration(60+rng.Intn(60)) * time.Second
	b.add(t, "resource.created", event.Links{}, map[string]any{"kind": "key", "name": "Dev Key"})

	selfSends := 2 + rng.Intn(4) // 2..5
	for i := 0; i < selfSends; i++ {
		t += time.Duration(2+rng.Intn(8)) * time.Minute
		b.add(t, "content.sent", event.Links{}, map[string]any{"recipient_domain": domain, "recipient_is_own_identity": true, "recipient_count": float64(1)})
	}
	if rng.Intn(10) < 3 { // 30% of the time, one ordinary external send once setup is done
		t += time.Duration(10+rng.Intn(20)) * time.Minute
		b.add(t, "content.sent", event.Links{}, map[string]any{"recipient_domain": "partner-1.example.test", "recipient_is_own_identity": false, "recipient_count": float64(1)})
	}

	return b.events, labelFor(subject, "benign", "", "outcome", b.events)
}

// genIntegrationHeavy: an org wiring up several real SaaS integrations
// whose agents are literally named after the brand they integrate with —
// task brief's "integration-heavy orgs whose agents have SaaS brand names
// plus integration words".
func genIntegrationHeavy(rng *rand.Rand, idx int) ([]event.Event, eval.LabelRow) {
	subject := fmt.Sprintf("acct_gen_integheavy_%03d", idx)
	start := epoch.AddDate(0, 0, 3+idx%40).Add(time.Duration(idx)*time.Minute + secondJitter(rng))
	b := newBuilder(subject, start)
	domain := subject + ".example.test"

	b.add(0, "subject.created", event.Links{EmailHash: linkHash("integheavy-" + subject + "-email")},
		map[string]any{"channel": "signup", "email_domain_class": "corporate", "identity_kind": "individual"})

	n := 6 + rng.Intn(3) // 6..8
	t := time.Duration(0)
	for i := 0; i < n; i++ {
		t += time.Duration(3+rng.Intn(9)) * time.Minute
		name := integrationAgentNames[(idx+i)%len(integrationAgentNames)]
		kind := "agent"
		if i == n-1 {
			kind = "key"
		}
		b.add(t, "resource.created", event.Links{}, map[string]any{"kind": kind, "name": name, "address_domain": domain})
	}

	sends := 2 + rng.Intn(3) // a modest amount of real traffic, not a fan-out
	for i := 0; i < sends; i++ {
		t += time.Duration(10+rng.Intn(30)) * time.Minute
		b.add(t, "content.sent", event.Links{}, map[string]any{"recipient_domain": domainName(subject, i%3), "recipient_is_own_identity": false, "recipient_count": float64(1)})
	}

	return b.events, labelFor(subject, "benign", "", "outcome", b.events)
}

// genDay1ReceiptsFanout: a receipts/notification agent fanning out to a
// realistic range of distinct customer domains on day 1 — task brief's
// "day-1 receipts fan-out to 10–40 domains" (a RANGE, unlike the existing
// hand-written benign_receipts_fanout.jsonl's single fixed count of 30 —
// see internal/feature.firstDayDistinctDomainsLogScale's doc comment for
// why the weights were tuned against that one fixed point, not a range).
func genDay1ReceiptsFanout(rng *rand.Rand, idx int) ([]event.Event, eval.LabelRow) {
	subject := fmt.Sprintf("acct_gen_receipts_%03d", idx)
	start := epoch.AddDate(0, 0, 6+idx%40).Add(time.Duration(idx)*time.Minute + secondJitter(rng))
	b := newBuilder(subject, start)
	domain := subject + ".example.test"

	b.add(0, "subject.created", event.Links{}, map[string]any{"channel": "signup", "email_domain_class": "webmail", "identity_kind": "individual"})
	b.add(5*time.Second, "resource.created", event.Links{}, map[string]any{"kind": "agent", "name": "Receipts Agent", "address_domain": domain})

	n := 10 + rng.Intn(31) // 10..40
	t := 2 * time.Minute
	for i := 0; i < n; i++ {
		b.add(t, "content.sent", event.Links{}, map[string]any{"recipient_domain": domainName(subject, i), "recipient_is_own_identity": false, "recipient_count": float64(1)})
		t += time.Duration(1+rng.Intn(3)) * time.Minute
	}

	return b.events, labelFor(subject, "benign", "", "outcome", b.events)
}

// genSupportDeskLaterFanout: a support inbox that only starts replying to
// many distinct domains several days after signup, spread across several
// following days — task brief's "support desks replying to many domains
// on later days". Deliberately outside email.first_day_distinct_domains' window
// (design §8 open question 9: "No v0 feature reacts to a fan-out that
// happens AFTER day 1").
func genSupportDeskLaterFanout(rng *rand.Rand, idx int) ([]event.Event, eval.LabelRow) {
	subject := fmt.Sprintf("acct_gen_supportdesk_%03d", idx)
	start := epoch.AddDate(0, 0, 9+idx%40).Add(time.Duration(idx)*time.Minute + secondJitter(rng))
	b := newBuilder(subject, start)
	domain := subject + ".example.test"

	b.add(0, "subject.created", event.Links{}, map[string]any{"channel": "signup", "email_domain_class": "corporate", "identity_kind": "individual"})
	b.add(10*time.Minute, "resource.created", event.Links{}, map[string]any{"kind": "agent", "name": "Support Desk Agent", "address_domain": domain})

	startDay := 5 + rng.Intn(3)      // day 5..7
	replyDays := 3 + rng.Intn(3)     // reply across 3..5 following days
	domainsPerDay := 4 + rng.Intn(7) // 4..10 distinct domains/day
	domainCounter := 0
	for day := 0; day < replyDays; day++ {
		dayOffset := time.Duration(startDay+day) * 24 * time.Hour
		for i := 0; i < domainsPerDay; i++ {
			t := dayOffset + time.Duration(i)*20*time.Minute
			b.add(t, "content.sent", event.Links{}, map[string]any{"recipient_domain": domainName(subject, domainCounter), "recipient_is_own_identity": false, "recipient_count": float64(1)})
			domainCounter++
		}
	}

	return b.events, labelFor(subject, "benign", "", "outcome", b.events)
}

// genNewsletterLaterFanout: a single later-day burst to many distinct
// domains at once (as opposed to the support-desk family's gradual
// multi-day reply pattern) — task brief's "newsletter-style later-day
// fan-out".
func genNewsletterLaterFanout(rng *rand.Rand, idx int) ([]event.Event, eval.LabelRow) {
	subject := fmt.Sprintf("acct_gen_newsletter_%03d", idx)
	start := epoch.AddDate(0, 0, 12+idx%40).Add(time.Duration(idx)*time.Minute + secondJitter(rng))
	b := newBuilder(subject, start)
	domain := subject + ".example.test"

	b.add(0, "subject.created", event.Links{}, map[string]any{"channel": "signup", "email_domain_class": "corporate", "identity_kind": "individual"})
	b.add(30*time.Minute, "resource.created", event.Links{}, map[string]any{"kind": "agent", "name": "Newsletter Agent", "address_domain": domain})

	blastDay := 8 + rng.Intn(5) // day 8..12
	n := 20 + rng.Intn(31)      // 20..50 distinct domains in one sitting
	base := time.Duration(blastDay) * 24 * time.Hour
	for i := 0; i < n; i++ {
		t := base + time.Duration(i)*30*time.Second
		b.add(t, "content.sent", event.Links{}, map[string]any{"recipient_domain": domainName(subject, i), "recipient_is_own_identity": false, "recipient_count": float64(1)})
	}

	return b.events, labelFor(subject, "benign", "", "outcome", b.events)
}

// genSlowUpgrader: a handful of ordinary declined payment attempts (not
// prepaid, no fraud pattern), a real paid upgrade only after several days,
// and steady, unremarkable send volume — task brief's "slow upgraders".
func genSlowUpgrader(rng *rand.Rand, idx int) ([]event.Event, eval.LabelRow) {
	subject := fmt.Sprintf("acct_gen_slowupg_%03d", idx)
	start := epoch.AddDate(0, 0, 15+idx%40).Add(time.Duration(idx)*time.Minute + secondJitter(rng))
	b := newBuilder(subject, start)
	domain := subject + ".example.test"

	b.add(0, "subject.created", event.Links{}, map[string]any{"channel": "signup", "email_domain_class": "corporate", "identity_kind": "individual"})

	declines := rng.Intn(3) // 0..2 — an ordinary card typo/expired-card retry, not a fraud pattern
	fundings := []string{"credit", "debit"}
	t := 5 * time.Minute
	for i := 0; i < declines; i++ {
		b.add(t, "payment.attempt", event.Links{}, map[string]any{"outcome": "declined", "reason": "card_declined", "funding": fundings[rng.Intn(len(fundings))], "amount_minor": float64(2900), "currency": "usd"})
		t += 2 * time.Minute
	}
	b.add(t, "resource.created", event.Links{}, map[string]any{"kind": "agent", "name": "Ops Agent", "address_domain": domain})

	upgradeDay := 3 + rng.Intn(8) // 3..10 days later, genuinely slow
	b.add(time.Duration(upgradeDay)*24*time.Hour, "payment.attempt", event.Links{}, map[string]any{"outcome": "succeeded", "funding": fundings[rng.Intn(len(fundings))], "amount_minor": float64(2900), "currency": "usd"})
	b.add(time.Duration(upgradeDay)*24*time.Hour+time.Minute, "subscription.changed", event.Links{}, map[string]any{"plan": "pro", "status": "active", "amount_minor": float64(2900)})

	sends := 3 + rng.Intn(6) // 3..8, spread over the following days
	for i := 0; i < sends; i++ {
		day := upgradeDay + 1 + i
		b.add(time.Duration(day)*24*time.Hour, "content.sent", event.Links{}, map[string]any{"recipient_domain": domainName(subject, i%5), "recipient_is_own_identity": false, "recipient_count": float64(1)})
	}

	return b.events, labelFor(subject, "benign", "", "outcome", b.events)
}

// genTrialZeroDollar: a free-trial account ($0 subscription, no card on
// file) with light, ordinary usage — task brief's "trial accounts with $0
// subscriptions". firstPaidUpgradeAt (internal/feature/windows.go)
// requires amount_minor > 0, so this correctly reports Upgraded == 0.
func genTrialZeroDollar(rng *rand.Rand, idx int) ([]event.Event, eval.LabelRow) {
	subject := fmt.Sprintf("acct_gen_trial_%03d", idx)
	start := epoch.AddDate(0, 0, 18+idx%40).Add(time.Duration(idx)*time.Minute + secondJitter(rng))
	b := newBuilder(subject, start)
	domain := subject + ".example.test"

	b.add(0, "subject.created", event.Links{}, map[string]any{"channel": "signup", "email_domain_class": "webmail", "identity_kind": "individual"})
	b.add(5*time.Minute, "subscription.changed", event.Links{}, map[string]any{"plan": "trial", "status": "trialing", "amount_minor": float64(0)})

	agents := 1 + rng.Intn(2) // 1..2
	t := 10 * time.Minute
	for i := 0; i < agents; i++ {
		b.add(t, "resource.created", event.Links{}, map[string]any{"kind": "agent", "name": fmt.Sprintf("Trial Agent %d", i+1), "address_domain": domain})
		t += 5 * time.Minute
	}

	sends := 1 + rng.Intn(3) // 1..3, over the following days
	for i := 0; i < sends; i++ {
		day := 1 + i*2
		b.add(time.Duration(day)*24*time.Hour, "content.sent", event.Links{}, map[string]any{"recipient_domain": domainName(subject, i), "recipient_is_own_identity": false, "recipient_count": float64(1)})
	}

	return b.events, labelFor(subject, "benign", "", "outcome", b.events)
}

// genBenignPrepaid: an ordinary customer whose card just happens to be
// prepaid, succeeding on the first attempt with no other signal firing —
// task brief's "benign prepaid" (fix round S3). core.first_funding_prepaid
// (internal/feature) is a real fraud signal in the abusive families
// (burst/fast/churn all pay with a prepaid card specifically because a
// stolen card is often prepaid), but plenty of genuine customers use one
// too; this family exists so the corpus doesn't silently teach "prepaid
// implies abusive" by simply never showing a benign counter-example.
//
// Fix round T5: half of this family (idx odd) instead goes through
// genBenignPrepaidEagerDay0 — a genuinely hard near-miss whose risk
// straddles config/rules.yaml's 0.6 new_account_velocity threshold
// (worked out by hand against config/local_weights.yaml's shipped
// weights: 1-2 agents + 1 key + 1-2 external domains, all same-day,
// lands ~0.45-0.65 depending on this instance's own jitter) rather than
// the original mild variant's comfortably-low score, so the gate's
// precision/recall aren't measured only against corpus subjects that
// are trivially easy to classify either way.
func genBenignPrepaid(rng *rand.Rand, idx int) ([]event.Event, eval.LabelRow) {
	if idx%2 == 1 {
		return genBenignPrepaidEagerDay0(rng, idx)
	}
	subject := fmt.Sprintf("acct_gen_prepaid_%03d", idx)
	start := epoch.AddDate(0, 0, 36+idx%40).Add(time.Duration(idx)*time.Minute + secondJitter(rng))
	b := newBuilder(subject, start)
	domain := subject + ".example.test"

	b.add(0, "subject.created", event.Links{}, map[string]any{"channel": "signup", "email_domain_class": "corporate", "identity_kind": "individual"})
	b.add(3*time.Minute, "payment.attempt", event.Links{}, map[string]any{"outcome": "succeeded", "funding": "prepaid", "amount_minor": float64(1900), "currency": "usd"})
	b.add(4*time.Minute, "subscription.changed", event.Links{}, map[string]any{"plan": "starter", "status": "active", "amount_minor": float64(1900)})

	agents := 1 + rng.Intn(3) // 1..3
	t := 10 * time.Minute
	for i := 0; i < agents; i++ {
		b.add(t, "resource.created", event.Links{}, map[string]any{"kind": "agent", "name": fmt.Sprintf("Agent %d", i+1), "address_domain": domain})
		t += time.Duration(3+rng.Intn(5)) * time.Minute
	}
	sends := 2 + rng.Intn(4) // 2..5, over the following days
	for i := 0; i < sends; i++ {
		day := 1 + i
		b.add(time.Duration(day)*24*time.Hour, "content.sent", event.Links{}, map[string]any{"recipient_domain": domainName(subject, i%3), "recipient_is_own_identity": false, "recipient_count": float64(1)})
	}

	return b.events, labelFor(subject, "benign", "", "outcome", b.events)
}

// genBenignPrepaidEagerDay0: an eager customer — pays with a prepaid card,
// wires up an agent and a key, and starts sending to a couple of real
// external domains all within the SAME DAY it signs up. Every one of
// those is individually a real (if weak) fraud signal in
// config/local_weights.yaml (core.first_funding_prepaid, core.resource_velocity_1h/
// core.credential_velocity_1h, email.first_day_distinct_domains, core.burst_ratio_24h_vs_lifetime
// all fire), and stacking them together is exactly what an actually
// abusive account also looks like on day 0 — the honest difference is
// only that this account keeps behaving ordinarily afterward and this
// activity is genuinely its own, not a stolen card. Task brief (fix round
// T5): "benign prepaid subjects with day-0 activity that lands near the
// rule threshold" — this is a hard case by design, not a bug; some
// instances will legitimately cross 0.6 and count against precision.
func genBenignPrepaidEagerDay0(rng *rand.Rand, idx int) ([]event.Event, eval.LabelRow) {
	subject := fmt.Sprintf("acct_gen_prepaid_%03d", idx)
	start := epoch.AddDate(0, 0, 36+idx%40).Add(time.Duration(idx)*time.Minute + secondJitter(rng))
	b := newBuilder(subject, start)
	domain := subject + ".example.test"

	b.add(0, "subject.created", event.Links{}, map[string]any{"channel": "signup", "email_domain_class": "corporate", "identity_kind": "individual"})
	b.add(3*time.Minute, "payment.attempt", event.Links{}, map[string]any{"outcome": "succeeded", "funding": "prepaid", "amount_minor": float64(1900), "currency": "usd"})
	b.add(4*time.Minute, "subscription.changed", event.Links{}, map[string]any{"plan": "starter", "status": "active", "amount_minor": float64(1900)})

	agents := 1 + rng.Intn(2) // 1..2 — enough to move core.resource_velocity_1h, not a full blast
	t := 10*time.Minute + secondJitter(rng)
	for i := 0; i < agents; i++ {
		b.add(t, "resource.created", event.Links{}, map[string]any{"kind": "agent", "name": fmt.Sprintf("Agent %d", i+1), "address_domain": domain})
		t += time.Duration(3+rng.Intn(5))*time.Minute + secondJitter(rng)
	}
	b.add(t, "resource.created", event.Links{}, map[string]any{"kind": "key", "name": "API Key"})
	t += time.Duration(2+rng.Intn(4))*time.Minute + secondJitter(rng)

	domains := 1 + rng.Intn(2) // 1..2 distinct domains, same day as signup
	for i := 0; i < domains; i++ {
		b.add(t, "content.sent", event.Links{}, map[string]any{"recipient_domain": domainName(subject, i), "recipient_is_own_identity": false, "recipient_count": float64(1)})
		t += time.Duration(5+rng.Intn(15))*time.Minute + secondJitter(rng)
	}
	// Ordinary usage continues afterward — the honest signal that this
	// wasn't a blast-and-abandon account, even though day 0 alone reads
	// as a near-miss.
	moreSends := 1 + rng.Intn(3) // 1..3, over the following days
	for i := 0; i < moreSends; i++ {
		day := 1 + i
		b.add(time.Duration(day)*24*time.Hour, "content.sent", event.Links{}, map[string]any{"recipient_domain": domainName(subject, (domains+i)%3), "recipient_is_own_identity": false, "recipient_count": float64(1)})
	}

	return b.events, labelFor(subject, "benign", "", "outcome", b.events)
}

// genBenignDeclineThenSuccess: an ordinary customer who mistypes their
// card number (or it's briefly expired) once or twice before it goes
// through, with unremarkable usage afterward — task brief's "benign
// decline-then-success" (fix round S3). core.declines_before_first_success
// (weight 0.6) is a real fraud signal in burst/fast (repeated stolen-card
// attempts), but an ordinary fat-fingered retry looks identical at the
// feature level; this family is the benign counter-example.
func genBenignDeclineThenSuccess(rng *rand.Rand, idx int) ([]event.Event, eval.LabelRow) {
	subject := fmt.Sprintf("acct_gen_declinesuccess_%03d", idx)
	start := epoch.AddDate(0, 0, 37+idx%40).Add(time.Duration(idx)*time.Minute + secondJitter(rng))
	b := newBuilder(subject, start)
	domain := subject + ".example.test"

	b.add(0, "subject.created", event.Links{}, map[string]any{"channel": "signup", "email_domain_class": "webmail", "identity_kind": "individual"})

	t := 2 * time.Minute
	declines := 1 + rng.Intn(2) // 1..2 — a mistyped number or an expired card, not a fraud pattern
	for i := 0; i < declines; i++ {
		b.add(t, "payment.attempt", event.Links{}, map[string]any{"outcome": "declined", "reason": "card_declined", "funding": "credit", "amount_minor": float64(2400), "currency": "usd"})
		t += time.Duration(1+rng.Intn(3)) * time.Minute
	}
	b.add(t, "payment.attempt", event.Links{}, map[string]any{"outcome": "succeeded", "funding": "debit", "amount_minor": float64(2400), "currency": "usd"})
	t += time.Minute
	b.add(t, "subscription.changed", event.Links{}, map[string]any{"plan": "pro", "status": "active", "amount_minor": float64(2400)})
	t += 5 * time.Minute

	agents := 1 + rng.Intn(3) // 1..3
	for i := 0; i < agents; i++ {
		b.add(t, "resource.created", event.Links{}, map[string]any{"kind": "agent", "name": fmt.Sprintf("Agent %d", i+1), "address_domain": domain})
		t += time.Duration(3+rng.Intn(6)) * time.Minute
	}
	sends := 1 + rng.Intn(4) // 1..4
	for i := 0; i < sends; i++ {
		day := 1 + i
		b.add(time.Duration(day)*24*time.Hour, "content.sent", event.Links{}, map[string]any{"recipient_domain": domainName(subject, i%3), "recipient_is_own_identity": false, "recipient_count": float64(1)})
	}

	return b.events, labelFor(subject, "benign", "", "outcome", b.events)
}

// genBenignSharedCardHousehold builds a small group (2..3) of INDEPENDENT
// benign accounts sharing the same card_fingerprint_hash — task brief's
// "benign shared-card households" (fix round S3): a family or small team
// paying with one shared card. core.fingerprint_seen_on_other_subjects (weight
// 1.0) fires for every member, exactly as it would for a stolen-card
// churn ring — this family is the honest counter-example proving the
// feature alone isn't damning; each member is otherwise unremarkable and
// never deleted.
func genBenignSharedCardHousehold(rng *rand.Rand, groupIdx int) []incarnation {
	sharedCard := linkHash(fmt.Sprintf("household-%d-shared-card", groupIdx))
	size := 2 + rng.Intn(2) // 2..3 members
	base := epoch.AddDate(0, 0, 38+groupIdx%40).Add(time.Duration(groupIdx)*time.Hour + secondJitter(rng))

	members := make([]incarnation, 0, size)
	for m := 0; m < size; m++ {
		subject := fmt.Sprintf("acct_gen_household_%03d_%d", groupIdx, m+1)
		start := base.Add(time.Duration(m) * time.Duration(10+rng.Intn(20)) * time.Minute)
		b := newBuilder(subject, start)
		domain := subject + ".example.test"

		b.add(0, "subject.created", event.Links{}, map[string]any{"channel": "signup", "email_domain_class": "corporate", "identity_kind": "individual"})
		b.add(2*time.Minute, "payment.attempt", event.Links{CardFingerprintHash: sharedCard}, map[string]any{"outcome": "succeeded", "funding": "credit", "amount_minor": float64(1500), "currency": "usd"})
		b.add(3*time.Minute, "subscription.changed", event.Links{}, map[string]any{"plan": "starter", "status": "active", "amount_minor": float64(1500)})

		agents := 1 + rng.Intn(2) // 1..2
		t := 10 * time.Minute
		for i := 0; i < agents; i++ {
			b.add(t, "resource.created", event.Links{}, map[string]any{"kind": "agent", "name": fmt.Sprintf("Agent %d", i+1), "address_domain": domain})
			t += time.Duration(5+rng.Intn(10)) * time.Minute
		}
		sends := 1 + rng.Intn(3)
		for i := 0; i < sends; i++ {
			day := 1 + i
			b.add(time.Duration(day)*24*time.Hour, "content.sent", event.Links{}, map[string]any{"recipient_domain": domainName(subject, i%3), "recipient_is_own_identity": false, "recipient_count": float64(1)})
		}

		members = append(members, incarnation{events: b.events, label: labelFor(subject, "benign", "shared_card_household", "outcome", b.events)})
	}
	return members
}

// genBenignLegitResignup builds a deleted-then-resignup PAIR sharing an
// email_hash — task brief's "benign legitimate re-signups" (fix round
// S3): subject A uses the product for a while, then voluntarily and
// permanently deletes its account (nothing abusive about it — people
// leave products); much later subject B signs up again with the SAME
// email (a returning customer, or a family member sharing an inbox) and
// behaves completely ordinarily. core.linked_deleted_n (weight 1.3) fires for
// B the same way it would for a churn incarnation's second-or-later
// member — this pair is the honest counter-example: ONE deleted
// neighbour, on its own, is not damning.
func genBenignLegitResignup(rng *rand.Rand, pairIdx int) []incarnation {
	sharedEmail := linkHash(fmt.Sprintf("resignup-%d-shared-email", pairIdx))
	base := epoch.AddDate(0, 0, 39+pairIdx%40).Add(time.Duration(pairIdx)*time.Hour + secondJitter(rng))

	aSubject := fmt.Sprintf("acct_gen_resignup_%03d_a", pairIdx)
	aStart := base
	a := newBuilder(aSubject, aStart)
	aDomain := aSubject + ".example.test"
	a.add(0, "subject.created", event.Links{EmailHash: sharedEmail}, map[string]any{"channel": "signup", "email_domain_class": "webmail", "identity_kind": "individual"})
	a.add(1*time.Hour, "resource.created", event.Links{}, map[string]any{"kind": "agent", "name": "Agent 1", "address_domain": aDomain})
	aUsageDays := 3 + rng.Intn(10) // used it for a while before leaving
	a.add(time.Duration(aUsageDays)*24*time.Hour, "content.sent", event.Links{}, map[string]any{"recipient_domain": domainName(aSubject, 0), "recipient_is_own_identity": false, "recipient_count": float64(1)})
	a.add(time.Duration(aUsageDays)*24*time.Hour+time.Hour, "subject.deleted", event.Links{}, map[string]any{"mode": "permanent"}) // voluntary, unremarkable churn

	// B signs up weeks later, sharing A's email, and behaves ordinarily —
	// never deleted, no other signal.
	bSubject := fmt.Sprintf("acct_gen_resignup_%03d_b", pairIdx)
	bStart := aStart.Add(time.Duration(aUsageDays)*24*time.Hour + time.Duration(14+rng.Intn(30))*24*time.Hour)
	b := newBuilder(bSubject, bStart)
	bDomain := bSubject + ".example.test"
	b.add(0, "subject.created", event.Links{EmailHash: sharedEmail}, map[string]any{"channel": "signup", "email_domain_class": "webmail", "identity_kind": "individual"})
	agents := 1 + rng.Intn(2)
	t := 10 * time.Minute
	for i := 0; i < agents; i++ {
		b.add(t, "resource.created", event.Links{}, map[string]any{"kind": "agent", "name": fmt.Sprintf("Agent %d", i+1), "address_domain": bDomain})
		t += time.Duration(5+rng.Intn(10)) * time.Minute
	}
	sends := 1 + rng.Intn(3)
	for i := 0; i < sends; i++ {
		day := 1 + i
		b.add(time.Duration(day)*24*time.Hour, "content.sent", event.Links{}, map[string]any{"recipient_domain": domainName(bSubject, i%3), "recipient_is_own_identity": false, "recipient_count": float64(1)})
	}

	return []incarnation{
		{events: a.events, label: labelFor(aSubject, "benign", "legit_resignup", "outcome", a.events)},
		{events: b.events, label: labelFor(bSubject, "benign", "legit_resignup", "outcome", b.events)},
	}
}
