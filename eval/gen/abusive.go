package main

import (
	"fmt"
	"math/rand"
	"time"

	"github.com/tokencanopy/abusekit/eval"
	"github.com/tokencanopy/abusekit/internal/event"
)

// impersonationNames are brand mentions WITHOUT an integration-adjacent
// word (internal/feature.BrandSet's suppression list) — genuine
// impersonation shapes, unlike benign.go's integrationAgentNames, which
// deliberately DOES pair every brand with one.
var impersonationNames = []string{
	"PayPal Account Alert",
	"Netflix Security Notice",
	"Coinbase Support Team",
	"Wells Fargo Account Verify",
	"Apple ID Locked",
	"Microsoft Account Warning",
	"USPS Delivery Notice",
	"Bank of America Alert",
}

// genBurst: signup, fraud-declined attempts, a prepaid success, a fast
// paid upgrade, then a burst of agents/keys — task brief / design §1's
// "burst" family (criterion 2(a): reaches `high` from onboarding signals
// alone, before any content.sent).
func genBurst(rng *rand.Rand, idx int) ([]event.Event, eval.LabelRow) {
	subject := fmt.Sprintf("acct_gen_burst_%03d", idx)
	start := epoch.AddDate(0, 0, 20+idx%40).Add(time.Duration(idx) * time.Minute)
	b := newBuilder(subject, start)

	b.add(0, "subject.created", event.Links{EmailHash: linkHash("burst-" + subject + "-email")},
		map[string]any{"channel": "signup", "email_domain_class": "disposable", "identity_kind": "individual"})

	declines := 2 + rng.Intn(3) // 2..4
	t := 10 * time.Second
	declineHash := linkHash("burst-" + subject + "-card-declined")
	for i := 0; i < declines; i++ {
		b.add(t, "payment.attempt", event.Links{CardFingerprintHash: declineHash}, map[string]any{"outcome": "declined", "reason": "card_declined", "funding": "credit", "amount_minor": float64(4200), "currency": "usd"})
		t += 10 * time.Second
	}
	successHash := linkHash("burst-" + subject + "-card-success")
	b.add(t, "payment.attempt", event.Links{CardFingerprintHash: successHash}, map[string]any{"outcome": "succeeded", "funding": abusiveFunding(rng), "amount_minor": float64(4200), "currency": "usd"})
	t += 10 * time.Second
	b.add(t, "subscription.changed", event.Links{}, map[string]any{"plan": "plan_b", "status": "active", "amount_minor": float64(4200)})
	t += 10 * time.Second

	n := 4 + rng.Intn(5) // 4..8
	domain := subject + ".example.test"
	for i := 0; i < n; i++ {
		kind := "agent"
		if i%3 == 2 {
			kind = "key"
		}
		b.add(t, "resource.created", event.Links{}, map[string]any{"kind": kind, "name": fmt.Sprintf("Agent %d", i+1), "address_domain": domain})
		t += 10 * time.Second
	}

	return b.events, labelFor(subject, "abusive", "burst", "operator", b.events)
}

