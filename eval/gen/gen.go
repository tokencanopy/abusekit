// Command gen is abusekit's seeded synthetic-corpus generator (task
// brief: "Generate them with a seeded generator in eval/gen/"). Generate
// is deterministic given the same Options.Seed — see gen_test.go's
// TestGenerate_Deterministic — and produces every value from a public,
// documented convention so the committed corpus (eval/fixtures/synthetic/)
// is reproducible and auditable rather than a black box:
//
//   - subject ids: acct_gen_<family>_<index> (or
//     acct_gen_churn_<kind>_<chain>_<n> for a churn incarnation).
//   - domains: <slug>.example.test (public-repo data boundary: AGENTS.md,
//     eval/fixtures/README.md).
//   - link hashes: sha256("fixture:<seed>") for a short, descriptive,
//     generator-local seed string — see linkHash and
//     eval/fixtures/README.md's table for the exact seed each family
//     uses.
//   - timestamps: 2031 (this repo's existing fictional-year convention),
//     built up from a fixed epoch with small seeded jitter, never a real
//     wall-clock instant.
//
// No brand, domain, name, or timing here is copied from the September
// 2026 incident beyond the general shapes design §1 already describes in
// public prose (AGENTS.md: "the corpus is redacted synthetic-or-
// anonymised").
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"sort"
	"time"

	"github.com/tokencanopy/abusekit/eval"
	"github.com/tokencanopy/abusekit/internal/event"
)

// epoch anchors every generated timestamp — 2031, matching the existing
// eval/fixtures/*.jsonl convention (never a real date).
var epoch = time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)

// Options controls Generate's output.
type Options struct {
	// Seed drives every random choice (domain counts, jitter, decline
	// counts, ...). The same Seed always produces byte-identical output.
	Seed int64
	// BenignPerFamily / AbusivePerFamily set how many subjects each
	// family contributes. Zero means "use the default" (22 / 6 — see
	// Generate's doc comment for why those defaults clear the task
	// brief's >=150/>=40 floors with margin).
	BenignPerFamily  int
	AbusivePerFamily int
	// ChurnChainsPerKind / ChurnChainLength set how many chains (and how
	// many incarnations per chain) each of the three churn variants
	// (email/card/device-linked) contributes. Zero means "use the
	// default" (2 chains x 5 incarnations = 10 subjects per kind, 30
	// churn subjects total).
	ChurnChainsPerKind int
	ChurnChainLength   int
}

// Result is Generate's output: the events and labels that
// eval.LoadReplayDataset reads as an event-replay pair, plus a manifest
// of which family produced which subject (used only by gen_test.go and
// the CLI's summary printout — never read back by eval itself).
type Result struct {
	Events []event.Event
	Labels []eval.LabelRow
	// FamilyOf maps every generated subject id to its family name, for
	// reporting (gen_test.go's family-coverage assertions; the CLI's
	// "N benign across M families" summary).
	FamilyOf map[string]string
}

func (o Options) withDefaults() Options {
	if o.BenignPerFamily <= 0 {
		o.BenignPerFamily = 22 // 7 families x 22 = 154, clearing the >=150 floor with margin
	}
	if o.AbusivePerFamily <= 0 {
		o.AbusivePerFamily = 6 // burst, fast, dormant_then_blast, slow_operator x 6 = 24
	}
	if o.ChurnChainsPerKind <= 0 {
		o.ChurnChainsPerKind = 2 // x3 kinds x 5-subject chains = 30 churn subjects
	}
	if o.ChurnChainLength <= 0 {
		o.ChurnChainLength = 5
	}
	return o
}

// benignFamilies enumerates the seven required shapes (task brief) in a
// fixed order, so Generate's iteration — and therefore its output — is
// deterministic regardless of Go map iteration.
var benignFamilies = []struct {
	name string
	gen  func(rng *rand.Rand, idx int) ([]event.Event, eval.LabelRow)
}{
	{"benign_fast_dev_onboarding", genFastDevOnboarding},
	{"benign_integration_heavy", genIntegrationHeavy},
	{"benign_day1_receipts_fanout", genDay1ReceiptsFanout},
	{"benign_support_desk_later_fanout", genSupportDeskLaterFanout},
	{"benign_newsletter_later_fanout", genNewsletterLaterFanout},
	{"benign_slow_upgrader", genSlowUpgrader},
	{"benign_trial_zero_dollar", genTrialZeroDollar},
}

var abusiveFamilies = []struct {
	name string
	gen  func(rng *rand.Rand, idx int) ([]event.Event, eval.LabelRow)
}{
	{"abusive_burst", genBurst},
	{"abusive_fast", genFast},
	{"abusive_dormant_then_blast", genDormantThenBlast},
	{"abusive_slow_operator", genSlowOperator},
}

var churnKinds = []string{"email", "card", "device"}

