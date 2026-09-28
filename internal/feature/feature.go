// Package feature computes abusekit's v0 feature vector (design §4.5) for
// one subject from its own event history plus same-tenant linking
// evidence: velocity/lifetime counts, the permanent onboarding facts that
// never age out, and the linked_* features fed by internal/store's
// same-tenant identity graph (design §4.2).
//
// Extract is pure except for one injected dependency, Neighbors, which
// resolves the linked_* features' same-tenant evidence — the one piece of
// a subject's feature vector that genuinely lives outside its own event
// history. Everything else is a deterministic fold over the events slice
// the caller (internal/worker, or a test) already has in hand.
package feature

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/tokencanopy/abusekit/internal/event"
)

// resourceKindKey is the resource.created `kind` value the key_* features
// count (design §4.12: "resource.created/deleted (agents, keys)" — v0
// treats any other kind, e.g. "agent", as contributing only to the
// resource_* totals, never to key_*). resource.created's `kind` field is
// free text in internal/event's redaction schema (no enum, unlike e.g.
// email_domain_class), so this is a convention producers must follow, not
// something ingest itself enforces. Compared case/whitespace-insensitively
// (S10 fix round) since it's producer-supplied free text.
const resourceKindKey = "key"

// subjectAgeClampHours and upgradeDelayClampMinutes bound the two
// unbounded-by-construction time features (B5 fix round, proven to
// otherwise swamp the local scorer's linear model for a multi-day-old
// account: a 14-day-old dormant-then-blast subject's raw upgrade_delay_min
// alone could run into the tens of thousands). Both features saturate at
// 24h once an account is at least that old/that far from a paid upgrade,
// which is exactly the design's own suggested treatment.
const (
	subjectAgeClampHours     = 24
	upgradeDelayClampMinutes = 24 * 60
)

// selfSendBeforeExternalCap bounds another unbounded-by-construction count
// (R1 round 2, proven: a developer sending several self-test emails before
// ever emailing anyone else pushed a benign account's score toward `high`
// on this feature alone once the raw count ran high). Saturates rather
// than clamping to a fixed ceiling value the way the time features above
// do, since it's already a small integer with no natural "still
// meaningful past this point" ceiling of its own.
const selfSendBeforeExternalCap = 2

// firstDayDistinctDomainsLogScale scales first_day_distinct_domains'
// log1p(n) curve (D2 round 3) so that n=10 reproduces EXACTLY the
// contribution R1 round 2's hard cap of 10 gave it — chosen so
// config/local_weights.yaml's weight for this feature didn't need to
// change alongside the formula. R1 round 2 capped this feature outright
// (a benign day-1 receipts account fanning out to dozens of genuine
// distinct customer domains pushed a benign score toward `high` on this
// feature alone), but a hard cap makes every count above it read
// identically — a 30-domain account and a 300-domain account would score
// the same, when the latter is a meaningfully bigger fan-out. log1p
// keeps volume sensitivity above the old cap (compressed, not linear —
// the whole point is that a `high`-worthy signal has to come from
// elsewhere too, not from domain count alone) instead of flattening it to
// zero.
var firstDayDistinctDomainsLogScale = 10 / math.Log1p(10)

// Names is the ordered, canonical list of every v0 feature Extract
// computes — matching design §4.5's new_account_velocity inputs list
// exactly. cmd/abusekit builds its FeatureSet from this (not a
// hand-copied literal) so the registered feature names and what Extract
// actually computes can never drift apart. Order doesn't affect scoring
// (internal/core.Plan/Combine only ever look features up by name via
// Features.Map) but is fixed for deterministic iteration/output.
var Names = []string{
	"subject_age_h",
	"resource_velocity_1h",
	"resource_total",
	"key_velocity_1h",
	"key_total",
	"upgrade_delay_min",
	"upgraded",
	"declines_before_first_success",
	"first_funding_prepaid",
	"name_brand_match",
	"name_has_at",
	"first_day_distinct_domains",
	"self_send_before_external",
	"linked_deleted_n",
	"linked_labelled_abusive_n",
	"fingerprint_seen_on_other_subjects",
	"neighbors_truncated",
	"burst_ratio_24h_vs_lifetime",
	"sends_10m_max",
	"sends_1h",
	"sends_first_day",
	"webmail_recipient_share",
	"webmail_sends_1h",
	"distinct_recipients_1h",
	"subject_brand_match",
}