// genFast: the same shape as genBurst, compressed so agent creation AND
// the first send both land inside one minute of signup — task brief /
// design §1's "fast" family (criterion 2(b): reaches `high` on the
// synchronous evaluate call before that first send).
func genFast(rng *rand.Rand, idx int) ([]event.Event, eval.LabelRow) {
	subject := fmt.Sprintf("acct_gen_fast_%03d", idx)
	start := epoch.AddDate(0, 0, 24+idx%40).Add(time.Duration(idx) * time.Minute)
	b := newBuilder(subject, start)

	b.add(0, "subject.created", event.Links{EmailHash: linkHash("fast-" + subject + "-email")},
		map[string]any{"channel": "signup", "email_domain_class": "disposable", "identity_kind": "individual"})

	declines := 2 + rng.Intn(2) // 2..3
	t := 2 * time.Second
	declineHash := linkHash("fast-" + subject + "-card-declined")
	for i := 0; i < declines; i++ {
		b.add(t, "payment.attempt", event.Links{CardFingerprintHash: declineHash}, map[string]any{"outcome": "declined", "reason": "card_declined", "funding": "credit", "amount_minor": float64(4200), "currency": "usd"})
		t += 2 * time.Second
	}
	successHash := linkHash("fast-" + subject + "-card-success")
	b.add(t, "payment.attempt", event.Links{CardFingerprintHash: successHash}, map[string]any{"outcome": "succeeded", "funding": abusiveFunding(rng), "amount_minor": float64(4200), "currency": "usd"})
	t += 2 * time.Second
	b.add(t, "subscription.changed", event.Links{}, map[string]any{"plan": "plan_b", "status": "active", "amount_minor": float64(4200)})
	t += 2 * time.Second

	n := 3 + rng.Intn(3) // 3..5
	domain := subject + ".example.test"
	for i := 0; i < n; i++ {
		kind := "agent"
		if i%2 == 1 {
			kind = "key"
		}
		b.add(t, "resource.created", event.Links{}, map[string]any{"kind": kind, "name": fmt.Sprintf("Agent %d", i+1), "address_domain": domain})
		t += 2 * time.Second
	}
	// First send lands well inside 60s of signup.
	b.add(t, "content.sent", event.Links{}, map[string]any{"recipient_domain": "lure-target-1.example.test", "recipient_is_own_identity": false, "recipient_count": float64(1)})

	return b.events, labelFor(subject, "abusive", "fast", "operator", b.events)
}

// genDormantThenBlast: a week-plus-old, otherwise quiet account that
// suddenly creates a burst of agents (one brand-impersonating) and sends
// to many distinct external domains within an hour — task brief / design
// §1's "dormant-then-blast" family. No payment signals at all, matching
// the original design's own reviewed reasoning (see
// eval/fixtures/README.md's dormant_then_blast.jsonl entry).
func genDormantThenBlast(rng *rand.Rand, idx int) ([]event.Event, eval.LabelRow) {
	subject := fmt.Sprintf("acct_gen_dormant_%03d", idx)
	start := epoch.AddDate(0, 0, 28+idx%40).Add(time.Duration(idx) * time.Minute)
	b := newBuilder(subject, start)

	b.add(0, "subject.created", event.Links{EmailHash: linkHash("dormant-" + subject + "-email")},
		map[string]any{"channel": "signup", "email_domain_class": "disposable", "identity_kind": "individual"})

	dormantDays := 7 + rng.Intn(4) // 7..10
	blastStart := time.Duration(dormantDays) * 24 * time.Hour

	n := 8 + rng.Intn(5) // 8..12
	impersonate := impersonationNames[idx%len(impersonationNames)]
	domain := subject + ".example.test"
	t := blastStart
	for i := 0; i < n; i++ {
		// Roughly half agents, half keys — matching the existing
		// hand-written eval/fixtures/dormant_then_blast.jsonl (5
		// agents + 5 keys), which is what makes both
		// resource_velocity_1h AND key_velocity_1h fire together; an
		// agents-only burst under-weights this family relative to the
		// weights it was originally tuned against.
		kind := "agent"
		name := fmt.Sprintf("Agent %s", string(rune('A'+i)))
		if i%2 == 1 {
			kind = "key"
			name = fmt.Sprintf("Key %s", string(rune('A'+i)))
		}
		if i == n/2 {
			kind, name = "agent", impersonate
		}
		b.add(t, "resource.created", event.Links{}, map[string]any{"kind": kind, "name": name, "address_domain": domain})
		t += 90 * time.Second
	}

	sendCount := 15 + rng.Intn(16) // 15..30
	// Sends follow shortly after the resource burst, and the whole burst
	// (agents + sends) stays within resource_velocity_1h's trailing 1h
	// window from the LAST event (the one Slice "full" anchors on) — NOT
	// spread across a further hour, which would push the early
	// resource.created events outside that window by the time the last
	// send lands. Mirrors the existing hand-written
	// eval/fixtures/dormant_then_blast.jsonl, whose burst and blast are
	// ~10 minutes apart, not an hour: 12 agents x 90s (18min) + a 5min
	// gap + 30 sends x 45s (22.5min) = ~45.5min, comfortably under 60.
	sendStart := t + 5*time.Minute
	for i := 0; i < sendCount; i++ {
		b.add(sendStart+time.Duration(i)*45*time.Second, "content.sent", event.Links{}, map[string]any{"recipient_domain": domainName(subject, i), "recipient_is_own_identity": false, "recipient_count": float64(1)})
	}

	return b.events, labelFor(subject, "abusive", "dormant_then_blast", "operator", b.events)
}