// Generate builds the full synthetic corpus: Options.BenignPerFamily
// subjects for each of the seven benign families (default 22 each, 154
// total — clearing the task brief's ">=150 benign subjects across the
// realistic families" floor), Options.AbusivePerFamily subjects for each
// of burst/fast/dormant-then-blast/slow-operator (default 6 each, 24
// total), plus Options.ChurnChainsPerKind churn chains of
// Options.ChurnChainLength incarnations for each of the three
// email/card/device-linked variants (default 2x5=10 per kind, 30 total)
// — 54 abusive subjects total, clearing the ">=40 abusive subjects
// across burst, fast, churn (email/card/device-linked variants),
// dormant-then-blast and slow-operator families" floor.
//
// Every random draw comes from a single *rand.Rand seeded from
// Options.Seed, consumed in the FIXED family/index order above — this is
// what makes Generate deterministic: reordering the loops, or drawing
// randomness in a different sequence, would change the output for an
// unchanged Seed, which is exactly what gen_test.go's determinism check
// would catch.
func Generate(opts Options) Result {
	opts = opts.withDefaults()
	rng := rand.New(rand.NewSource(opts.Seed))

	res := Result{FamilyOf: map[string]string{}}
	add := func(family string, events []event.Event, label eval.LabelRow) {
		res.Events = append(res.Events, events...)
		res.Labels = append(res.Labels, label)
		res.FamilyOf[label.Subject] = family
	}

	for _, f := range benignFamilies {
		for i := 0; i < opts.BenignPerFamily; i++ {
			events, label := f.gen(rng, i)
			add(f.name, events, label)
		}
	}
	for _, f := range abusiveFamilies {
		for i := 0; i < opts.AbusivePerFamily; i++ {
			events, label := f.gen(rng, i)
			add(f.name, events, label)
		}
	}
	for _, kind := range churnKinds {
		for chain := 0; chain < opts.ChurnChainsPerKind; chain++ {
			for _, inc := range genChurnChain(rng, kind, chain, opts.ChurnChainLength) {
				add("abusive_churn_"+kind, inc.events, inc.label)
			}
		}
	}

	sort.Slice(res.Events, func(i, j int) bool {
		if res.Events[i].Subject != res.Events[j].Subject {
			return res.Events[i].Subject < res.Events[j].Subject
		}
		return res.Events[i].At.Before(res.Events[j].At)
	})
	sort.Slice(res.Labels, func(i, j int) bool { return res.Labels[i].Subject < res.Labels[j].Subject })

	return res
}

// --- shared generation helpers ------------------------------------------------

// linkHash is sha256("fixture:"+seed) hex-encoded — matching
// eval/fixtures/README.md's documented convention (never a real per-
// tenant HMAC, never derived from anything real).
func linkHash(seed string) string {
	sum := sha256.Sum256([]byte("fixture:" + seed))
	return hex.EncodeToString(sum[:])
}

// builder accumulates one subject's events with auto-incrementing ids
// and explicit per-event timestamps (start + an offset), matching
// event.Event's own wire shape exactly (no separate wire type needed —
// see gen.go's package doc comment).
type builder struct {
	subject string
	start   time.Time
	seq     int
	events  []event.Event
}

func newBuilder(subject string, start time.Time) *builder {
	return &builder{subject: subject, start: start}
}

// at returns b.start + offset — every family generator computes its
// event timestamps this way, so the whole subject's timeline reads as a
// simple table of offsets from its own start.
func (b *builder) at(offset time.Duration) time.Time {
	return b.start.Add(offset)
}

func (b *builder) add(offset time.Duration, typ string, links event.Links, data map[string]any) {
	b.seq++
	b.events = append(b.events, event.Event{
		ID:      fmt.Sprintf("%s-evt-%03d", b.subject, b.seq),
		Subject: b.subject,
		Type:    typ,
		At:      b.at(offset),
		Links:   links,
		Data:    data,
	})
}

// decisionAtMap derives a labels-row decision_at map from a subject's own
// events (task brief's replay shape): "full" is always the last event's
// At plus 1ns; "early_15m" is always the first event's At plus 15
// minutes (regardless of whether that instant falls before or after the
// last real event — eval.LoadReplayDataset's strictly-before filter
// handles either case correctly); "first_send" is included only when the
// subject has at least one EXTERNAL (recipient_is_own_identity == false)
// content.sent event, at that event's own At.
func decisionAtMap(events []event.Event) map[string]string {
	first, last := events[0].At, events[0].At
	var firstExternal time.Time
	haveExternal := false
	for _, e := range events {
		if e.At.Before(first) {
			first = e.At
		}
		if e.At.After(last) {
			last = e.At
		}
		if e.Type == "content.sent" {
			if own, ok := e.Data["recipient_is_own_identity"].(bool); ok && !own {
				if !haveExternal || e.At.Before(firstExternal) {
					firstExternal = e.At
					haveExternal = true
				}
			}
		}
	}
	m := map[string]string{
		"full":      last.Add(time.Nanosecond).UTC().Format(time.RFC3339Nano),
		"early_15m": first.Add(15 * time.Minute).UTC().Format(time.RFC3339Nano),
	}
	if haveExternal {
		m["first_send"] = firstExternal.UTC().Format(time.RFC3339Nano)
	}
	return m
}

func labelFor(subject, label, category, source string, events []event.Event) eval.LabelRow {
	return eval.LabelRow{Subject: subject, Label: label, Category: category, Source: source, DecisionAt: decisionAtMap(events)}
}

// domainName returns a deterministic, distinct .example.test domain for
// index n under a per-subject slug — used by every fan-out family.
func domainName(slug string, n int) string {
	return fmt.Sprintf("customer-%d.%s.example.test", n, slug)
}