// Features is one subject's v0 feature vector (design §4.5), as of the
// instant a Windows value fixes. A subject with no events yet is the zero
// Features (every field 0) — see Extract.
type Features struct {
	// SubjectAgeH is hours between the subject's first-ever event and
	// Windows.Now, floored at 0 (a clock-skewed event slightly in the
	// future would otherwise make this negative) and capped at
	// subjectAgeClampHours (B5: an old account must not swamp the linear
	// model through this feature alone — burst_ratio_24h_vs_lifetime is
	// what actually distinguishes "old and quiet" from "old and just
	// burst").
	SubjectAgeH float64
	// ResourceVelocity1h / ResourceTotal count every resource.created
	// event (any kind) in the trailing Windows.OneHour window, and over
	// the subject's whole lifetime, respectively.
	ResourceVelocity1h float64
	ResourceTotal      float64
	// KeyVelocity1h / KeyTotal are ResourceVelocity1h/ResourceTotal
	// restricted to resource.created events whose `kind` is "key"
	// (resourceKindKey, compared case/whitespace-insensitively) — API-key
	// creation velocity specifically, since a burst of keys is often a
	// stronger abuse signal than agents alone.
	KeyVelocity1h float64
	KeyTotal      float64
	// UpgradeDelayMin is minutes between the subject's first event and its
	// first PAID subscription.changed event (amount_minor > 0 — B5: a
	// free-plan change or a cancellation must never count as an
	// "upgrade"). A subject with no paid upgrade yet uses
	// minutes-elapsed-so-far instead of a sentinel, clamped (with the paid
	// case) at upgradeDelayClampMinutes so neither a genuinely fast
	// upgrade nor a long-unresolved non-upgrade can swamp the model. See
	// Upgraded for whether a paid upgrade has happened at all — the two
	// are deliberately separate features (design open question / B5):
	// UpgradeDelayMin alone can't distinguish "just upgraded, delay
	// unknown yet" from "never upgraded, waited the full clamp window".
	UpgradeDelayMin float64
	// Upgraded is 1 when the subject has ever recorded a PAID
	// subscription.changed event (amount_minor > 0), else 0.
	Upgraded float64
	// DeclinesBeforeFirstSuccess counts payment.attempt events with
	// outcome "declined" that occurred before the subject's first
	// "succeeded" attempt. Before any success, this is a running count of
	// every decline so far; once a success occurs, it never changes again
	// (a permanent onboarding fact, design §4.5).
	DeclinesBeforeFirstSuccess float64
	// FirstFundingPrepaid is 1 when the subject's first successful
	// payment.attempt's funding was "prepaid", else 0 (including "no
	// successful attempt yet").
	FirstFundingPrepaid float64
	// NameBrandMatch is 1 when any resource.created/resource.deleted
	// event's raw `name` field matches a curated, word/token-boundary-aware
	// brand name (BrandSet, config/brands.yaml, S3 fix round), else 0.
	NameBrandMatch float64
	// NameHasAt is 1 when any resource.created/resource.deleted event's
	// raw `name` field contains a literal "@" (checked on the RAW name,
	// not name_skeleton — Skeleton folds "@" to "a" as a leetspeak
	// confusable, which would otherwise erase the very signal this
	// feature exists to catch), else 0.
	NameHasAt float64
	// FirstDayDistinctDomains counts distinct content.sent
	// recipient_domain values (case-folded, S10 — "Example.TEST" and
	// "example.test" are the same domain) seen within the subject's first
	// 24h (Windows.DayHour) of existence — design's "first-day recipient
	// fan-out" (§1). Unlike the *_24h features below, this window is
	// anchored to the subject's first event, not to Windows.Now: once the
	// first day has passed, this feature is permanently fixed.
	FirstDayDistinctDomains float64
	// SelfSendBeforeExternal counts content.sent events with
	// recipient_is_own_identity true that occurred at or before the
	// subject's first content.sent event with recipient_is_own_identity
	// false (design's "rehearsing against the operator's own inboxes
	// before a blast", §1). 0 when there have been no self-sends, or all
	// self-sends happened after the first external send.
	SelfSendBeforeExternal float64
	// LinkedDeletedN / LinkedLabelledAbusiveN count this subject's
	// same-tenant neighbours (design §4.2, via the injected Neighbors —
	// see Config for which link kinds count as evidence by default)
	// that have ever emitted a PERMANENT subject.deleted event (a
	// trash-mode deletion — reversible within e2a's own retention window —
	// does not count; N3 fix round: a later-restored trash deletion must
	// not read as abandonment forever), or carry an "abusive" label,
	// respectively. LinkedDeletedN saturates at 3 (S1 fix round): past
	// that point additional deleted neighbours are strong-but-not-linearly-
	// stronger evidence, and an unbounded count let a long churn chain
	// dominate the score far past the threshold that already established
	// "this is a churn incarnation".
	LinkedDeletedN         float64
	LinkedLabelledAbusiveN float64
	// FingerprintSeenOnOtherSubjects is 1 when this subject shares a
	// card_fingerprint_hash link with at least one other same-tenant
	// subject, else 0. Unlike LinkedDeletedN/LinkedLabelledAbusiveN, this
	// specifically checks the card_fingerprint_hash link kind (design's
	// incident narrative: "several stolen-card declines" reused across
	// accounts) and is always considered regardless of Config's
	// link-kind-inclusion knobs.
	FingerprintSeenOnOtherSubjects float64
	// NeighborsTruncated is 1 when the same-tenant neighbour discovery
	// backing the linked_* features above hit design §4.2's fan-in cap (S1
	// fix round: the design explicitly calls for surfacing this as a
	// feature — "neighbors_truncated=true (a feature)" — rather than
	// silently under-counting evidence when a link key fans out wide).
	NeighborsTruncated float64
	// BurstRatio24hVsLifetime is the fraction of the subject's lifetime
	// resource.created + content.sent activity that fell within the
	// trailing Windows.DayHour window: close to 1.0 for a brand-new
	// account (nothing to compare against yet) OR an older, previously
	// quiet account that just burst ("dormant-then-blast"); close to 0
	// for an account with a long, currently-quiet history. 0 when the
	// subject has no resource/content activity at all.
	BurstRatio24hVsLifetime float64
	// Sends10mMax is the LARGEST sum of content.sent recipient_count
	// within any 10-minute-wide window across the subject's history up to
	// Windows.Now, capped at sendsVolumeCap and gated by
	// youngAccountFactor — B1 fix round: an established sender's routine
	// burst must not read the same as a brand-new signup's; see
	// youngAccountWindow's own doc comment.
	Sends10mMax float64
	// Sends1h is the sum of content.sent recipient_count in the trailing
	// Windows.OneHour window, capped at sendsVolumeCap and gated by
	// youngAccountFactor.
	Sends1h float64
	// SendsFirstDay is the sum of content.sent recipient_count within the
	// subject's first 24h (Windows.DayHour) of existence, anchored to
	// firstSeenAt exactly like FirstDayDistinctDomains — permanently
	// fixed once that window closes, and deliberately NOT gated by
	// youngAccountFactor (it can never reflect an established account's
	// CURRENT behaviour in the first place).
	SendsFirstDay float64
	// WebmailRecipientShare is the LIFETIME share (0..1) of sent
	// recipients whose recipient_domain is on the loaded webmail list — a
	// permanent fact, not a decaying window, and not youngAccountFactor-
	// gated (it measures WHO an account emails, not how much).
	WebmailRecipientShare float64
	// WebmailSends1h is Sends1h restricted to webmail-domain recipients,
	// computed directly rather than as WebmailRecipientShare*Sends1h (S7
	// fix round — see webmailSends1h's own doc comment for why that
	// product would be wrong), capped at sendsVolumeCap and gated by
	// youngAccountFactor.
	WebmailSends1h float64
	// DistinctRecipients1h counts distinct content.sent recipient_hash
	// values in the trailing Windows.OneHour window (falling back to
	// summing recipient_count for any event with no hash at all), capped
	// at sendsVolumeCap and gated by youngAccountFactor.
	DistinctRecipients1h float64
	// SubjectBrandMatch counts DISTINCT curated brands matched across
	// every content.sent subject_line in the trailing Windows.OneHour
	// window, excluding any brand already counted by NameBrandMatch (S2
	// fix round) and applying S1 fix round's subject-line-specific
	// integration exemption (an account whose own resource/agent name
	// already carries an integration token has every subject line
	// exempted outright), capped at subjectBrandMatchCap.
	SubjectBrandMatch float64
}

