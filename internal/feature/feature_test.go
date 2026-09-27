package feature

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tokencanopy/abusekit/internal/event"
)

// base is a fixed instant every test builds its fictional timeline from
// (2031, matching the repo's public-data-boundary convention of
// fictional-year fixture timestamps).
var base = time.Date(2031, time.January, 1, 0, 0, 0, 0, time.UTC)

func at(offset time.Duration) time.Time { return base.Add(offset) }

func ev(id, typ string, offset time.Duration, data map[string]any) event.Event {
	return event.Event{ID: id, Subject: "acct_test", Type: typ, At: at(offset), Data: data}
}

func defaultWindows(offset time.Duration) Windows {
	return DefaultWindows(at(offset))
}

// fakeNeighbors is a canned Neighbors for tests that don't want a real
// store; Err, when set, makes Evidence fail so Extract's error-wrapping
// path is exercised too.
type fakeNeighbors struct {
	evidence NeighborEvidence
	err      error
	calls    int
}

func (f *fakeNeighbors) Evidence(context.Context, string, string) (NeighborEvidence, error) {
	f.calls++
	return f.evidence, f.err
}

func TestExtract_EmptyHistory(t *testing.T) {
	res, err := Extract(context.Background(), "e2a", "acct_test", nil, nil, defaultWindows(0))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res != (Result{}) {
		t.Errorf("empty history: got %+v, want the zero Result", res)
	}
}

func TestExtract_RequiresWindowsNow(t *testing.T) {
	events := []event.Event{ev("e1", "subject.created", 0, nil)}
	_, err := Extract(context.Background(), "e2a", "acct_test", events, nil, Windows{})
	if err == nil {
		t.Fatalf("expected an error when windows.Now is zero")
	}
}

func TestExtract_RequiresPositiveWindowDurations(t *testing.T) {
	events := []event.Event{ev("e1", "subject.created", 0, nil)}
	_, err := Extract(context.Background(), "e2a", "acct_test", events, nil, Windows{Now: at(time.Hour)})
	if err == nil {
		t.Fatalf("expected an error when OneHour/DayHour are unset")
	}
}

func TestExtract_NilNeighborsTreatedAsNoNeighbors(t *testing.T) {
	events := []event.Event{ev("e1", "subject.created", 0, nil)}
	res, err := Extract(context.Background(), "e2a", "acct_test", events, nil, defaultWindows(time.Hour))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.Features.LinkedDeletedN != 0 || res.Features.LinkedLabelledAbusiveN != 0 || res.Features.FingerprintSeenOnOtherSubjects != 0 {
		t.Errorf("nil Neighbors: got non-zero linked_* features: %+v", res.Features)
	}
}

func TestExtract_NeighborEvidenceError(t *testing.T) {
	events := []event.Event{ev("e1", "subject.created", 0, nil)}
	fake := &fakeNeighbors{err: errors.New("boom")}
	_, err := Extract(context.Background(), "e2a", "acct_test", events, fake, defaultWindows(time.Hour))
	if err == nil {
		t.Fatalf("expected Extract to propagate a Neighbors.Evidence error")
	}
}

func TestExtract_LinkedFeaturesFromNeighbors(t *testing.T) {
	events := []event.Event{ev("e1", "subject.created", 0, nil)}
	fake := &fakeNeighbors{evidence: NeighborEvidence{DeletedCount: 2, LabelledAbusiveCount: 3, FingerprintShared: true}}
	res, err := Extract(context.Background(), "e2a", "acct_test", events, fake, defaultWindows(time.Hour))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if fake.calls != 1 {
		t.Errorf("expected Neighbors.Evidence called exactly once, got %d", fake.calls)
	}
	f := res.Features
	if f.LinkedDeletedN != 2 || f.LinkedLabelledAbusiveN != 3 || f.FingerprintSeenOnOtherSubjects != 1 {
		t.Errorf("linked_* features = %+v, want {2,3,1}", f)
	}
}

func TestFeatures_MapMatchesNames(t *testing.T) {
	m := Features{}.Map()
	if len(m) != len(Names) {
		t.Fatalf("Map has %d keys, Names has %d", len(m), len(Names))
	}
	for _, n := range Names {
		if _, ok := m[n]; !ok {
			t.Errorf("Names contains %q but Map doesn't produce it", n)
		}
	}
}

