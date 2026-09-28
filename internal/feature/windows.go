package feature

import (
	"math"
	"sort"
	"strings"
	"time"

	"github.com/tokencanopy/abusekit/internal/event"
)

// rescoreCoalesceInterval is the granularity design's next_rescore_at
// scheduling rounds up to (S6 fix round, proven: 60 sends in quick
// succession each independently scheduling their own 1h-window-exit
// rescore produced 59 extra full scoring rounds rather than one shared
// one). Rounding every candidate UP to the next multiple of this interval
// since the Unix epoch means nearby candidates — including ones computed
// by different Tick passes — land on the same bucket and share one
// rescore instead of each getting its own.
const rescoreCoalesceInterval = 5 * time.Minute

// windowedEventTypes are the only event types a *_1h/*_24h window feature
// actually reads (resourceCount, burstRatio) — S6 fix round: nextRescoreAt
// previously scheduled a window-exit rescore for ANY event type, including
// payment.attempt/subscription.changed/subject.created, none of which any
// windowed feature ever looks at, so those events were generating pure
// waste (a scheduled rescore that, once run, changes nothing).
func isWindowedEventType(t string) bool {
	return t == "resource.created" || t == "content.sent"
}

// hasEventType reports whether events contains at least one event of type
// t (S2b: nextRescoreAt uses this to skip scheduling the young-account
// cutover for a subject with no content.sent history at all).
func hasEventType(events []event.Event, t string) bool {
	for _, e := range events {
		if e.Type == t {
			return true
		}
	}
	return false
}

// withinWindow reports whether at falls in the half-open window
// (now-window, now] — i.e. strictly newer than window ago, and not newer
// than now itself. An event exactly window-old is excluded (it has just
// expired); an event exactly at now is included. A future-dated event
// (at after now — possible under clock skew) is excluded from every
// window, though it still counts toward an unwindowed lifetime total.
func withinWindow(at, now time.Time, window time.Duration) bool {
	return at.After(now.Add(-window)) && !at.After(now)
}

// subjectAgeHours is hours between firstSeenAt and now, floored at 0 (a
// clock-skewed event slightly ahead of now must never report a negative
// age) and capped at subjectAgeClampHours (B5 fix round: an old account
// must not swamp the linear model through this feature alone).
func subjectAgeHours(firstSeenAt, now time.Time) float64 {
	h := now.Sub(firstSeenAt).Hours()
	if h < 0 {
		return 0
	}
	if h > subjectAgeClampHours {
		return subjectAgeClampHours
	}
	return h
}