// Map converts f into the map[string]float64 shape internal/core.Plan and
// Combine consume (via a rule's `inputs` selecting a subset by name).
func (f Features) Map() map[string]float64 {
	return map[string]float64{
		"subject_age_h":                      f.SubjectAgeH,
		"resource_velocity_1h":               f.ResourceVelocity1h,
		"resource_total":                     f.ResourceTotal,
		"key_velocity_1h":                    f.KeyVelocity1h,
		"key_total":                          f.KeyTotal,
		"upgrade_delay_min":                  f.UpgradeDelayMin,
		"upgraded":                           f.Upgraded,
		"declines_before_first_success":      f.DeclinesBeforeFirstSuccess,
		"first_funding_prepaid":              f.FirstFundingPrepaid,
		"name_brand_match":                   f.NameBrandMatch,
		"name_has_at":                        f.NameHasAt,
		"first_day_distinct_domains":         f.FirstDayDistinctDomains,
		"self_send_before_external":          f.SelfSendBeforeExternal,
		"linked_deleted_n":                   f.LinkedDeletedN,
		"linked_labelled_abusive_n":          f.LinkedLabelledAbusiveN,
		"fingerprint_seen_on_other_subjects": f.FingerprintSeenOnOtherSubjects,
		"neighbors_truncated":                f.NeighborsTruncated,
		"burst_ratio_24h_vs_lifetime":        f.BurstRatio24hVsLifetime,
		"sends_10m_max":                      f.Sends10mMax,
		"sends_1h":                           f.Sends1h,
		"sends_first_day":                    f.SendsFirstDay,
		"webmail_recipient_share":            f.WebmailRecipientShare,
		"webmail_sends_1h":                   f.WebmailSends1h,
		"distinct_recipients_1h":             f.DistinctRecipients1h,
		"subject_brand_match":                f.SubjectBrandMatch,
	}
}