// genSlowOperator: an operator that deliberately paces every action to
// stay under velocity-based detection — resources trickle in one at a
// time over many days (never a burst), self-sends rehearse against the
// operator's own inbox spread across that same long window, a brand-
// impersonating name appears partway through, and a modest, slow payment
// pattern (a decline or two, then a prepaid success) plays out over days
// rather than seconds — before an eventual external blast once the
// buildup is done. Task brief's "slow-operator" family; design §8's own
// open questions concede no v0 feature specifically targets a "slow but
// sustained" pattern, so this family's honest recall may legitimately be
// lower than burst/fast/churn's — see the PR body's metrics table.
func genSlowOperator(rng *rand.Rand, idx int) ([]event.Event, eval.LabelRow) {
	subject := fmt.Sprintf("acct_gen_slowop_%03d", idx)
	start := epoch.AddDate(0, 0, 32+idx%40).Add(time.Duration(idx) * time.Minute)
	b := newBuilder(subject, start)
	domain := subject + ".example.test"

	b.add(0, "subject.created", event.Links{EmailHash: linkHash("slowop-" + subject + "-email")},
		map[string]any{"channel": "signup", "email_domain_class": "disposable", "identity_kind": "individual"})

	declines := rng.Intn(2) // 0..1, spread out
	t := 6 * time.Hour
	declineHash := linkHash("slowop-" + subject + "-card")
	for i := 0; i < declines; i++ {
		b.add(t, "payment.attempt", event.Links{CardFingerprintHash: declineHash}, map[string]any{"outcome": "declined", "reason": "card_declined", "funding": "credit", "amount_minor": float64(3100), "currency": "usd"})
		t += 12 * time.Hour
	}
	b.add(t, "payment.attempt", event.Links{CardFingerprintHash: declineHash}, map[string]any{"outcome": "succeeded", "funding": abusiveFunding(rng), "amount_minor": float64(3100), "currency": "usd"})
	t += 6 * time.Hour
	b.add(t, "subscription.changed", event.Links{}, map[string]any{"plan": "plan_b", "status": "active", "amount_minor": float64(3100)})

	impersonate := impersonationNames[(idx+3)%len(impersonationNames)]
	resources := 8 + rng.Intn(6) // 8..13, one per day-ish
	for i := 0; i < resources; i++ {
		day := i + 1
		name := fmt.Sprintf("Agent %d", i+1)
		if i == resources/2 {
			name = impersonate
		}
		b.add(time.Duration(day)*24*time.Hour, "resource.created", event.Links{}, map[string]any{"kind": "agent", "name": name, "address_domain": domain})
		if i%3 == 0 {
			// A self-send rehearsal sprinkled across the same slow window.
			b.add(time.Duration(day)*24*time.Hour+time.Hour, "content.sent", event.Links{}, map[string]any{"recipient_domain": domain, "recipient_is_own_identity": true, "recipient_count": float64(1)})
		}
	}

	blastDay := resources + 2
	sendCount := 10 + rng.Intn(11) // 10..20
	for i := 0; i < sendCount; i++ {
		t := time.Duration(blastDay)*24*time.Hour + time.Duration(i)*3*time.Minute
		b.add(t, "content.sent", event.Links{}, map[string]any{"recipient_domain": domainName(subject, i), "recipient_is_own_identity": false, "recipient_count": float64(1)})
	}

	return b.events, labelFor(subject, "abusive", "slow_operator", "operator", b.events)
}