func TestSubjectAgeHours(t *testing.T) {
	tests := []struct {
		name        string
		firstSeenAt time.Time
		now         time.Time
		want        float64
	}{
		{"zero age", base, base, 0},
		{"one hour", base, base.Add(time.Hour), 1},
		{"ninety minutes", base, base.Add(90 * time.Minute), 1.5},
		{"clock skew: event slightly in the future clamps to 0", base.Add(time.Minute), base, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := subjectAgeHours(tt.firstSeenAt, tt.now)
			if got != tt.want {
				t.Errorf("subjectAgeHours(%v, %v) = %v, want %v", tt.firstSeenAt, tt.now, got, tt.want)
			}
		})
	}
}

func TestWithinWindow_Boundaries(t *testing.T) {
	now := at(time.Hour)
	window := time.Hour
	tests := []struct {
		name string
		at   time.Time
		want bool
	}{
		{"exactly window-old: excluded (just expired)", now.Add(-window), false},
		{"one nanosecond inside the window", now.Add(-window).Add(time.Nanosecond), true},
		{"exactly at now: included", now, true},
		{"one nanosecond after now (future/skew): excluded", now.Add(time.Nanosecond), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := withinWindow(tt.at, now, window); got != tt.want {
				t.Errorf("withinWindow(%v, %v, %v) = %v, want %v", tt.at, now, window, got, tt.want)
			}
		})
	}
}

func TestResourceCount_VelocityAndTotal(t *testing.T) {
	events := []event.Event{
		ev("r1", "resource.created", 0, map[string]any{"kind": "agent"}),
		ev("r2", "resource.created", 30*time.Minute, map[string]any{"kind": "key"}),
		ev("r3", "resource.created", 2*time.Hour, map[string]any{"kind": "agent"}), // outside the 1h window at now=2h30m
		ev("d1", "resource.deleted", 10*time.Minute, map[string]any{"kind": "agent"}),
	}
	now := at(2*time.Hour + 30*time.Minute)

	if got := resourceCount(events, "", now, time.Hour); got != 1 {
		t.Errorf("resource_velocity_1h = %v, want 1 (only r3 is within the last hour)", got)
	}
	if got := resourceCount(events, "", now, 0); got != 3 {
		t.Errorf("resource_total = %v, want 3 (resource.created only, not resource.deleted)", got)
	}
	if got := resourceCount(events, "key", now, 0); got != 1 {
		t.Errorf("key_total = %v, want 1", got)
	}
	if got := resourceCount(events, "key", now, time.Hour); got != 0 {
		t.Errorf("key_velocity_1h = %v, want 0 (the one key is 2h old)", got)
	}
}

func TestUpgradeDelayMinutes(t *testing.T) {
	t.Run("upgrade recorded", func(t *testing.T) {
		events := []event.Event{
			ev("s1", "subject.created", 0, nil),
			ev("u1", "subscription.changed", 16*time.Minute, map[string]any{"plan": "scale"}),
		}
		got := upgradeDelayMinutes(events, base, at(time.Hour))
		if got != 16 {
			t.Errorf("upgrade_delay_min = %v, want 16", got)
		}
	})
	t.Run("no upgrade yet: falls back to minutes elapsed so far", func(t *testing.T) {
		events := []event.Event{ev("s1", "subject.created", 0, nil)}
		got := upgradeDelayMinutes(events, base, at(45*time.Minute))
		if got != 45 {
			t.Errorf("upgrade_delay_min = %v, want 45", got)
		}
	})
	t.Run("out-of-order delivery: earliest subscription.changed wins regardless of slice order", func(t *testing.T) {
		events := []event.Event{
			ev("u2", "subscription.changed", 30*time.Minute, nil),
			ev("u1", "subscription.changed", 5*time.Minute, nil), // chronologically earlier, delivered second
		}
		got := upgradeDelayMinutes(events, base, at(time.Hour))
		if got != 5 {
			t.Errorf("upgrade_delay_min = %v, want 5 (the earliest event, even though it's later in the slice)", got)
		}
	})
}