// Windows fixes the "as of" evaluation instant plus the sliding-window
// durations design §4.5 calls out by name (*_1h, *_24h) — a struct rather
// than separate parameters so a caller can't accidentally transpose them,
// and so a future window (say, *_7d) is one new field, not a new Extract
// parameter.
type Windows struct {
	// Now is the evaluation instant every window looks back from, and
	// SubjectAgeH/UpgradeDelayMin measure forward from the subject's first
	// event to. Required — Extract errors if it's zero, rather than
	// silently substituting a real wall-clock time.Now(), which would make
	// every feature involving Now non-deterministic and untestable against
	// fixed (including fictional, historical-replay) timestamps.
	Now time.Time
	// OneHour and DayHour are the *_1h / *_24h window durations. Production
	// always uses DefaultWindows' 1h/24h; a test may substitute other
	// durations to exercise window-boundary logic without needing
	// hour-scale fixture data.
	OneHour time.Duration
	DayHour time.Duration
}

// DefaultWindows returns the production Windows (1h / 24h) evaluated as of
// now.
func DefaultWindows(now time.Time) Windows {
	return Windows{Now: now, OneHour: time.Hour, DayHour: 24 * time.Hour}
}

// NeighborEvidence is the same-tenant identity-graph evidence the
// linked_* features need about one subject (design §4.2).
type NeighborEvidence struct {
	// DeletedCount is how many of the subject's same-tenant neighbours
	// have ever emitted a PERMANENT subject.deleted event (see
	// Features.LinkedDeletedN).
	DeletedCount int
	// LabelledAbusiveCount is how many of the subject's same-tenant
	// neighbours carry an "abusive" label.
	LabelledAbusiveCount int
	// FingerprintShared reports whether the subject shares a
	// card_fingerprint_hash link with at least one other same-tenant
	// subject.
	FingerprintShared bool
	// Truncated reports whether discovering the evidence above hit design
	// §4.2's fan-in cap (S1 fix round) — see Features.NeighborsTruncated.
	Truncated bool
}

