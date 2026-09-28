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
// must NOT trip name_brand_match, matching the design's own reviewed
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
	start := epoch.AddDate(0, 0, idx%40).Add(time.Duration(idx) * time.Minute)
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
	start := epoch.AddDate(0, 0, 3+idx%40).Add(time.Duration(idx) * time.Minute)
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
	start := epoch.AddDate(0, 0, 6+idx%40).Add(time.Duration(idx) * time.Minute)
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
// on later days". Deliberately outside first_day_distinct_domains' window
// (design §8 open question 9: "No v0 feature reacts to a fan-out that
// happens AFTER day 1").
func genSupportDeskLaterFanout(rng *rand.Rand, idx int) ([]event.Event, eval.LabelRow) {
	subject := fmt.Sprintf("acct_gen_supportdesk_%03d", idx)
	start := epoch.AddDate(0, 0, 9+idx%40).Add(time.Duration(idx) * time.Minute)
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
	start := epoch.AddDate(0, 0, 12+idx%40).Add(time.Duration(idx) * time.Minute)
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
	start := epoch.AddDate(0, 0, 15+idx%40).Add(time.Duration(idx) * time.Minute)
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
	start := epoch.AddDate(0, 0, 18+idx%40).Add(time.Duration(idx) * time.Minute)
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