func TestDeclinesBeforeFirstSuccess(t *testing.T) {
	t.Run("counts declines strictly before the first success", func(t *testing.T) {
		events := []event.Event{
			ev("p1", "payment.attempt", 0, map[string]any{"outcome": "declined"}),
			ev("p2", "payment.attempt", 10*time.Second, map[string]any{"outcome": "declined"}),
			ev("p3", "payment.attempt", 20*time.Second, map[string]any{"outcome": "succeeded"}),
			ev("p4", "payment.attempt", 30*time.Second, map[string]any{"outcome": "declined"}), // after success: not counted
		}
		if got := declinesBeforeFirstSuccess(events); got != 2 {
			t.Errorf("declines_before_first_success = %v, want 2", got)
		}
	})
	t.Run("no success yet: counts every decline so far", func(t *testing.T) {
		events := []event.Event{
			ev("p1", "payment.attempt", 0, map[string]any{"outcome": "declined"}),
			ev("p2", "payment.attempt", 10*time.Second, map[string]any{"outcome": "declined"}),
			ev("p3", "payment.attempt", 20*time.Second, map[string]any{"outcome": "blocked"}),
		}
		if got := declinesBeforeFirstSuccess(events); got != 2 {
			t.Errorf("declines_before_first_success = %v, want 2", got)
		}
	})
	t.Run("out-of-order delivery: a decline delivered after success but timestamped before it still counts", func(t *testing.T) {
		events := []event.Event{
			ev("p2", "payment.attempt", 20*time.Second, map[string]any{"outcome": "succeeded"}),
			ev("p1", "payment.attempt", 10*time.Second, map[string]any{"outcome": "declined"}), // earlier At, delivered second
		}
		if got := declinesBeforeFirstSuccess(events); got != 1 {
			t.Errorf("declines_before_first_success = %v, want 1", got)
		}
	})
}

func TestFirstFundingPrepaid(t *testing.T) {
	t.Run("first success is prepaid", func(t *testing.T) {
		events := []event.Event{
			ev("p1", "payment.attempt", 0, map[string]any{"outcome": "declined", "funding": "credit"}),
			ev("p2", "payment.attempt", 10*time.Second, map[string]any{"outcome": "succeeded", "funding": "prepaid"}),
		}
		if got := firstFundingPrepaid(events); got != 1 {
			t.Errorf("first_funding_prepaid = %v, want 1", got)
		}
	})
	t.Run("first success is not prepaid", func(t *testing.T) {
		events := []event.Event{
			ev("p1", "payment.attempt", 0, map[string]any{"outcome": "succeeded", "funding": "credit"}),
		}
		if got := firstFundingPrepaid(events); got != 0 {
			t.Errorf("first_funding_prepaid = %v, want 0", got)
		}
	})
	t.Run("no success yet", func(t *testing.T) {
		events := []event.Event{ev("p1", "payment.attempt", 0, map[string]any{"outcome": "declined", "funding": "prepaid"})}
		if got := firstFundingPrepaid(events); got != 0 {
			t.Errorf("first_funding_prepaid = %v, want 0", got)
		}
	})
	t.Run("out-of-order delivery: chronologically-first success decides, not delivery order", func(t *testing.T) {
		events := []event.Event{
			ev("p2", "payment.attempt", 20*time.Second, map[string]any{"outcome": "succeeded", "funding": "credit"}),
			ev("p1", "payment.attempt", 10*time.Second, map[string]any{"outcome": "succeeded", "funding": "prepaid"}),
		}
		if got := firstFundingPrepaid(events); got != 1 {
			t.Errorf("first_funding_prepaid = %v, want 1 (the earlier success, funding=prepaid)", got)
		}
	})
}

func TestNameBrandMatchAndHasAt(t *testing.T) {
	t.Run("brand match via name_skeleton", func(t *testing.T) {
		events := []event.Event{ev("r1", "resource.created", 0, map[string]any{"name": "PayPal Support", "name_skeleton": "paypal support"})}
		if got := nameBrandMatch(events); got != 1 {
			t.Errorf("name_brand_match = %v, want 1", got)
		}
	})
	t.Run("no brand match", func(t *testing.T) {
		events := []event.Event{ev("r1", "resource.created", 0, map[string]any{"name": "Notifications Agent", "name_skeleton": "notifications agent"})}
		if got := nameBrandMatch(events); got != 0 {
			t.Errorf("name_brand_match = %v, want 0", got)
		}
	})
	t.Run("has_at checks the RAW name, not the skeleton", func(t *testing.T) {
		// '@' folds to 'a' in Skeleton (event.Skeleton's leetspeak table), so
		// name_has_at must read the raw "name" field, never "name_skeleton".
		events := []event.Event{ev("r1", "resource.created", 0, map[string]any{"name": "support@agent", "name_skeleton": "supportaagent"})}
		if got := nameHasAt(events); got != 1 {
			t.Errorf("name_has_at = %v, want 1", got)
		}
	})
	t.Run("resource.deleted also counts", func(t *testing.T) {
		events := []event.Event{ev("r1", "resource.deleted", 0, map[string]any{"name": "amazon-bot", "name_skeleton": "amazon-bot"})}
		if got := nameBrandMatch(events); got != 1 {
			t.Errorf("name_brand_match = %v, want 1", got)
		}
	})
}