// normalizeToken lower-cases and trims s — S10 fix round: resource.created's
// `kind` and content.sent's `recipient_domain` are producer-supplied free
// text with no enum, so "Key"/"key "/"KEY" and "Example.TEST"/
// "example.test" must compare and dedupe as the same value.
func normalizeToken(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// resourceKindAliases maps a producer's free-text resource.created `kind`
// variant to the canonical value normalizeResourceKind returns — the
// scope's "resource-kind aliases" item, extended by S2b's N4 fix round to
// also accept "api key" (a literal space) and the plural "api_keys"/
// "keys": whichever convention a producer writes this field with, it
// should never silently read as ordinary (uncounted) resource activity
// instead of the key-specific signal it actually is. Every entry here is
// already normalizeToken-folded (lower-case, trimmed) since that's how it
// is looked up below.
var resourceKindAliases = map[string]string{
	"key":      resourceKindKey,
	"keys":     resourceKindKey,
	"api_key":  resourceKindKey,
	"api_keys": resourceKindKey,
	"api-key":  resourceKindKey,
	"apikey":   resourceKindKey,
	"api key":  resourceKindKey,
}

// normalizeResourceKind canonicalises resource.created/deleted's
// producer-supplied, free-text `kind` field: case/whitespace-insensitive
// (normalizeToken, S10 fix round, already true) plus resourceKindAliases'
// spelling variants (S2b, N4 fix round). A kind outside the alias table
// passes through normalizeToken's folding unchanged, never rejected — the
// vocabulary here is a documented convention producers SHOULD follow, not
// something ingest itself enforces (design §4.12).
func normalizeResourceKind(raw string) string {
	n := normalizeToken(raw)
	if canonical, ok := resourceKindAliases[n]; ok {
		return canonical
	}
	return n
}

// resourceCount counts resource.created events, optionally restricted to a
// specific `kind` and/or a trailing window ending at now.
//
//   - kind == "" counts every kind.
//   - window <= 0 means "no window" (the lifetime total); window > 0
//     restricts to events within (now-window, now] per withinWindow.
func resourceCount(events []event.Event, kind string, now time.Time, window time.Duration) float64 {
	var n float64
	for _, e := range events {
		if e.Type != "resource.created" {
			continue
		}
		if kind != "" {
			k, ok := dataString(e.Data, "kind")
			if !ok || normalizeResourceKind(k) != kind {
				continue
			}
		}
		if window > 0 && !withinWindow(e.At, now, window) {
			continue
		}
		n++
	}
	return n
}

// minAt returns the earliest At among events for which keep reports true,
// regardless of events' own order — Extract's contract is oldest-first
// delivery (matching internal/store.EventsForSubject), but every "first X"
// helper here scans for the true minimum rather than trusting that
// contract and returning on the first match, so a late-delivered-but-
// chronologically-earlier event (a backfill, a retried batch, plain
// network reordering — design §5's "duplicate/out-of-order/late events")
// is never missed.
func minAt(events []event.Event, keep func(event.Event) bool) (at time.Time, ok bool) {
	for _, e := range events {
		if !keep(e) {
			continue
		}
		if !ok || e.At.Before(at) {
			at = e.At
			ok = true
		}
	}
	return at, ok
}

// firstPaidUpgradeAt returns the At of the subject's earliest PAID AND
// ACTIVE subscription.changed event (status == "active" AND
// amount_minor > 0). B5 fix round: a free-plan change or a cancellation
// (amount_minor absent, zero, or negative) must never count as an
// "upgrade" — proven, the original "any subscription.changed" rule let a
// benign free-to-free plan switch look identical to a paid upgrade. R5
// round 2: a "trialing" subscription with a price on file is NOT yet an
// upgrade either — proven, a trial that never converts would otherwise
// read identically to an account that actually started paying; only
// "active" (the status a producer's billing webhook sets once the charge
// itself succeeds, not merely quoted) counts.
func firstPaidUpgradeAt(events []event.Event) (at time.Time, ok bool) {
	return minAt(events, func(e event.Event) bool {
		if e.Type != "subscription.changed" {
			return false
		}
		status, _ := dataString(e.Data, "status")
		if status != "active" {
			return false
		}
		amount, ok := dataNumber(e.Data, "amount_minor")
		return ok && amount > 0
	})
}

// upgraded is 1 when the subject has ever recorded a paid subscription.changed
// event, else 0 — see Features.Upgraded.
func upgraded(events []event.Event) float64 {
	_, ok := firstPaidUpgradeAt(events)
	return boolToFloat(ok)
}

// upgradeDelayMinutes is minutes between firstSeenAt and the subject's
// first (earliest) PAID subscription.changed event, clamped at
// upgradeDelayClampMinutes; see Features.UpgradeDelayMin's doc comment for
// the "no paid upgrade yet" fallback and the clamp's purpose (B5 fix
// round).
func upgradeDelayMinutes(events []event.Event, firstSeenAt, now time.Time) float64 {
	var minutes float64
	if at, ok := firstPaidUpgradeAt(events); ok {
		minutes = at.Sub(firstSeenAt).Minutes()
	} else {
		minutes = now.Sub(firstSeenAt).Minutes()
	}
	if minutes < 0 {
		minutes = 0
	}
	if minutes > upgradeDelayClampMinutes {
		minutes = upgradeDelayClampMinutes
	}
	return minutes
}

// declinesBeforeFirstSuccess counts payment.attempt events with outcome
// "declined" up to (and not including) the subject's first (earliest)
// "succeeded" attempt — or all declines, if there is no success yet.
func declinesBeforeFirstSuccess(events []event.Event) float64 {
	firstSuccess, haveSuccess := minAt(events, func(e event.Event) bool {
		if e.Type != "payment.attempt" {
			return false
		}
		outcome, _ := dataString(e.Data, "outcome")
		return outcome == "succeeded"
	})

	var n float64
	for _, e := range events {
		if e.Type != "payment.attempt" {
			continue
		}
		outcome, _ := dataString(e.Data, "outcome")
		if outcome != "declined" {
			continue
		}
		if haveSuccess && e.At.After(firstSuccess) {
			continue
		}
		n++
	}
	return n
}

// firstFundingPrepaid is 1 when the subject's first (earliest) successful
// payment.attempt's funding was "prepaid", else 0.
func firstFundingPrepaid(events []event.Event) float64 {
	var (
		firstAt      time.Time
		firstFunding string
		have         bool
	)
	for _, e := range events {
		if e.Type != "payment.attempt" {
			continue
		}
		outcome, _ := dataString(e.Data, "outcome")
		if outcome != "succeeded" {
			continue
		}
		if !have || e.At.Before(firstAt) {
			firstAt = e.At
			firstFunding, _ = dataString(e.Data, "funding")
			have = true
		}
	}
	if !have {
		return 0
	}
	return boolToFloat(firstFunding == "prepaid")
}

// nameHasAt is 1 when any resource.created/resource.deleted event's RAW
// `name` field (not name_skeleton — see Features.NameHasAt) contains a
// literal "@".
func nameHasAt(events []event.Event) float64 {
	for _, e := range events {
		if e.Type != "resource.created" && e.Type != "resource.deleted" {
			continue
		}
		name, ok := dataString(e.Data, "name")
		if !ok {
			continue
		}
		if strings.Contains(name, "@") {
			return 1
		}
	}
	return 0
}

// firstDayDistinctDomains counts distinct (case-folded, S10 fix round)
// content.sent recipient_domain values within [firstSeenAt,
// firstSeenAt+window] inclusive on both ends — unlike the sliding
// *_1h/*_24h windows, this one is anchored to the subject's first event,
// not to "now": once past firstSeenAt+window, this feature is permanently
// fixed. Scaled by log1p (D2 round 3, replacing R1 round 2's hard cap of
// 10 — see firstDayDistinctDomainsLogScale) rather than a raw count: a
// day-1 receipts account fanning out to dozens of genuine, distinct
// customer domains is ordinary legitimate behaviour, not itself abuse
// evidence, but a hard cap made a 30-domain and a 300-domain account read
// identically, with zero volume sensitivity past it.
func firstDayDistinctDomains(events []event.Event, firstSeenAt time.Time, window time.Duration) float64 {
	cutoff := firstSeenAt.Add(window)
	domains := make(map[string]struct{})
	for _, e := range events {
		if e.Type != "content.sent" {
			continue
		}
		if e.At.Before(firstSeenAt) || e.At.After(cutoff) {
			continue
		}
		d, ok := dataString(e.Data, "recipient_domain")
		if !ok || d == "" {
			continue
		}
		domains[normalizeToken(d)] = struct{}{}
	}
	return firstDayDistinctDomainsLogScale * math.Log1p(float64(len(domains)))
}

// selfSendBeforeExternal counts content.sent events with
// recipient_is_own_identity true that occurred at or before the subject's
// first (earliest) content.sent event with recipient_is_own_identity
// false. All self-sends count when there is no external send yet.
// Saturates at selfSendBeforeExternalCap (R1 round 2, proven: a developer
// sending several test emails to their own inbox before ever emailing
// anyone else — ordinary integration testing, not rehearsal for a blast —
// pushed a benign account's score toward `high` on this feature alone
// once the count ran into the high single digits).
func selfSendBeforeExternal(events []event.Event) float64 {
	firstExternal, haveExternal := minAt(events, func(e event.Event) bool {
		if e.Type != "content.sent" {
			return false
		}
		own, ok := dataBool(e.Data, "recipient_is_own_identity")
		return ok && !own
	})

	var n int
	for _, e := range events {
		if e.Type != "content.sent" {
			continue
		}
		own, ok := dataBool(e.Data, "recipient_is_own_identity")
		if !ok || !own {
			continue
		}
		if haveExternal && e.At.After(firstExternal) {
			continue
		}
		n++
	}
	return saturate(n, selfSendBeforeExternalCap)
}

// burstRatio is the fraction of the subject's lifetime resource.created +
// content.sent activity that fell within the trailing window ending at
// now. 0 when there is no such activity at all (never a division by
// zero).
func burstRatio(events []event.Event, now time.Time, window time.Duration) float64 {
	var total, recent float64
	for _, e := range events {
		if e.Type != "resource.created" && e.Type != "content.sent" {
			continue
		}
		total++
		if withinWindow(e.At, now, window) {
			recent++
		}
	}
	if total == 0 {
		return 0
	}
	return recent / total
}

// nextRescoreAt is the earliest instant, strictly after now, at which an
// already-computed window feature would change even with no new event
// (design §4.8), coalesced to rescoreCoalesceInterval (S6 fix round). It
// considers:
//
//   - the *_1h window: the oldest still-in-window resource.created/
//     content.sent event's exit time (At + windows.OneHour) — the only two
//     event types any windowed feature reads (S6; see isWindowedEventType).
//   - the *_24h window, the same way, over windows.DayHour.
//   - FirstDayDistinctDomains' fixed firstSeenAt+windows.DayHour cutover,
//     while it is still in the future.
//   - the earliest FUTURE-dated event (At > now, e.g. a backfill or a
//     clock-skewed producer within design's ±24h allowance) — B4 fix
//     round, proven: a burst of events dated slightly ahead of the
//     worker's own clock was invisible to every window until a later,
//     unrelated event happened to bump dirty_seq; without this candidate
//     it would simply never be scored inside its own window.
//
// Returns the zero time.Time when none of the above is currently pending
// (e.g. the subject has no events within either window, and its first-day
// cutover has already passed) — "nothing to rescore for on a timer; only a
// new event will make this subject dirty again".
func nextRescoreAt(events []event.Event, now, firstSeenAt time.Time, windows Windows) time.Time {
	var candidates []time.Time
	if t, ok := earliestWindowExit(events, now, windows.OneHour); ok {
		candidates = append(candidates, t)
	}
	if t, ok := earliestWindowExit(events, now, windows.DayHour); ok {
		candidates = append(candidates, t)
	}
	if cutover := firstSeenAt.Add(windows.DayHour); cutover.After(now) {
		candidates = append(candidates, cutover)
	}
	// S2b: the young-account cutover (youngAccountWindow) is a second
	// passive-decay transition alongside the first-day cutover above — the
	// young-gated send-volume features (Sends10mMax, Sends1h,
	// WebmailSends1h, DistinctRecipients1h) fall to 0 once the subject
	// crosses it, even with zero new events, so a rescore must be
	// scheduled for that instant too or an established sender's score
	// would stay pinned at its last young-period value indefinitely.
	// Scheduled only when the subject has EVER recorded a content.sent
	// event (isWindowedEventType's own event-type scoping, applied here
	// too) — a subject with no send history at all has every one of
	// those features already at 0, so the transition changes nothing and
	// scheduling it anyway would be pure waste (the same reasoning
	// isWindowedEventType's own doc comment gives).
	if hasEventType(events, "content.sent") {
		if cutover := firstSeenAt.Add(youngAccountWindow); cutover.After(now) {
			candidates = append(candidates, cutover)
		}
	}
	if t, ok := minAt(events, func(e event.Event) bool { return e.At.After(now) }); ok {
		candidates = append(candidates, t)
	}
	if len(candidates) == 0 {
		return time.Time{}
	}
	min := candidates[0]
	for _, c := range candidates[1:] {
		if c.Before(min) {
			min = c
		}
	}
	return coalesce(min)
}

// coalesce rounds t UP to the next multiple of rescoreCoalesceInterval
// since the Unix epoch (S6 fix round), so nearby candidates from unrelated
// events — including ones computed on different Tick passes — land on the
// same scheduled instant and share one rescore instead of each getting its
// own.
func coalesce(t time.Time) time.Time {
	truncated := t.Truncate(rescoreCoalesceInterval)
	if truncated.Before(t) {
		return truncated.Add(rescoreCoalesceInterval)
	}
	return truncated
}

// earliestWindowExit returns the exit time (At + window) of the oldest
// windowed-event-type (resource.created/content.sent — S6 fix round) event
// still inside the window (now-window, now], i.e. the earliest instant at
// which the window would lose an event it currently counts.
func earliestWindowExit(events []event.Event, now time.Time, window time.Duration) (time.Time, bool) {
	oldest, ok := minAt(events, func(e event.Event) bool {
		return isWindowedEventType(e.Type) && withinWindow(e.At, now, window)
	})
	if !ok {
		return time.Time{}, false
	}
	exit := oldest.Add(window)
	if !exit.After(now) {
		// Can't happen given withinWindow's own contract (a kept event's
		// exit time At+window is always > now), but guards against ever
		// scheduling a "next" rescore that isn't actually in the future.
		return time.Time{}, false
	}
	return exit, true
}

// --- S2b: send volume, webmail, recipient hashing and subject-brand
// matching -------------------------------------------------------------
//
// This section computes the second wave of v0 features addressing
// common bulk-phishing shapes: a burst of recipients in a short window,
// a lure whose fan-out concentrates on consumer webmail providers rather
// than real customer domains, and a brand mentioned in the message
// SUBJECT rather than (or in addition to) the sending resource's own
// name.

// youngAccountWindow bounds every send-volume feature below to a
// subject's first 7 days (B1 fix round, proven: an established, months-
// old paid sender can legitimately burst hundreds of recipients in a
// single send — volume alone must never read the same for that account
// as it does for a signup that started blasting on day zero). Sends1h,
// Sends10mMax, WebmailSends1h and DistinctRecipients1h are all
// multiplied by youngAccountFactor; SendsFirstDay needs no such gate — it
// is already permanently anchored to the subject's first day, the same
// way FirstDayDistinctDomains is, so it can never reflect an established
// account's CURRENT behaviour in the first place.
const youngAccountWindow = 7 * 24 * time.Hour

// youngAccountFactor is 1 while now is within youngAccountWindow of
// firstSeenAt, else 0 — a hard gate, not a gradual decay: once an
// account ages out of its first week, every send-volume feature it gates
// reads 0 from then on, regardless of how much mail it sends. This is
// what makes those features DECAY rather than remain a permanent,
// never-reconsidered fact the way a lifetime maximum would (nextRescoreAt
// schedules the exact instant this flips, so the transition happens even
// with no new event).
func youngAccountFactor(firstSeenAt, now time.Time) float64 {
	if now.Sub(firstSeenAt) <= youngAccountWindow {
		return 1
	}
	return 0
}

// sendsVolumeCap bounds every send-volume/recipient-count feature below
// (Sends10mMax, Sends1h, SendsFirstDay, DistinctRecipients1h) so a single
// pathological event or account can't swamp the local scorer's linear
// model through raw magnitude alone — matching NameBrandMatch/
// SubjectBrandMatch's own capped spirit. A plain cap (rather than
// first_day_distinct_domains' log1p curve) is the better fit here: log1p
// is already a substantial fraction of its own eventual ceiling at very
// SMALL n, which would give an ordinary handful-of-recipients send nearly
// as much weight, proportionally, as a genuine mass blast — backwards for
// a feature whose whole point is separating "a few" from "a lot". A cap
// keeps the raw count intact up to a ceiling comfortably above any volume
// a v0 fixture exercises, only bounding the pathological case.
const sendsVolumeCap = 300

// subjectBrandMatchCap bounds Features.SubjectBrandMatch — a lure
// template mentioning many different brands isn't linearly worse past a
// point, the same reasoning NameBrandMatch's own flat (0/1) contribution
// already reflects; an unbounded count would let a template that test-
// mails every brand in the list swamp the model through this feature
// alone.
const subjectBrandMatchCap = 3

// capAt caps v at max (v itself if v <= max).
func capAt(v, max float64) float64 {
	if v > max {
		return max
	}
	return v
}

// recipientCountOf reads a content.sent event's recipient_count, falling
// back to 1 (a single recipient) when the field is absent — the
// redaction schema (S6/N6 fix rounds) already rejects a present-but-
// invalid recipient_count (non-positive, non-integer, or paired with a
// recipient_hash while > 1) before an event ever reaches feature
// extraction, but Extract has no way to know an event actually went
// through Redact (a test building event.Event by hand, or a future
// caller feeding it raw), so a defensive fallback to 1 covers that case
// the same way dataString/dataBool/dataNumber already degrade to
// "absent" on a type mismatch rather than trusting the caller. Capped at
// sendsVolumeCap (S7 fix round: "cap per-event recipient_count in every
// sum" — a single event's declared count must not by itself dominate
// every sum that reads it).
func recipientCountOf(e event.Event) float64 {
	n, ok := dataNumber(e.Data, "recipient_count")
	if !ok || n <= 0 || n != math.Trunc(n) {
		return 1
	}
	return capAt(n, sendsVolumeCap)
}

// sendsInWindow sums recipientCountOf across content.sent events falling
// within the half-open window (now-window, now] — withinWindow's own
// convention, which already excludes a future-dated event (N5 fix
// round: "future-dated events are not counted").
func sendsInWindow(events []event.Event, now time.Time, window time.Duration) float64 {
	var sum float64
	for _, e := range events {
		if e.Type != "content.sent" || !withinWindow(e.At, now, window) {
			continue
		}
		sum += recipientCountOf(e)
	}
	return sum
}

// sends1h is Features.Sends1h: sendsInWindow over the trailing window
// ending at now, capped at sendsVolumeCap and gated by
// youngAccountFactor (B1 fix round) — decays exactly like
// resourceCount(events, "", now, window) as the window slides forward
// with no new event; content.sent is already a windowed event type (see
// isWindowedEventType), so no rescore-scheduling change is needed for
// that part of the decay.
func sends1h(events []event.Event, now, firstSeenAt time.Time, window time.Duration) float64 {
	return capAt(sendsInWindow(events, now, window), sendsVolumeCap) * youngAccountFactor(firstSeenAt, now)
}

// sendsFirstDay is Features.SendsFirstDay: the sum of content.sent
// recipient_count within [firstSeenAt, firstSeenAt+window] inclusive on
// both ends, capped at sendsVolumeCap — anchored to the subject's first
// event exactly like firstDayDistinctDomains, not to "now": once past
// firstSeenAt+window, this feature is permanently fixed, and
// nextRescoreAt's existing first-day-cutover candidate (feature-agnostic)
// already covers its one transition with no code change. Deliberately
// NOT gated by youngAccountFactor — see youngAccountWindow's own doc
// comment for why this feature needs no such gate at all.
func sendsFirstDay(events []event.Event, firstSeenAt, now time.Time, window time.Duration) float64 {
	cutoff := firstSeenAt.Add(window)
	var sum float64
	for _, e := range events {
		if e.Type != "content.sent" {
			continue
		}
		if e.At.Before(firstSeenAt) || e.At.After(cutoff) || e.At.After(now) {
			continue
		}
		sum += recipientCountOf(e)
	}
	return capAt(sum, sendsVolumeCap)
}

// sends10mMaxWindow is the fixed window sends10mMax searches for its
// largest recipient-count sum.
const sends10mMaxWindow = 10 * time.Minute

// sends10mMax is Features.Sends10mMax: the LARGEST sum of content.sent
// recipient_count within any sends10mMaxWindow-wide window across the
// subject's history up to now, capped at sendsVolumeCap and gated by
// youngAccountFactor (B1 fix round: an earlier, lifetime-unbounded
// version of this search never re-considered account age at all, so an
// established sender's routine burst read exactly like a new signup's —
// see youngAccountWindow's own doc comment for why gating, not merely a
// trailing window, is the actual fix). Future-dated events (N5 fix
// round) are excluded from the search entirely. Computed order-
// independently (a standard two-pointer sliding-window-sum maximum over
// events sorted by At) so out-of-order delivery can never miss the true
// maximum the way a single forward pass over delivery order could.
func sends10mMax(events []event.Event, now, firstSeenAt time.Time) float64 {
	type point struct {
		at time.Time
		n  float64
	}
	var pts []point
	for _, e := range events {
		if e.Type != "content.sent" || e.At.After(now) {
			continue
		}
		pts = append(pts, point{e.At, recipientCountOf(e)})
	}
	if len(pts) == 0 {
		return 0
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i].at.Before(pts[j].at) })

	var maxSum, sum float64
	left := 0
	for right := range pts {
		sum += pts[right].n
		for left < right && !pts[left].at.After(pts[right].at.Add(-sends10mMaxWindow)) {
			sum -= pts[left].n
			left++
		}
		if sum > maxSum {
			maxSum = sum
		}
	}
	return capAt(maxSum, sendsVolumeCap) * youngAccountFactor(firstSeenAt, now)
}

