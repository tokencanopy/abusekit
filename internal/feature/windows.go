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

// normalizeResourceKind [S2b] canonicalises resource.created/deleted's
// producer-supplied, free-text `kind` field per the design's [S2b]
// producer-contract amendment (docs/design's §4.5 note): case/whitespace-
// insensitive (S10's normalizeToken, already true), and folding the
// aliases "key"/"api_key"/"apikey"/"api-key" to resourceKindKey ("key") and
// "agent"/"mailbox"/"inbox" to "agent" — proven necessary, a real
// producer's own "api_key" spelling was never recognized as a key at all,
// so a burst of key creation read as ordinary (uncounted) resource
// activity instead of the key-specific signal it actually was. A kind
// outside both alias groups passes through normalizeToken's folding
// unchanged (still case/whitespace-insensitive), never rejected — the
// vocabulary here is a documented convention producers SHOULD follow, not
// something ingest itself enforces (design §4.12).
func normalizeResourceKind(raw string) string {
	switch normalizeToken(raw) {
	case "key", "api_key", "apikey", "api-key":
		return "key"
	case "agent", "mailbox", "inbox":
		return "agent"
	default:
		return normalizeToken(raw)
	}
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

// nameBrandMatch is 1 when any resource.created/resource.deleted event's
// raw `name` field matches brands (S3 fix round: word/token-boundary-aware,
// never a bare substring check — see BrandSet). Matched on the RAW name,
// not the precomputed name_skeleton: BrandSet.Matches folds through
// event.Skeleton itself, and doing that twice (once to produce
// name_skeleton at ingest, once here) is not guaranteed idempotent for the
// digit-adjacency-dependent leet folding.
func nameBrandMatch(events []event.Event, brands BrandSet) float64 {
	for _, e := range events {
		if e.Type != "resource.created" && e.Type != "resource.deleted" {
			continue
		}
		name, ok := dataString(e.Data, "name")
		if !ok {
			continue
		}
		if brands.Matches(name) {
			return 1
		}
	}
	return 0
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

// subjectBrandMatchCap [S2b] bounds Features.SubjectBrandMatch (design's
// own "capped at 3" — matching NameBrandMatch's spirit that a lure
// mentioning many different brands isn't linearly worse past a point, and
// an unbounded count would let a template test-mailing every brand in the
// list swamp the model through this feature alone).
const subjectBrandMatchCap = 3

// recipientCountOf [S2b] reads a content.sent event's recipient_count,
// falling back to 1 (a single recipient) when the field is absent or not a
// positive number — design's [S2b] amendment: "recipient_count summed,
// falling back to 1". Every send-volume feature below (Sends10mMax,
// Sends1h, SendsFirstDay, WebmailRecipientShare) reads recipient counts
// through this one function so the fallback rule can't drift between them.
func recipientCountOf(e event.Event) float64 {
	n, ok := dataNumber(e.Data, "recipient_count")
	if !ok || n <= 0 {
		return 1
	}
	return n
}

// sendsInWindow [S2b] sums recipientCountOf across content.sent events
// falling within the half-open window (now-window, now] — withinWindow's
// own convention — used by both Sends1h (a real decaying window) and
// DistinctRecipients1h's no-hash fallback.
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

// sends1h [S2b] is Features.Sends1h: log1p of sendsInWindow over the
// trailing window ending at now — decays exactly like
// resourceCount(events, "", now, window) as the window slides forward
// with no new event; content.sent is already a windowed event type (see
// isWindowedEventType), so no rescore-scheduling change is needed for the
// decay this introduces.
func sends1h(events []event.Event, now time.Time, window time.Duration) float64 {
	return math.Log1p(sendsInWindow(events, now, window))
}

// sendsFirstDay [S2b] is Features.SendsFirstDay: log1p of the sum of
// content.sent recipient_count within [firstSeenAt, firstSeenAt+window]
// inclusive on both ends — anchored to the subject's first event exactly
// like firstDayDistinctDomains, not to "now": once past firstSeenAt+window,
// this feature is permanently fixed, and nextRescoreAt's existing
// first-day-cutover candidate (feature-agnostic) already covers its one
// transition with no code change.
func sendsFirstDay(events []event.Event, firstSeenAt time.Time, window time.Duration) float64 {
	cutoff := firstSeenAt.Add(window)
	var sum float64
	for _, e := range events {
		if e.Type != "content.sent" {
			continue
		}
		if e.At.Before(firstSeenAt) || e.At.After(cutoff) {
			continue
		}
		sum += recipientCountOf(e)
	}
	return math.Log1p(sum)
}

// sends10mMax [S2b] is Features.Sends10mMax: log1p of the LARGEST sum of
// content.sent recipient_count within any window-duration-wide window
// across the subject's whole history — computed order-independently (a
// standard two-pointer sliding-window-sum maximum over events sorted by
// At) so out-of-order delivery can never miss the true maximum the way a
// single forward pass over delivery order could. Unlike sends1h/
// sendsFirstDay above, this is a MAXIMUM over already-elapsed windows: it
// can only grow as a new event arrives, never shrink as time passes with
// no new event, so it needs no window-exit rescore of its own (see its
// Features field doc comment).
func sends10mMax(events []event.Event, window time.Duration) float64 {
	type point struct {
		at time.Time
		n  float64
	}
	var pts []point
	for _, e := range events {
		if e.Type != "content.sent" {
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
		for left < right && !pts[left].at.After(pts[right].at.Add(-window)) {
			sum -= pts[left].n
			left++
		}
		if sum > maxSum {
			maxSum = sum
		}
	}
	return math.Log1p(maxSum)
}

// distinctRecipients1h [S2b] is Features.DistinctRecipients1h: log1p of
// the count of distinct content.sent recipient_hash values within the
// trailing window ending at now, falling back to ADDING recipient_count
// (not counting the event as a single recipient) for any event with no
// recipient_hash at all — an event with no hash gives no way to tell its
// recipients apart, so treating it as "recipient_count more distinct
// recipients" is closer to the truth than either dropping it or counting
// it as exactly one.
func distinctRecipients1h(events []event.Event, now time.Time, window time.Duration) float64 {
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
	return math.Log1p(float64(len(seen)) + fallback)
}

// webmailRecipientShare [S2b] is Features.WebmailRecipientShare: the
// LIFETIME share (0..1) of sent recipients whose recipient_domain is on
// webmail's loaded list — a permanent fact, not a decaying window (see its
// Features field doc comment). 0 when the subject has sent nothing at all
// (never a division by zero).
func webmailRecipientShare(events []event.Event, webmail WebmailSet) float64 {
	var total, webmailSum float64
	for _, e := range events {
		if e.Type != "content.sent" {
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

// subjectBrandMatch [S2b] is Features.SubjectBrandMatch: the count of
// DISTINCT curated brands (BrandSet.MatchedBrandNames — same
// canonicalisation and integration-token gate as NameBrandMatch) matched
// across every content.sent subject_line within the trailing window ending
// at now, capped at subjectBrandMatchCap. content.sent is already a
// windowed event type, so this decaying window's rescore scheduling is
// already covered with no code change.
func subjectBrandMatch(events []event.Event, now time.Time, window time.Duration, brands BrandSet) float64 {
	matched := make(map[string]struct{})
	for _, e := range events {
		if e.Type != "content.sent" || !withinWindow(e.At, now, window) {
			continue
		}
		subj, ok := dataString(e.Data, "subject_line")
		if !ok || subj == "" {
			continue
		}
		for name := range brands.MatchedBrandNames(subj) {
			matched[name] = struct{}{}
		}
	}
	return saturate(len(matched), subjectBrandMatchCap)
}
