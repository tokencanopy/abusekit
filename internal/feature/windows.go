package feature

import (
	"strings"
	"time"

	"github.com/tokencanopy/abusekit/internal/event"
)

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
// age).
func subjectAgeHours(firstSeenAt, now time.Time) float64 {
	h := now.Sub(firstSeenAt).Hours()
	if h < 0 {
		return 0
	}
	return h
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
			if !ok || k != kind {
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

// upgradeDelayMinutes is minutes between firstSeenAt and the subject's
// first (earliest) subscription.changed event; see
// Features.UpgradeDelayMin's doc comment for the "no upgrade yet"
// fallback.
func upgradeDelayMinutes(events []event.Event, firstSeenAt, now time.Time) float64 {
	if at, ok := minAt(events, func(e event.Event) bool { return e.Type == "subscription.changed" }); ok {
		return at.Sub(firstSeenAt).Minutes()
	}
	return now.Sub(firstSeenAt).Minutes()
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
// name_skeleton contains a curated brand name (see brandNames).
func nameBrandMatch(events []event.Event) float64 {
	for _, e := range events {
		if e.Type != "resource.created" && e.Type != "resource.deleted" {
			continue
		}
		skeleton, ok := dataString(e.Data, "name_skeleton")
		if !ok {
			continue
		}
		if matchesBrand(skeleton) {
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

// firstDayDistinctDomains counts distinct content.sent recipient_domain
// values within [firstSeenAt, firstSeenAt+window] inclusive on both ends —
// unlike the sliding *_1h/*_24h windows, this one is anchored to the
// subject's first event, not to "now": once past firstSeenAt+window, this
// feature is permanently fixed.
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
		domains[d] = struct{}{}
	}
	return float64(len(domains))
}

// selfSendBeforeExternal counts content.sent events with
// recipient_is_own_identity true that occurred at or before the subject's
// first (earliest) content.sent event with recipient_is_own_identity
// false. All self-sends count when there is no external send yet.
func selfSendBeforeExternal(events []event.Event) float64 {
	firstExternal, haveExternal := minAt(events, func(e event.Event) bool {
		if e.Type != "content.sent" {
			return false
		}
		own, ok := dataBool(e.Data, "recipient_is_own_identity")
		return ok && !own
	})

	var n float64
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
	return n
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
// (design §4.8). It considers three kinds of decay:
//
//   - the *_1h window: the oldest still-in-window event's exit time
//     (At + windows.OneHour) — since events are oldest-first, the FIRST
//     event found still inside the window is the one that exits soonest.
//   - the *_24h window, the same way, over windows.DayHour.
//   - FirstDayDistinctDomains' fixed firstSeenAt+windows.DayHour cutover,
//     while it is still in the future.
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
	if len(candidates) == 0 {
		return time.Time{}
	}
	min := candidates[0]
	for _, c := range candidates[1:] {
		if c.Before(min) {
			min = c
		}
	}
	return min
}

// earliestWindowExit returns the exit time (At + window) of the oldest
// event of ANY type still inside the window (now-window, now], i.e. the
// earliest instant at which the window would lose an event it currently
// counts. Every event type is considered here, not just the ones
// resourceCount/burstRatio actually window (resource.created,
// content.sent): a payment.attempt or subscription.changed event doesn't
// feed a *_1h/*_24h feature today, but a future feature that windows a new
// event type should not need a change here to be scheduled correctly —
// this is deliberately "when does ANYTHING currently in the window age
// out", a conservative superset of what v0's own windowed features need.
func earliestWindowExit(events []event.Event, now time.Time, window time.Duration) (time.Time, bool) {
	oldest, ok := minAt(events, func(e event.Event) bool { return withinWindow(e.At, now, window) })
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