func TestFirstDayDistinctDomains(t *testing.T) {
	day := 24 * time.Hour
	t.Run("counts distinct domains within the first day, boundary inclusive", func(t *testing.T) {
		events := []event.Event{
			ev("c1", "content.sent", 0, map[string]any{"recipient_domain": "a.example.test"}),
			ev("c2", "content.sent", time.Hour, map[string]any{"recipient_domain": "b.example.test"}),
			ev("c3", "content.sent", time.Hour, map[string]any{"recipient_domain": "a.example.test"}),       // duplicate domain
			ev("c4", "content.sent", day, map[string]any{"recipient_domain": "c.example.test"}),             // exactly at the cutover: included
			ev("c5", "content.sent", day+time.Second, map[string]any{"recipient_domain": "d.example.test"}), // just past: excluded
		}
		if got := firstDayDistinctDomains(events, base, day); got != 3 {
			t.Errorf("first_day_distinct_domains = %v, want 3 (a, b, c)", got)
		}
	})
	t.Run("sends after the first day don't count", func(t *testing.T) {
		events := []event.Event{ev("c1", "content.sent", day+time.Hour, map[string]any{"recipient_domain": "late.example.test"})}
		if got := firstDayDistinctDomains(events, base, day); got != 0 {
			t.Errorf("first_day_distinct_domains = %v, want 0", got)
		}
	})
}

func TestSelfSendBeforeExternal(t *testing.T) {
	t.Run("counts self-sends before the first external send", func(t *testing.T) {
		events := []event.Event{
			ev("c1", "content.sent", 0, map[string]any{"recipient_is_own_identity": true}),
			ev("c2", "content.sent", time.Minute, map[string]any{"recipient_is_own_identity": true}),
			ev("c3", "content.sent", 2*time.Minute, map[string]any{"recipient_is_own_identity": false}),
			ev("c4", "content.sent", 3*time.Minute, map[string]any{"recipient_is_own_identity": true}), // after external: not counted
		}
		if got := selfSendBeforeExternal(events); got != 2 {
			t.Errorf("self_send_before_external = %v, want 2", got)
		}
	})
	t.Run("no external send yet: every self-send counts", func(t *testing.T) {
		events := []event.Event{ev("c1", "content.sent", 0, map[string]any{"recipient_is_own_identity": true})}
		if got := selfSendBeforeExternal(events); got != 1 {
			t.Errorf("self_send_before_external = %v, want 1", got)
		}
	})
	t.Run("no self-sends at all", func(t *testing.T) {
		events := []event.Event{ev("c1", "content.sent", 0, map[string]any{"recipient_is_own_identity": false})}
		if got := selfSendBeforeExternal(events); got != 0 {
			t.Errorf("self_send_before_external = %v, want 0", got)
		}
	})
	t.Run("out-of-order delivery: earliest external send wins regardless of slice order", func(t *testing.T) {
		events := []event.Event{
			ev("c3", "content.sent", 3*time.Minute, map[string]any{"recipient_is_own_identity": false}), // delivered first
			ev("c1", "content.sent", 0, map[string]any{"recipient_is_own_identity": true}),
			ev("c2", "content.sent", time.Minute, map[string]any{"recipient_is_own_identity": true}),
		}
		if got := selfSendBeforeExternal(events); got != 2 {
			t.Errorf("self_send_before_external = %v, want 2", got)
		}
	})
}

func TestBurstRatio(t *testing.T) {
	t.Run("all activity within the window", func(t *testing.T) {
		events := []event.Event{
			ev("r1", "resource.created", 0, nil),
			ev("c1", "content.sent", time.Minute, nil),
		}
		if got := burstRatio(events, at(2*time.Minute), 24*time.Hour); got != 1 {
			t.Errorf("burst_ratio = %v, want 1", got)
		}
	})
	t.Run("dormant then blast: old lifetime, one recent burst", func(t *testing.T) {
		events := []event.Event{
			ev("r1", "resource.created", 0, nil),
			ev("r2", "resource.created", 30*24*time.Hour, nil),
			ev("r3", "resource.created", 60*24*time.Hour, nil),
			ev("r4", "resource.created", 60*24*time.Hour, nil), // "just now" burst
		}
		now := at(60*24*time.Hour + time.Hour)
		got := burstRatio(events, now, 24*time.Hour)
		if got != 0.5 {
			t.Errorf("burst_ratio = %v, want 0.5 (2 of 4 within the last 24h)", got)
		}
	})
	t.Run("no activity events at all: no division by zero", func(t *testing.T) {
		events := []event.Event{ev("p1", "payment.attempt", 0, nil)}
		if got := burstRatio(events, at(time.Hour), 24*time.Hour); got != 0 {
			t.Errorf("burst_ratio = %v, want 0", got)
		}
	})
	t.Run("non-activity event types are excluded from both numerator and denominator", func(t *testing.T) {
		events := []event.Event{
			ev("s1", "subject.created", 0, nil),
			ev("u1", "subscription.changed", 0, nil),
			ev("r1", "resource.created", time.Hour, nil),
		}
		if got := burstRatio(events, at(2*time.Hour), 24*time.Hour); got != 1 {
			t.Errorf("burst_ratio = %v, want 1 (only r1 counts as activity)", got)
		}
	})
}