// Neighbors resolves NeighborEvidence for one subject. Extract depends on
// this narrow interface rather than on internal/store directly, so a unit
// test can supply canned evidence without a database (see NoNeighbors),
// and so the link-kind-inclusion policy (design's open question, Config)
// lives in the one implementation that actually queries link kinds
// (StoreNeighbors), not in Extract itself.
type Neighbors interface {
	Evidence(ctx context.Context, tenant, subject string) (NeighborEvidence, error)
}

// NoNeighbors is a Neighbors that reports no same-tenant linking evidence
// at all. Useful for a caller (or test) that hasn't wired a real store —
// or genuinely has no same-tenant graph to consult — and wants the
// linked_* features held at their zero value rather than passing a nil
// interface around.
var NoNeighbors Neighbors = noNeighbors{}

type noNeighbors struct{}

func (noNeighbors) Evidence(context.Context, string, string) (NeighborEvidence, error) {
	return NeighborEvidence{}, nil
}

// Result is Extract's output: this round's feature vector, plus the
// earliest instant an already-computed window feature would change even
// with no new event (design §4.8: "next_rescore_at = earliest
// feature-window expiry"). The zero time.Time means "nothing pending" —
// no window feature is currently keeping any event artificially "current"
// past its natural expiry, so only a new event (bumping dirty_seq) will
// bring the subject back into the worker's queue. internal/worker combines
// this with rule-retry and budget-reset scheduling (design §4.8 / B4 fix
// round) that Extract has no visibility into.
type Result struct {
	Features      Features
	NextRescoreAt time.Time
}