// webmailBlastDomains are REAL major consumer webmail provider domains
// (config/webmail.yaml's own list — a public fact, not customer data; see
// that file's own header comment) that genWebmailBlast's recipients
// rotate through. Every OTHER family's recipient_domain is a synthetic
// `.example.test` name (public-repo data-boundary rule), which is exactly
// why S2b's webmail_recipient_share/webmail_sends_1h read 0 across the
// entire corpus until this family exists — a real webmail-domain-heavy
// blast needs a real webmail domain to recognize, and a fictional one
// would prove nothing about WebmailSet's actual matching.
var webmailBlastDomains = []string{"gmail.com", "yahoo.com", "outlook.com", "hotmail.com", "icloud.com"}

// genWebmailBlast: the F9 TODO's first new family — a brand-new,
// disposable-email account with the same fast decline/success/upgrade/
// resource-burst shape as genFast, followed by a content.sent blast to
// REAL consumer webmail addresses instead of the corpus's usual synthetic
// domains, so webmail_recipient_share and webmail_sends_1h are actually
// exercised end to end (rather than reading 0 for the whole corpus, as
// they otherwise would). No brand mention anywhere — this family isolates
// the webmail-volume signal from subject_brand_match, which
// genSubjectLure below exercises instead.
func genWebmailBlast(rng *rand.Rand, idx int) ([]event.Event, eval.LabelRow) {
	subject := fmt.Sprintf("acct_gen_webmailblast_%03d", idx)
	start := epoch.AddDate(0, 0, 36+idx%40).Add(time.Duration(idx) * time.Minute)
	b := newBuilder(subject, start)

	b.add(0, "subject.created", event.Links{EmailHash: linkHash("webmailblast-" + subject + "-email")},
		map[string]any{"channel": "signup", "email_domain_class": "disposable", "identity_kind": "individual"})

	declines := 2 + rng.Intn(2) // 2..3
	t := 2 * time.Second
	declineHash := linkHash("webmailblast-" + subject + "-card-declined")
	for i := 0; i < declines; i++ {
		b.add(t, "payment.attempt", event.Links{CardFingerprintHash: declineHash}, map[string]any{"outcome": "declined", "reason": "card_declined", "funding": "credit", "amount_minor": float64(4200), "currency": "usd"})
		t += 2 * time.Second
	}
	successHash := linkHash("webmailblast-" + subject + "-card-success")
	b.add(t, "payment.attempt", event.Links{CardFingerprintHash: successHash}, map[string]any{"outcome": "succeeded", "funding": abusiveFunding(rng), "amount_minor": float64(4200), "currency": "usd"})
	t += 2 * time.Second
	b.add(t, "subscription.changed", event.Links{}, map[string]any{"plan": "plan_b", "status": "active", "amount_minor": float64(4200)})
	t += 2 * time.Second

	n := 3 + rng.Intn(3) // 3..5
	domain := subject + ".example.test"
	for i := 0; i < n; i++ {
		kind := "agent"
		if i%2 == 1 {
			kind = "key"
		}
		b.add(t, "resource.created", event.Links{}, map[string]any{"kind": kind, "name": fmt.Sprintf("Agent %d", i+1), "address_domain": domain})
		t += 2 * time.Second
	}

	sendCount := 20 + rng.Intn(11) // 20..30, all within a 10-minute window
	for i := 0; i < sendCount; i++ {
		wd := webmailBlastDomains[i%len(webmailBlastDomains)]
		b.add(t+time.Duration(i)*20*time.Second, "content.sent", event.Links{}, map[string]any{"recipient_domain": wd, "recipient_is_own_identity": false, "recipient_count": float64(1)})
	}

	return b.events, labelFor(subject, "abusive", "webmail_blast", "operator", b.events)
}