// distinctRecipients1h is Features.DistinctRecipients1h: the count of
// distinct content.sent recipient_hash values within the trailing window
// ending at now, falling back to ADDING recipient_count (not counting the
// event as a single recipient) for any event with no recipient_hash at
// all — an event with no hash gives no way to tell its recipients apart,
// so treating it as "recipient_count more distinct recipients" is closer
// to the truth than either dropping it or counting it as exactly one.
// Capped at sendsVolumeCap and gated by youngAccountFactor for the same
// reason as the send-volume features above.
func distinctRecipients1h(events []event.Event, now, firstSeenAt time.Time, window time.Duration) float64 {
	seen := make(map[string]struct{})
	var fallback float64
	for _, e := range events {
		if e.Type != "content.sent" || !withinWindow(e.At, now, window) {
			continue
		}
		if h, ok := dataString(e.Data, "recipient_hash"); ok && h != "" {
			seen[h] = struct{}{}
			continue
		}
		fallback += recipientCountOf(e)
	}
	return capAt(float64(len(seen))+fallback, sendsVolumeCap) * youngAccountFactor(firstSeenAt, now)
}

// webmailRecipientShare is Features.WebmailRecipientShare: the LIFETIME
// share (0..1) of sent recipients whose recipient_domain is on webmail's
// loaded list — a permanent fact, not a decaying window (unlike the
// *_1h features above, and deliberately not youngAccountFactor-gated: it
// measures WHO an account emails, not how much, and that ratio is
// informative regardless of account age). 0 when the subject has sent
// nothing at all (never a division by zero). Future-dated events (N5 fix
// round) are excluded from both the numerator and denominator.
func webmailRecipientShare(events []event.Event, now time.Time, webmail WebmailSet) float64 {
	var total, webmailSum float64
	for _, e := range events {
		if e.Type != "content.sent" || e.At.After(now) {
			continue
		}
		n := recipientCountOf(e)
		total += n
		if d, ok := dataString(e.Data, "recipient_domain"); ok && webmail.Contains(d) {
			webmailSum += n
		}
	}
	if total == 0 {
		return 0
	}
	return webmailSum / total
}