// Extract computes every v0 feature for one subject from events — every
// event.Event ever stored for (tenant, subject), sorted oldest-first
// (internal/store.EventsForSubject's own contract already guarantees this
// order; Extract does not re-sort or deduplicate) — plus neighbors'
// same-tenant linking evidence and a brand list, as of windows.Now.
//
// events empty returns the zero Result (every feature 0/false, no pending
// rescore), never an error: "no events yet" is a normal state (e.g. a
// brand-new subject row created by the same event that's about to be
// processed, or an internal/synthetic subject nobody ever sends real
// events for), not a caller mistake.
//
// neighbors nil is treated as NoNeighbors, a convenience for a caller (or
// test) that doesn't care about the linked_* features. brands' zero value
// (BrandSet{}) holds name_brand_match/subject_brand_match at 0; webmail's
// zero value (WebmailSet{}) holds webmail_recipient_share/
// webmail_sends_1h at 0 (S2b).
func Extract(ctx context.Context, tenant, subject string, events []event.Event, neighbors Neighbors, windows Windows, brands BrandSet, webmail WebmailSet) (Result, error) {
	if windows.Now.IsZero() {
		return Result{}, fmt.Errorf("feature: windows.Now must be set")
	}
	if windows.OneHour <= 0 || windows.DayHour <= 0 {
		return Result{}, fmt.Errorf("feature: windows.OneHour and windows.DayHour must both be positive")
	}
	if neighbors == nil {
		neighbors = NoNeighbors
	}
	if len(events) == 0 {
		return Result{}, nil
	}

	now := windows.Now
	// firstSeenAt is the minimum At across every event, not events[0].At:
	// Extract's documented contract is oldest-first delivery (matching
	// internal/store.EventsForSubject), but computing the true minimum
	// costs nothing extra and stays correct even for a caller (or test)
	// that hands Extract events out of order — see minAt's doc comment.
	firstSeenAt, _ := minAt(events, func(event.Event) bool { return true })

	ev, err := neighbors.Evidence(ctx, tenant, subject)
	if err != nil {
		return Result{}, fmt.Errorf("feature: resolve neighbor evidence for %s: %w", subject, err)
	}

	// S2b: computed once and shared between NameBrandMatch and
	// SubjectBrandMatch (S2 fix round's double-counting cap) and between
	// SubjectBrandMatch and S1 fix round's subject-line integration
	// exemption.
	namedBrands := namedBrandNames(events, brands)
	accountIntegrationName := accountHasIntegrationName(events)

	f := Features{
		SubjectAgeH:                    subjectAgeHours(firstSeenAt, now),
		ResourceVelocity1h:             resourceCount(events, "", now, windows.OneHour),
		ResourceTotal:                  resourceCount(events, "", now, 0),
		KeyVelocity1h:                  resourceCount(events, resourceKindKey, now, windows.OneHour),
		KeyTotal:                       resourceCount(events, resourceKindKey, now, 0),
		UpgradeDelayMin:                upgradeDelayMinutes(events, firstSeenAt, now),
		Upgraded:                       upgraded(events),
		DeclinesBeforeFirstSuccess:     declinesBeforeFirstSuccess(events),
		FirstFundingPrepaid:            firstFundingPrepaid(events),
		NameBrandMatch:                 boolToFloat(len(namedBrands) > 0),
		NameHasAt:                      nameHasAt(events),
		FirstDayDistinctDomains:        firstDayDistinctDomains(events, firstSeenAt, windows.DayHour),
		SelfSendBeforeExternal:         selfSendBeforeExternal(events),
		LinkedDeletedN:                 saturate(ev.DeletedCount, 3),
		LinkedLabelledAbusiveN:         float64(ev.LabelledAbusiveCount),
		FingerprintSeenOnOtherSubjects: boolToFloat(ev.FingerprintShared),
		NeighborsTruncated:             boolToFloat(ev.Truncated),
		BurstRatio24hVsLifetime:        burstRatio(events, now, windows.DayHour),
		Sends10mMax:                    sends10mMax(events, now, firstSeenAt),
		Sends1h:                        sends1h(events, now, firstSeenAt, windows.OneHour),
		SendsFirstDay:                  sendsFirstDay(events, firstSeenAt, now, windows.DayHour),
		WebmailRecipientShare:          webmailRecipientShare(events, now, webmail),
		WebmailSends1h:                 webmailSends1h(events, now, firstSeenAt, windows.OneHour, webmail),
		DistinctRecipients1h:           distinctRecipients1h(events, now, firstSeenAt, windows.OneHour),
		SubjectBrandMatch:              subjectBrandMatch(events, now, windows.OneHour, brands, namedBrands, accountIntegrationName),
	}

	return Result{
		Features:      f,
		NextRescoreAt: nextRescoreAt(events, now, firstSeenAt, windows),
	}, nil
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// saturate caps n at max (S1 fix round: linked_deleted_n's saturating cap).
func saturate(n, max int) float64 {
	if n > max {
		n = max
	}
	return float64(n)
}

// dataString, dataBool and dataNumber read a possibly-absent,
// possibly-wrong-typed key out of an event's already-redacted Data map.
// internal/event.Redact guarantees a listed field's dynamic type matches
// its declared kind, but Extract has no way to know an event actually went
// through Redact (a test building event.Event by hand, or a future caller
// feeding it raw), so every read here degrades to "absent" on a type
// mismatch rather than panicking on a failed type assertion.
func dataString(data map[string]any, key string) (string, bool) {
	v, ok := data[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func dataBool(data map[string]any, key string) (bool, bool) {
	v, ok := data[key]
	if !ok {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}

// dataNumber reads a numeric field. encoding/json decodes every JSON
// number into float64, which is also what a test constructing
// map[string]any by hand should use (as the real ingest path's redacted
// events always do).
func dataNumber(data map[string]any, key string) (float64, bool) {
	v, ok := data[key]
	if !ok {
		return 0, false
	}
	f, ok := v.(float64)
	return f, ok
}