// subjectLureBrands are entirely FICTIONAL brand names (eval/fixtures/
// test_brands.yaml's own set — never a real one, per this repo's hygiene
// rule for anything that fabricates lure-shaped SUBJECT LINE prose, a
// stricter bar than a bare resource name like impersonationNames above:
// a full lure sentence reads closer to reconstructing a real phishing
// template than a name alone does). `make gate` merges test_brands.yaml
// in via --brands-extra specifically so these fictional brands are
// recognized when scoring this corpus (Makefile's gate target).
var subjectLureBrands = []string{"Fictabook", "Fictashop", "Glowbank"}

// subjectLureTemplates pairs each subjectLureBrands entry with one
// lure-shaped subject line — index-aligned with subjectLureBrands, so
// genSubjectLure's idx%3 selects both consistently.
var subjectLureTemplates = []string{
	"Your Fictashop order needs verification",
	"Fictabook: unusual sign-in detected",
	"Glowbank account alert: action required",
}

// genSubjectLure: the F9 TODO's second new family — the same fast decline/
// success/upgrade/resource-burst shape as genWebmailBlast, followed by a
// content.sent blast whose subject_line carries a fictional-brand lure
// (never a real one — see subjectLureBrands), to REGULAR synthetic
// `.example.test` recipients (never a webmail domain — this family
// isolates subject_brand_match from the webmail-volume signal
// genWebmailBlast exercises instead). No resource/agent name mentions a
// brand at all, so any name_brand_match observed on these subjects would
// be a real bug, not this family's own construction.
func genSubjectLure(rng *rand.Rand, idx int) ([]event.Event, eval.LabelRow) {
	subject := fmt.Sprintf("acct_gen_subjectlure_%03d", idx)
	start := epoch.AddDate(0, 0, 40+idx%40).Add(time.Duration(idx) * time.Minute)
	b := newBuilder(subject, start)

	b.add(0, "subject.created", event.Links{EmailHash: linkHash("subjectlure-" + subject + "-email")},
		map[string]any{"channel": "signup", "email_domain_class": "disposable", "identity_kind": "individual"})

	declines := 2 + rng.Intn(2) // 2..3
	t := 2 * time.Second
	declineHash := linkHash("subjectlure-" + subject + "-card-declined")
	for i := 0; i < declines; i++ {
		b.add(t, "payment.attempt", event.Links{CardFingerprintHash: declineHash}, map[string]any{"outcome": "declined", "reason": "card_declined", "funding": "credit", "amount_minor": float64(4200), "currency": "usd"})
		t += 2 * time.Second
	}
	successHash := linkHash("subjectlure-" + subject + "-card-success")
	b.add(t, "payment.attempt", event.Links{CardFingerprintHash: successHash}, map[string]any{"outcome": "succeeded", "funding": abusiveFunding(rng), "amount_minor": float64(4200), "currency": "usd"})
	t += 2 * time.Second
	b.add(t, "subscription.changed", event.Links{}, map[string]any{"plan": "plan_b", "status": "active", "amount_minor": float64(4200)})
	t += 2 * time.Second

	n := 3 + rng.Intn(3) // 3..5, names deliberately brand-free
	domain := subject + ".example.test"
	for i := 0; i < n; i++ {
		kind := "agent"
		if i%2 == 1 {
			kind = "key"
		}
		b.add(t, "resource.created", event.Links{}, map[string]any{"kind": kind, "name": fmt.Sprintf("Agent %d", i+1), "address_domain": domain})
		t += 2 * time.Second
	}

	lureIdx := idx % len(subjectLureBrands)
	subjectLine := subjectLureTemplates[lureIdx]
	sendCount := 15 + rng.Intn(11) // 15..25, all within a 10-minute window
	for i := 0; i < sendCount; i++ {
		b.add(t+time.Duration(i)*20*time.Second, "content.sent", event.Links{}, map[string]any{"subject_line": subjectLine, "recipient_domain": domainName(subject, i), "recipient_is_own_identity": false, "recipient_count": float64(1)})
	}

	return b.events, labelFor(subject, "abusive", "subject_lure", "operator", b.events)
}