// webmailSends1h is Features.WebmailSends1h: the sum of content.sent
// recipient_count within the trailing window ending at now, restricted to
// events whose recipient_domain is on webmail's loaded list, capped at
// sendsVolumeCap and gated by youngAccountFactor. Computed DIRECTLY (S7
// fix round) rather than as webmailRecipientShare(...) * sends1h(...): the
// share is a LIFETIME ratio and sends1h is a TRAILING sum, so multiplying
// the two conflates two different timescales and produces a number that
// tracks neither one correctly (an account whose lifetime share is high
// but whose recent hour was entirely non-webmail would still report a
// large "webmail sends" value, and vice versa). Scanning the window
// directly for webmail-domain recipients has no such mismatch.
func webmailSends1h(events []event.Event, now, firstSeenAt time.Time, window time.Duration, webmail WebmailSet) float64 {
	var sum float64
	for _, e := range events {
		if e.Type != "content.sent" || !withinWindow(e.At, now, window) {
			continue
		}
		d, ok := dataString(e.Data, "recipient_domain")
		if !ok || !webmail.Contains(d) {
			continue
		}
		sum += recipientCountOf(e)
	}
	return capAt(sum, sendsVolumeCap) * youngAccountFactor(firstSeenAt, now)
}

// namedBrandNames returns the set of distinct curated brand names matched
// across every resource.created/resource.deleted event's raw `name`
// field — the same evidence nameBrandMatch reduces to a single 0/1, kept
// here as a set so subjectBrandMatch can exclude a brand already counted
// there (S2b's S2 fix round: "do not double-count the same brand across
// name_brand_match and subject_brand_match"). Matched on the RAW name,
// not the precomputed name_skeleton — see nameBrandMatch's own doc
// comment for why.
func namedBrandNames(events []event.Event, brands BrandSet) map[string]struct{} {
	var out map[string]struct{}
	for _, e := range events {
		if e.Type != "resource.created" && e.Type != "resource.deleted" {
			continue
		}
		name, ok := dataString(e.Data, "name")
		if !ok {
			continue
		}
		for n := range brands.MatchedBrandNames(name) {
			if out == nil {
				out = make(map[string]struct{})
			}
			out[n] = struct{}{}
		}
	}
	return out
}

