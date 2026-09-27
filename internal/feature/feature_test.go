package feature

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
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
	res, err := Extract(context.Background(), "e2a", "acct_test", nil, nil, defaultWindows(0), BrandSet{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res != (Result{}) {
		t.Errorf("empty history: got %+v, want the zero Result", res)
	}
}

func TestExtract_RequiresWindowsNow(t *testing.T) {
	events := []event.Event{ev("e1", "subject.created", 0, nil)}
	_, err := Extract(context.Background(), "e2a", "acct_test", events, nil, Windows{}, BrandSet{})
	if err == nil {
		t.Fatalf("expected an error when windows.Now is zero")
	}
}

func TestExtract_RequiresPositiveWindowDurations(t *testing.T) {
	events := []event.Event{ev("e1", "subject.created", 0, nil)}
	_, err := Extract(context.Background(), "e2a", "acct_test", events, nil, Windows{Now: at(time.Hour)}, BrandSet{})
	if err == nil {
		t.Fatalf("expected an error when OneHour/DayHour are unset")
	}
}

func TestExtract_NilNeighborsTreatedAsNoNeighbors(t *testing.T) {
	events := []event.Event{ev("e1", "subject.created", 0, nil)}
	res, err := Extract(context.Background(), "e2a", "acct_test", events, nil, defaultWindows(time.Hour), BrandSet{})
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
	_, err := Extract(context.Background(), "e2a", "acct_test", events, fake, defaultWindows(time.Hour), BrandSet{})
	if err == nil {
		t.Fatalf("expected Extract to propagate a Neighbors.Evidence error")
	}
}

func TestExtract_LinkedFeaturesFromNeighbors(t *testing.T) {
	events := []event.Event{ev("e1", "subject.created", 0, nil)}
	fake := &fakeNeighbors{evidence: NeighborEvidence{DeletedCount: 2, LabelledAbusiveCount: 3, FingerprintShared: true, Truncated: true}}
	res, err := Extract(context.Background(), "e2a", "acct_test", events, fake, defaultWindows(time.Hour), BrandSet{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if fake.calls != 1 {
		t.Errorf("expected Neighbors.Evidence called exactly once, got %d", fake.calls)
	}
	f := res.Features
	if f.LinkedDeletedN != 2 || f.LinkedLabelledAbusiveN != 3 || f.FingerprintSeenOnOtherSubjects != 1 || f.NeighborsTruncated != 1 {
		t.Errorf("linked_* features = %+v, want {2,3,1,truncated=1}", f)
	}
}

func TestExtract_LinkedDeletedNSaturates(t *testing.T) {
	events := []event.Event{ev("e1", "subject.created", 0, nil)}
	fake := &fakeNeighbors{evidence: NeighborEvidence{DeletedCount: 19}}
	res, err := Extract(context.Background(), "e2a", "acct_test", events, fake, defaultWindows(time.Hour), BrandSet{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.Features.LinkedDeletedN != 3 {
		t.Errorf("LinkedDeletedN = %v, want saturated at 3 (S1 fix round)", res.Features.LinkedDeletedN)
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
		{"at the clamp ceiling: 24h", base, base.Add(24 * time.Hour), 24},
		{"past the clamp ceiling: a 14-day-old account clamps to 24h (B5)", base, base.Add(14 * 24 * time.Hour), 24},
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

func TestResourceCount_KindNormalization(t *testing.T) {
	// S10 fix round: `kind` is producer-supplied free text with no enum —
	// "Key", " key ", "KEY" must all count as the same kind.
	events := []event.Event{
		ev("r1", "resource.created", 0, map[string]any{"kind": "Key"}),
		ev("r2", "resource.created", 0, map[string]any{"kind": " KEY "}),
		ev("r3", "resource.created", 0, map[string]any{"kind": "key"}),
	}
	now := at(time.Minute)
	if got := resourceCount(events, "key", now, 0); got != 3 {
		t.Errorf("key_total with mixed-case/whitespace kind = %v, want 3", got)
	}
}

func TestUpgradeDelayMinutes(t *testing.T) {
	t.Run("paid upgrade recorded", func(t *testing.T) {
		events := []event.Event{
			ev("s1", "subject.created", 0, nil),
			ev("u1", "subscription.changed", 16*time.Minute, map[string]any{"plan": "scale", "amount_minor": float64(2900)}),
		}
		got := upgradeDelayMinutes(events, base, at(time.Hour))
		if got != 16 {
			t.Errorf("upgrade_delay_min = %v, want 16", got)
		}
	})
	t.Run("a free-plan change is not an upgrade (B5, proven)", func(t *testing.T) {
		events := []event.Event{
			ev("s1", "subject.created", 0, nil),
			ev("u1", "subscription.changed", time.Minute, map[string]any{"plan": "free", "amount_minor": float64(0)}),
		}
		got := upgradeDelayMinutes(events, base, at(45*time.Minute))
		if got != 45 {
			t.Errorf("upgrade_delay_min = %v, want 45 (falls back to elapsed-so-far; the free-plan event must not count)", got)
		}
		if u := upgraded(events); u != 0 {
			t.Errorf("upgraded = %v, want 0 for a free-plan change", u)
		}
	})
	t.Run("a subscription.changed with no amount_minor at all is not an upgrade", func(t *testing.T) {
		events := []event.Event{ev("u1", "subscription.changed", time.Minute, map[string]any{"plan": "free"})}
		if u := upgraded(events); u != 0 {
			t.Errorf("upgraded = %v, want 0", u)
		}
	})
	t.Run("no upgrade yet: falls back to minutes elapsed so far", func(t *testing.T) {
		events := []event.Event{ev("s1", "subject.created", 0, nil)}
		got := upgradeDelayMinutes(events, base, at(45*time.Minute))
		if got != 45 {
			t.Errorf("upgrade_delay_min = %v, want 45", got)
		}
	})
	t.Run("clamps at 24h even for a very old non-upgraded account (B5, proven)", func(t *testing.T) {
		events := []event.Event{ev("s1", "subject.created", 0, nil)}
		got := upgradeDelayMinutes(events, base, at(14*24*time.Hour))
		if got != upgradeDelayClampMinutes {
			t.Errorf("upgrade_delay_min = %v, want the clamp ceiling %v", got, upgradeDelayClampMinutes)
		}
	})
	t.Run("out-of-order delivery: earliest PAID subscription.changed wins regardless of slice order", func(t *testing.T) {
		events := []event.Event{
			ev("u2", "subscription.changed", 30*time.Minute, map[string]any{"amount_minor": float64(1000)}),
			ev("u1", "subscription.changed", 5*time.Minute, map[string]any{"amount_minor": float64(1000)}), // chronologically earlier, delivered second
		}
		got := upgradeDelayMinutes(events, base, at(time.Hour))
		if got != 5 {
			t.Errorf("upgrade_delay_min = %v, want 5 (the earliest event, even though it's later in the slice)", got)
		}
	})
	t.Run("a free change before a later paid one: only the paid one counts", func(t *testing.T) {
		events := []event.Event{
			ev("u1", "subscription.changed", 2*time.Minute, map[string]any{"amount_minor": float64(0)}),
			ev("u2", "subscription.changed", 10*time.Minute, map[string]any{"amount_minor": float64(2900)}),
		}
		got := upgradeDelayMinutes(events, base, at(time.Hour))
		if got != 10 {
			t.Errorf("upgrade_delay_min = %v, want 10 (the first PAID event, not the earlier free one)", got)
		}
		if u := upgraded(events); u != 1 {
			t.Errorf("upgraded = %v, want 1", u)
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

// smallTestBrands is a small BrandSet used by feature-level tests that only
// need to prove Extract/nameBrandMatch reads the right field and wires
// BrandSet correctly — the exhaustive matching-mechanism tests live in
// brand_test.go, and the exhaustive "does the real shipped list behave"
// tests live in TestLoadBrandsFile_ShippedListRegressionCases below.
func smallTestBrands() BrandSet {
	return NewBrandSet([]BrandEntry{{Name: "PayPal", Aliases: []string{"Pay Pal"}}})
}

func TestNameBrandMatchAndHasAt(t *testing.T) {
	brands := smallTestBrands()
	t.Run("brand match on the raw name", func(t *testing.T) {
		events := []event.Event{ev("r1", "resource.created", 0, map[string]any{"name": "PayPal Support"})}
		if got := nameBrandMatch(events, brands); got != 1 {
			t.Errorf("name_brand_match = %v, want 1", got)
		}
	})
	t.Run("no brand match", func(t *testing.T) {
		events := []event.Event{ev("r1", "resource.created", 0, map[string]any{"name": "Notifications Agent"})}
		if got := nameBrandMatch(events, brands); got != 0 {
			t.Errorf("name_brand_match = %v, want 0", got)
		}
	})
	t.Run("empty BrandSet never matches", func(t *testing.T) {
		events := []event.Event{ev("r1", "resource.created", 0, map[string]any{"name": "PayPal Support"})}
		if got := nameBrandMatch(events, BrandSet{}); got != 0 {
			t.Errorf("name_brand_match with an empty BrandSet = %v, want 0", got)
		}
	})
	t.Run("has_at checks the RAW name, not a folded form", func(t *testing.T) {
		// '@' folds to 'a' in Skeleton (event.Skeleton's leetspeak table), so
		// name_has_at must read the raw "name" field.
		events := []event.Event{ev("r1", "resource.created", 0, map[string]any{"name": "support@agent"})}
		if got := nameHasAt(events); got != 1 {
			t.Errorf("name_has_at = %v, want 1", got)
		}
	})
	t.Run("resource.deleted also counts", func(t *testing.T) {
		events := []event.Event{ev("r1", "resource.deleted", 0, map[string]any{"name": "PayPal Bot"})}
		if got := nameBrandMatch(events, brands); got != 1 {
			t.Errorf("name_brand_match = %v, want 1", got)
		}
	})
}

func TestFirstDayDistinctDomains(t *testing.T) {
	day := 24 * time.Hour
	t.Run("counts distinct domains within the first day, boundary inclusive, case-folded", func(t *testing.T) {
		events := []event.Event{
			ev("c1", "content.sent", 0, map[string]any{"recipient_domain": "a.example.test"}),
			ev("c2", "content.sent", time.Hour, map[string]any{"recipient_domain": "b.example.test"}),
			ev("c3", "content.sent", time.Hour, map[string]any{"recipient_domain": "A.EXAMPLE.TEST"}),       // same domain, different case (S10)
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

func TestCoalesce(t *testing.T) {
	epoch := time.Unix(0, 0).UTC()
	tests := []struct {
		name string
		in   time.Time
		want time.Time
	}{
		{"already on a boundary", epoch, epoch},
		{"just past a boundary rounds up to the next one", epoch.Add(time.Minute), epoch.Add(5 * time.Minute)},
		{"exactly the next boundary stays put", epoch.Add(5 * time.Minute), epoch.Add(5 * time.Minute)},
		{"just past the next boundary rounds up again", epoch.Add(5*time.Minute + time.Nanosecond), epoch.Add(10 * time.Minute)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := coalesce(tt.in); !got.Equal(tt.want) {
				t.Errorf("coalesce(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestNextRescoreAt(t *testing.T) {
	windows := Windows{OneHour: time.Hour, DayHour: 24 * time.Hour}

	t.Run("no events yet, but within the first day: the cutover is still pending", func(t *testing.T) {
		got := nextRescoreAt(nil, at(0), base, windows)
		want := coalesce(base.Add(24 * time.Hour))
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
		want := coalesce(at(time.Hour)) // the event's exit time (At + 1h)
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
		want := coalesce(firstSeenAt.Add(24 * time.Hour))
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
		want := coalesce(now.Add(10 * time.Minute))
		got := nextRescoreAt(events, now, firstSeenAt, windows)
		if !got.Equal(want) {
			t.Errorf("nextRescoreAt = %v, want %v", got, want)
		}
	})

	t.Run("non-windowed event types do not schedule a window-exit rescore (S6, proven)", func(t *testing.T) {
		events := []event.Event{ev("p1", "payment.attempt", 0, nil)}
		now := at(30 * time.Minute)
		// No resource.created/content.sent event exists, so the only
		// pending candidate is the first-day cutover, not a window exit for
		// the payment.attempt event.
		want := coalesce(base.Add(24 * time.Hour))
		got := nextRescoreAt(events, now, base, windows)
		if !got.Equal(want) {
			t.Errorf("nextRescoreAt = %v, want the first-day cutover %v (payment.attempt must not schedule a window exit)", got, want)
		}
	})

	t.Run("a future-dated event schedules a rescore for when its window will finally see it (B4, proven)", func(t *testing.T) {
		events := []event.Event{ev("r1", "resource.created", 2*time.Hour, nil)} // dated 2h ahead of now
		now := at(0)
		want := coalesce(at(2 * time.Hour))
		got := nextRescoreAt(events, now, base, windows)
		if !got.Equal(want) {
			t.Errorf("nextRescoreAt = %v, want %v (the future event's own timestamp)", got, want)
		}
	})
}

// repoRoot locates the repository root from this test file's own path, so
// TestLoadBrandsFile_ShippedListRegressionCases can load the real shipped
// config/brands.yaml rather than a copy.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller failed")
	}
	// this file: <root>/internal/feature/feature_test.go
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

// TestLoadBrandsFile_ShippedListRegressionCases loads the real
// config/brands.yaml this repo ships and checks it against every
// false-positive/false-negative case the S3 fix round's reviews raised —
// this is the actual evidence the shipped list (not just the matching
// mechanism, covered exhaustively in brand_test.go) behaves.
func TestLoadBrandsFile_ShippedListRegressionCases(t *testing.T) {
	brands, err := LoadBrandsFile(filepath.Join(repoRoot(t), "config", "brands.yaml"))
	if err != nil {
		t.Fatalf("LoadBrandsFile: %v", err)
	}

	mustMatch := []string{
		"PayPal Support",
		"PAYPAL SECURITY TEAM", // all-caps
		"Wells Fargo Alerts",
		"wells-fargo-verify",
		"Bank of America",
		"Pay Pal",
		"pay-pal-support",
	}
	for _, name := range mustMatch {
		if !brands.Matches(name) {
			t.Errorf("brands.Matches(%q) = false, want true", name)
		}
	}

	mustNotMatch := []string{
		"Pineapple Analytics Bot",
		"Grapple Sync Agent",
		"Applebee's Rewards",
		"Amazonas Logistics",
		"Striped Shirt Co",
		"Google Calendar Sync",
		"Stripe Webhook Relay",
		"Microsoft Teams Relay",
		"Notifications Agent",
	}
	for _, name := range mustNotMatch {
		if brands.Matches(name) {
			t.Errorf("brands.Matches(%q) = true, want false", name)
		}
	}
}