func TestNextRescoreAt(t *testing.T) {
	windows := Windows{OneHour: time.Hour, DayHour: 24 * time.Hour}

	t.Run("no events yet, but within the first day: the cutover is still pending", func(t *testing.T) {
		got := nextRescoreAt(nil, at(0), base, windows)
		want := base.Add(24 * time.Hour)
		if !got.Equal(want) {
			t.Errorf("nextRescoreAt = %v, want the first-day cutover %v", got, want)
		}
	})

	t.Run("no events and the first day has already passed: nothing pending", func(t *testing.T) {
		got := nextRescoreAt(nil, at(25*time.Hour), base, windows)
		if !got.IsZero() {
			t.Errorf("nextRescoreAt = %v, want zero", got)
		}
	})

	t.Run("1h window still holding an event: rescore when it exits", func(t *testing.T) {
		events := []event.Event{ev("r1", "resource.created", 0, nil)}
		now := at(30 * time.Minute)
		want := at(time.Hour) // the event's exit time (At + 1h)
		got := nextRescoreAt(events, now, base, windows)
		if !got.Equal(want) {
			t.Errorf("nextRescoreAt = %v, want %v", got, want)
		}
	})

	t.Run("first-day cutover pending", func(t *testing.T) {
		// No events within either sliding window, but the first-day cutover
		// (firstSeenAt + 24h) hasn't happened yet.
		now := at(2 * time.Hour)
		firstSeenAt := base
		want := firstSeenAt.Add(24 * time.Hour)
		got := nextRescoreAt(nil, now, firstSeenAt, windows)
		if !got.Equal(want) {
			t.Errorf("nextRescoreAt = %v, want the first-day cutover %v", got, want)
		}
	})

	t.Run("everything has already decayed and the first day has passed: nothing pending", func(t *testing.T) {
		events := []event.Event{ev("r1", "resource.created", 0, nil)}
		now := at(48 * time.Hour)
		got := nextRescoreAt(events, now, base, windows)
		if !got.IsZero() {
			t.Errorf("nextRescoreAt = %v, want zero", got)
		}
	})

	t.Run("earliest of several pending candidates wins", func(t *testing.T) {
		// now = base + 25h. firstSeenAt = base, so the first-day cutover
		// (base+24h) has already passed and contributes no candidate.
		// r1 (now-50m) is still inside the 1h window: exit at now+10m.
		// r2 (now-2h) has aged out of the 1h window but is still inside the
		// 24h window: exit at (now-2h)+24h = now+22h. The 1h-window exit is
		// earliest.
		firstSeenAt := base
		now := firstSeenAt.Add(25 * time.Hour)
		events := []event.Event{
			ev("r2", "resource.created", 23*time.Hour, nil),                // now - 2h
			ev("r1", "resource.created", 24*time.Hour+10*time.Minute, nil), // now - 50m
		}
		want := now.Add(10 * time.Minute)
		got := nextRescoreAt(events, now, firstSeenAt, windows)
		if !got.Equal(want) {
			t.Errorf("nextRescoreAt = %v, want %v", got, want)
		}
	})
}

func TestMatchesBrand(t *testing.T) {
	tests := []struct {
		skeleton string
		want     bool
	}{
		{"paypal support", true},
		{"paypal-verify-team", true},
		{"notifications agent", false},
		{"backup groups signup", false}, // regression: no short/generic brand tokens (ups, irs, chase) that would false-positive here
		{"first contact bot", false},    // regression: "irs" is not in the list (would otherwise match "fIRSt")
	}
	for _, tt := range tests {
		if got := matchesBrand(tt.skeleton); got != tt.want {
			t.Errorf("matchesBrand(%q) = %v, want %v", tt.skeleton, got, tt.want)
		}
	}
}