// accountHasIntegrationName reports whether any resource.created/
// resource.deleted event's raw `name` field carries an integration token
// (S2b's S1 fix round) — the SENDING ACCOUNT's own onboarding evidence
// that subjectBrandMatch uses to decide whether to exempt every subject
// line outright, instead of re-litigating "tracking"/"api"-style words
// found inside each individual subject.
func accountHasIntegrationName(events []event.Event) bool {
	for _, e := range events {
		if e.Type != "resource.created" && e.Type != "resource.deleted" {
			continue
		}
		name, ok := dataString(e.Data, "name")
		if !ok {
			continue
		}
		if hasIntegrationToken(tokenize(name)) {
			return true
		}
	}
	return false
}

// subjectBrandMatch is Features.SubjectBrandMatch: the count of DISTINCT
// curated brands (BrandSet.MatchedBrandNamesForSubject — S1 fix round)
// matched across every content.sent subject_line within the trailing
// window ending at now, EXCLUDING any brand already counted by
// namedBrandNames (S2 fix round: caps the combined per-brand
// contribution of name_brand_match and subject_brand_match — a brand
// already credited via the resource/agent name never ALSO inflates this
// count), capped at subjectBrandMatchCap. content.sent is already a
// windowed event type, so this decaying window's rescore scheduling is
// already covered with no code change. Future-dated events are excluded
// by withinWindow.
func subjectBrandMatch(events []event.Event, now time.Time, window time.Duration, brands BrandSet, alreadyNamed map[string]struct{}, accountHasIntegrationName bool) float64 {
	matched := make(map[string]struct{})
	for _, e := range events {
		if e.Type != "content.sent" || !withinWindow(e.At, now, window) {
			continue
		}
		subj, ok := dataString(e.Data, "subject_line")
		if !ok || subj == "" {
			continue
		}
		for name := range brands.MatchedBrandNamesForSubject(subj, accountHasIntegrationName) {
			if _, already := alreadyNamed[name]; already {
				continue
			}
			matched[name] = struct{}{}
		}
	}
	return saturate(len(matched), subjectBrandMatchCap)
}
