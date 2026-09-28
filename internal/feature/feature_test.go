package feature

import (
	"context"
	"errors"
	"fmt"
	"math"
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
	res, err := Extract(context.Background(), "e2a", "acct_test", nil, nil, defaultWindows(0), BrandSet{}, WebmailSet{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res != (Result{}) {
		t.Errorf("empty history: got %+v, want the zero Result", res)
	}
}

func TestExtract_RequiresWindowsNow(t *testing.T) {
	events := []event.Event{ev("e1", "subject.created", 0, nil)}
	_, err := Extract(context.Background(), "e2a", "acct_test", events, nil, Windows{}, BrandSet{}, WebmailSet{})
	if err == nil {
		t.Fatalf("expected an error when windows.Now is zero")
	}
}

func TestExtract_RequiresPositiveWindowDurations(t *testing.T) {
	events := []event.Event{ev("e1", "subject.created", 0, nil)}
	_, err := Extract(context.Background(), "e2a", "acct_test", events, nil, Windows{Now: at(time.Hour)}, BrandSet{}, WebmailSet{})
	if err == nil {
		t.Fatalf("expected an error when OneHour/DayHour are unset")
	}
}

func TestExtract_NilNeighborsTreatedAsNoNeighbors(t *testing.T) {
	events := []event.Event{ev("e1", "subject.created", 0, nil)}
	res, err := Extract(context.Background(), "e2a", "acct_test", events, nil, defaultWindows(time.Hour), BrandSet{}, WebmailSet{})
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
	_, err := Extract(context.Background(), "e2a", "acct_test", events, fake, defaultWindows(time.Hour), BrandSet{}, WebmailSet{})
	if err == nil {
		t.Fatalf("expected Extract to propagate a Neighbors.Evidence error")
	}
}

func TestExtract_LinkedFeaturesFromNeighbors(t *testing.T) {
	events := []event.Event{ev("e1", "subject.created", 0, nil)}
	fake := &fakeNeighbors{evidence: NeighborEvidence{DeletedCount: 2, LabelledAbusiveCount: 3, FingerprintShared: true, Truncated: true}}
	res, err := Extract(context.Background(), "e2a", "acct_test", events, fake, defaultWindows(time.Hour), BrandSet{}, WebmailSet{})
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
	res, err := Extract(context.Background(), "e2a", "acct_test", events, fake, defaultWindows(time.Hour), BrandSet{}, WebmailSet{})
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
			ev("u1", "subscription.changed", 16*time.Minute, map[string]any{"plan": "plan_b", "status": "active", "amount_minor": float64(2900)}),
		}
		got := upgradeDelayMinutes(events, base, at(time.Hour))
		if got != 16 {
			t.Errorf("upgrade_delay_min = %v, want 16", got)
		}
	})
	t.Run("a trialing subscription with a price is NOT an upgrade (R5, round 2)", func(t *testing.T) {
		events := []event.Event{
			ev("s1", "subject.created", 0, nil),
			ev("u1", "subscription.changed", 16*time.Minute, map[string]any{"plan": "plan_b", "status": "trialing", "amount_minor": float64(2900)}),
		}
		if u := upgraded(events); u != 0 {
			t.Errorf("upgraded = %v, want 0 for a trialing subscription (a price on file is not the same as actually being charged)", u)
		}
		got := upgradeDelayMinutes(events, base, at(45*time.Minute))
		if got != 45 {
			t.Errorf("upgrade_delay_min = %v, want 45 (falls back to elapsed-so-far; the trialing event must not count as the upgrade)", got)
		}
	})
	t.Run("active status with a price after a trialing one: only the active one counts", func(t *testing.T) {
		events := []event.Event{
			ev("s1", "subject.created", 0, nil),
			ev("u1", "subscription.changed", 5*time.Minute, map[string]any{"plan": "plan_b", "status": "trialing", "amount_minor": float64(2900)}),
			ev("u2", "subscription.changed", 20*time.Minute, map[string]any{"plan": "plan_b", "status": "active", "amount_minor": float64(2900)}),
		}
		if u := upgraded(events); u != 1 {
			t.Errorf("upgraded = %v, want 1", u)
		}
		got := upgradeDelayMinutes(events, base, at(time.Hour))
		if got != 20 {
			t.Errorf("upgrade_delay_min = %v, want 20 (the active event, not the earlier trialing one)", got)
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
			ev("u2", "subscription.changed", 30*time.Minute, map[string]any{"status": "active", "amount_minor": float64(1000)}),
			ev("u1", "subscription.changed", 5*time.Minute, map[string]any{"status": "active", "amount_minor": float64(1000)}), // chronologically earlier, delivered second
		}
		got := upgradeDelayMinutes(events, base, at(time.Hour))
		if got != 5 {
			t.Errorf("upgrade_delay_min = %v, want 5 (the earliest event, even though it's later in the slice)", got)
		}
	})
	t.Run("a free change before a later paid one: only the paid one counts", func(t *testing.T) {
		events := []event.Event{
			ev("u1", "subscription.changed", 2*time.Minute, map[string]any{"status": "active", "amount_minor": float64(0)}),
			ev("u2", "subscription.changed", 10*time.Minute, map[string]any{"status": "active", "amount_minor": float64(2900)}),
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
		if got := boolToFloat(len(namedBrandNames(events, brands)) > 0); got != 1 {
			t.Errorf("name_brand_match = %v, want 1", got)
		}
	})
	t.Run("no brand match", func(t *testing.T) {
		events := []event.Event{ev("r1", "resource.created", 0, map[string]any{"name": "Notifications Agent"})}
		if got := boolToFloat(len(namedBrandNames(events, brands)) > 0); got != 0 {
			t.Errorf("name_brand_match = %v, want 0", got)
		}
	})
	t.Run("empty BrandSet never matches", func(t *testing.T) {
		events := []event.Event{ev("r1", "resource.created", 0, map[string]any{"name": "PayPal Support"})}
		if got := boolToFloat(len(namedBrandNames(events, BrandSet{})) > 0); got != 0 {
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
		// Not "PayPal Bot" (R6 round 2): "bot" is now an integration-token
		// that suppresses a match on its own (BrandSet.Matches) — this test
		// is about resource.deleted being checked at all, not about that
		// gate, so it uses a name the gate leaves alone.
		events := []event.Event{ev("r1", "resource.deleted", 0, map[string]any{"name": "PayPal Alert"})}
		if got := boolToFloat(len(namedBrandNames(events, brands)) > 0); got != 1 {
			t.Errorf("name_brand_match = %v, want 1", got)
		}
	})
}

// domainsFor builds n content.sent events, each to its own distinct
// domain, all landing inside window from firstSeenAt.
func domainsFor(n int) []event.Event {
	events := make([]event.Event, n)
	for i := 0; i < n; i++ {
		events[i] = ev(fmt.Sprintf("c%d", i), "content.sent", 0, map[string]any{"recipient_domain": fmt.Sprintf("customer-%d.example.test", i)})
	}
	return events
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
		// 3 distinct domains (a, b, c) -> log1p-scaled (D2 round 3), not
		// the raw count: firstDayDistinctDomainsLogScale * log1p(3).
		want := firstDayDistinctDomainsLogScale * math.Log1p(3)
		if got := firstDayDistinctDomains(events, base, day); math.Abs(got-want) > 1e-9 {
			t.Errorf("first_day_distinct_domains = %v, want %v (log1p(3) scaled)", got, want)
		}
	})
	t.Run("sends after the first day don't count", func(t *testing.T) {
		events := []event.Event{ev("c1", "content.sent", day+time.Hour, map[string]any{"recipient_domain": "late.example.test"})}
		if got := firstDayDistinctDomains(events, base, day); got != 0 {
			t.Errorf("first_day_distinct_domains = %v, want 0", got)
		}
	})
	t.Run("no domains at all is exactly zero", func(t *testing.T) {
		if got := firstDayDistinctDomains(nil, base, day); got != 0 {
			t.Errorf("first_day_distinct_domains = %v, want 0", got)
		}
	})
	// D2 round 3: replaced the hard cap of 10 with a log1p(n) curve scaled
	// so n=10 reproduces exactly the OLD cap's contribution (10) — the
	// weight (config/local_weights.yaml) didn't have to change — while
	// still growing, just compressed, past it: volume sensitivity above
	// the old cap must not be exactly zero.
	t.Run("n=10 matches the old hard cap's value exactly (weight-compatible)", func(t *testing.T) {
		got := firstDayDistinctDomains(domainsFor(10), base, day)
		if math.Abs(got-10) > 1e-9 {
			t.Errorf("first_day_distinct_domains(10 domains) = %v, want 10 (log1p scaling must reproduce the old cap's contribution exactly at n=10)", got)
		}
	})
	t.Run("n=150 is meaningfully higher than n=10 (not flat past the old cap)", func(t *testing.T) {
		at10 := firstDayDistinctDomains(domainsFor(10), base, day)
		at150 := firstDayDistinctDomains(domainsFor(150), base, day)
		if at150 <= at10+5 {
			t.Errorf("first_day_distinct_domains(150) = %v, first_day_distinct_domains(10) = %v — want the former meaningfully higher (>=+5), not flat past the old cap", at150, at10)
		}
	})
	t.Run("n=30 (benign_receipts_fanout's own count) still lands well under a doubling of the old cap", func(t *testing.T) {
		got := firstDayDistinctDomains(domainsFor(30), base, day)
		if got < 10 || got > 20 {
			t.Errorf("first_day_distinct_domains(30) = %v, want in [10,20] (grows past the old cap's 10, but compressed by log1p, not linear)", got)
		}
	})
	t.Run("strictly monotonic in the number of distinct domains", func(t *testing.T) {
		prev := 0.0
		for _, n := range []int{1, 5, 10, 30, 100, 300} {
			got := firstDayDistinctDomains(domainsFor(n), base, day)
			if got <= prev {
				t.Errorf("first_day_distinct_domains(%d) = %v, want strictly greater than the previous n's %v", n, got, prev)
			}
			prev = got
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
	t.Run("saturates at the cap (R1 round 2)", func(t *testing.T) {
		var events []event.Event
		for i := 0; i < 8; i++ {
			events = append(events, ev(fmt.Sprintf("c%d", i), "content.sent", time.Duration(i)*time.Minute, map[string]any{"recipient_is_own_identity": true}))
		}
		if got := selfSendBeforeExternal(events); got != selfSendBeforeExternalCap {
			t.Errorf("self_send_before_external = %v, want the cap %v (8 self-sends before ever sending externally must not swamp the model uncapped)", got, selfSendBeforeExternalCap)
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

// TestLoadBrandsFile_Round2Probes is R6 round 2's own probe table (option
// (a): apple/stripe/google/amazon/dhl/microsoft are re-included in
// config/brands.yaml, gated by the SAME integration-token rule already
// protecting every other brand — not by blanket exclusion). Every case
// here is exactly one the re-review specified.
func TestLoadBrandsFile_Round2Probes(t *testing.T) {
	brands, err := LoadBrandsFile(filepath.Join(repoRoot(t), "config", "brands.yaml"))
	if err != nil {
		t.Fatalf("LoadBrandsFile: %v", err)
	}

	mustMatch := []string{
		"Wells Fargo Alerts",
		"Pay-Pal Security",
		"PayPaI", // capital I impersonating lowercase l, folded pre-lowercase by event.Skeleton
		"pаypal", // Cyrillic а (U+0430) impersonating Latin a
		"Netflix Billing",
		"Coinbase Support",
		"PayPalSupport", // glued, each word capitalized: no separator, camelCase-split (R6)
		"WellsFargo",
		"BankOfAmerica",
		"Apple ID Verification",     // re-included generic brand, no integration token nearby
		"Google Account Recovery",   // re-included generic brand
		"Amazon Order Confirmation", // re-included generic brand
		"DHL Shipping Alert",        // re-included generic brand, no integration token nearby
		"Stripe Payment Receipt",    // re-included generic brand, "receipt" is not an integration token
	}
	for _, name := range mustMatch {
		if !brands.Matches(name) {
			t.Errorf("brands.Matches(%q) = false, want true", name)
		}
	}

	mustNotMatch := []string{
		"Pineapple Support", // word-boundary safety, unaffected by the new rule
		"Stripe Webhook Relay",
		"Google Calendar Sync", // integration token ("sync") two words away from the brand mention, not adjacent — R6's chosen rule is text-wide, not positional, specifically because of this case
		"PayPal integration",
		"Coinbase Commerce webhook",
		"USPS tracking sync",
		// Documented v0 tradeoff (see config/brands.yaml): "tracking" is an
		// integration token even though it's also the single most natural
		// companion word for a real DHL/USPS phishing lure. Accepted
		// deliberately — the reviewed false-positive probes (Stripe/Google/
		// Microsoft integration names) are the more common case a v0 name
		// list has to get right, and the same rule cannot special-case one
		// brand without reintroducing the false-positive it exists to close.
		"DHL Package Tracking",
	}
	for _, name := range mustNotMatch {
		if brands.Matches(name) {
			t.Errorf("brands.Matches(%q) = true, want false", name)
		}
	}
}

// TestLoadBrandsFile_Round3Probes is D1 round 3. Proven: event.Skeleton
// folds an upper-case "I" to 'l' pre-lowercase (catching "PayPaI"-style
// impersonation), but a brand definition written in its natural mixed
// case ("Microsoft", "Netflix", "Coinbase", "Binance", "Bank of America")
// has a LOWER-case "i", which Skeleton never touches — so an all-caps
// candidate mention of the identical brand ("MICROSOFT SUPPORT") folds
// its (now upper-case) I to 'l' while the brand definition never does,
// and the two sides silently diverge. Every one of these five brands has
// an 'i' in its name for exactly this reason. Fixed by folding every
// remaining 'i' (canonicalise, brand.go) identically on both sides — and
// on config/brands.yaml's own examples, none of which happen to contain
// "i" (PayPal, Wells Fargo, Walmart, USPS, FedEx, Apple, Stripe, Google,
// Amazon, DHL), which is exactly why the original S3/R6 rounds never
// noticed this class of bug at all.
func TestLoadBrandsFile_Round3Probes(t *testing.T) {
	brands, err := LoadBrandsFile(filepath.Join(repoRoot(t), "config", "brands.yaml"))
	if err != nil {
		t.Fatalf("LoadBrandsFile: %v", err)
	}

	mustMatch := []string{
		"MICROSOFT SUPPORT",
		"NETFLIX BILLING",
		"COINBASE SUPPORT",
		"BINANCE SECURITY",
		"BANK OF AMERICA ALERT",
	}
	for _, name := range mustMatch {
		if !brands.Matches(name) {
			t.Errorf("brands.Matches(%q) = false, want true", name)
		}
	}

	// Every existing round-1/round-2 probe (word-boundary safety,
	// glued/camelCase compounds, and — most importantly — the
	// integration-token gate) must keep passing: the naive fix (folding
	// 'i' only in Matches' own candidate path, or only in brand
	// definitions) broke the integration-gate tests, which is why D1
	// requires canonicalise() to also rebuild integrationTokens itself.
	mustNotMatch := []string{
		"PAYPAL INTEGRATION",
		"STRIPE WEBHOOK RELAY",
		"MICROSOFT TEAMS RELAY",
		"USPS TRACKING SYNC",
		"COINBASE COMMERCE WEBHOOK",
	}
	for _, name := range mustNotMatch {
		if brands.Matches(name) {
			t.Errorf("brands.Matches(%q) = true, want false (integration-token gate must still fire after the i-fold)", name)
		}
	}
}

// TestLoadBrandsFile_S2bExtraBrands checks the shipped config/brands.yaml
// against the marketplace/social/shipping additions (scope's "extra
// public brands" item) and N1's case-sensitive UPS entry.
func TestLoadBrandsFile_S2bExtraBrands(t *testing.T) {
	brands, err := LoadBrandsFile(filepath.Join(repoRoot(t), "config", "brands.yaml"))
	if err != nil {
		t.Fatalf("LoadBrandsFile: %v", err)
	}

	mustMatch := []string{
		"Your eBay listing sold",
		"Poshmark payout pending",
		"Vinted account verification",
		"Your TikTok account was reported",
		"Facebook security alert",
		"Your Booking.com reservation",
		"UPS: delivery exception",
	}
	for _, name := range mustMatch {
		if !brands.Matches(name) {
			t.Errorf("brands.Matches(%q) = false, want true", name)
		}
	}

	mustNotMatch := []string{
		"it has its ups and downs", // N1: lower-case "ups" must not match
	}
	for _, name := range mustNotMatch {
		if brands.Matches(name) {
			t.Errorf("brands.Matches(%q) = true, want false", name)
		}
	}

	// N2/round 2's R3: the community-phrase gate applies to SUBJECT LINES
	// only, never to a resource/agent name (Matches/MatchedBrandNames) —
	// see TestBrandSet_NameMatchingNeverCommunityGated for that half.
	if len(brands.MatchedBrandNamesForSubject("Facebook group meetup")) != 0 {
		t.Errorf("MatchedBrandNamesForSubject(%q) matched, want none (N2: community context)", "Facebook group meetup")
	}
}

// --- S2b: resource-kind aliases (N4) ------------------------------------

func TestNormalizeResourceKind_Aliases(t *testing.T) {
	tests := []struct{ raw, want string }{
		{"key", "key"},
		{"Key", "key"},
		{"KEYS", "key"},
		{"api_key", "key"},
		{"api_keys", "key"},
		{"api-key", "key"},
		{"apikey", "key"},
		{"API Key", "key"},
		{"api key", "key"},
		{"agent", "agent"}, // not a key alias: passes through unchanged
	}
	for _, tt := range tests {
		if got := normalizeResourceKind(tt.raw); got != tt.want {
			t.Errorf("normalizeResourceKind(%q) = %q, want %q", tt.raw, got, tt.want)
		}
	}
}

func TestKeyVelocity_RecognisesAliasedKinds(t *testing.T) {
	events := []event.Event{
		ev("r1", "resource.created", 0, map[string]any{"kind": "api_key"}),
		ev("r2", "resource.created", 0, map[string]any{"kind": "API Key"}),
		ev("r3", "resource.created", 0, map[string]any{"kind": "keys"}),
		ev("r4", "resource.created", 0, map[string]any{"kind": "agent"}),
	}
	got := resourceCount(events, resourceKindKey, at(0), time.Hour)
	if got != 3 {
		t.Errorf("key_velocity_1h with aliased kinds = %v, want 3 (the agent kind must not count)", got)
	}
}

// --- S2b: send-volume young-account scoping (B1) ------------------------

// TestSends1h_HistoryRelativeNotCalendarGated is round 2's R1: a young
// account with NO prior sending history reads its current burst at
// (nearly) full strength; the SAME current volume from an account with a
// real, comparable prior baseline reads far lower — replacing round 1's
// hard 7-day cliff (which any of this ever landed the same score for,
// purely by calendar age, regardless of prior history).
func TestSends1h_HistoryRelativeNotCalendarGated(t *testing.T) {
	firstSeenAt := base
	events := []event.Event{
		ev("c1", "content.sent", 6*24*time.Hour, map[string]any{"recipient_count": float64(50)}),
	}
	young := sends1h(events, firstSeenAt.Add(6*24*time.Hour), firstSeenAt, time.Hour)
	if young <= 0 {
		t.Errorf("sends_1h (young account, no prior history) = %v, want a strongly positive burst_factor-driven value", young)
	}

	// An established account (60 days old) with a comparable REAL prior
	// baseline sending the SAME current volume must read far lower than a
	// brand-new account with no history at all would for the same burst —
	// never a hard 0 the way the old calendar gate produced, but heavily
	// discounted by BOTH burstFactor (a real baseline to compare against)
	// and ageDecayFactor (floored at 0.2, never fully gone).
	establishedEvents := []event.Event{
		// A comparable prior 10-minute peak within the last 30 days
		// (excluding the trailing 24h "current" period).
		ev("c0", "content.sent", 60*24*time.Hour-48*time.Hour, map[string]any{"recipient_count": float64(300)}),
		ev("c1", "content.sent", 60*24*time.Hour, map[string]any{"recipient_count": float64(300)}),
	}
	oldNow := firstSeenAt.Add(60 * 24 * time.Hour)
	old := sends1h(establishedEvents, oldNow, firstSeenAt, time.Hour)
	if old <= 0 {
		t.Errorf("sends_1h (established, real prior baseline) = %v, want > 0 (never a hard 0)", old)
	}
	if old >= young {
		t.Errorf("sends_1h established=%v must read LOWER than young=%v despite an equal or larger raw burst", old, young)
	}
}

func TestSends10mMax_GatedByAccountAgeAndCappedNotLifetime(t *testing.T) {
	firstSeenAt := base
	// A burst inside the first week counts.
	events := []event.Event{
		ev("c1", "content.sent", time.Hour, map[string]any{"recipient_count": float64(120)}),
		ev("c2", "content.sent", time.Hour+5*time.Minute, map[string]any{"recipient_count": float64(80)}),
	}
	now := firstSeenAt.Add(2 * time.Hour)
	got := sends10mMax(events, now, firstSeenAt)
	if got != 200 {
		t.Errorf("sends_10m_max (young account) = %v, want 200 (both events in the same 10m window)", got)
	}

	// The SAME historical burst, viewed 60 days later (an established
	// sender), must no longer register at all — B1: "the flag never
	// decays" is exactly the bug this gate closes.
	longAfter := firstSeenAt.Add(60 * 24 * time.Hour)
	gotLater := sends10mMax(events, longAfter, firstSeenAt)
	if gotLater != 0 {
		t.Errorf("sends_10m_max (60 days after a first-week burst) = %v, want 0", gotLater)
	}
}

func TestSendsFirstDay_NotGatedByAccountAge(t *testing.T) {
	firstSeenAt := base
	events := []event.Event{ev("c1", "content.sent", time.Hour, map[string]any{"recipient_count": float64(40)})}
	// SendsFirstDay is a permanent day-1 fact: it must still report the
	// same value long after the account has matured, unlike its
	// history-relative siblings Sends1h/Sends10mMax/WebmailSends1h/
	// DistinctRecipients1h.
	now := firstSeenAt.Add(60 * 24 * time.Hour)
	got := sendsFirstDay(events, firstSeenAt, now, 24*time.Hour)
	if got != 40 {
		t.Errorf("sends_first_day (60 days later) = %v, want 40 (permanent, not gated)", got)
	}
}

func TestSends10mMax_FutureDatedEventsExcluded(t *testing.T) {
	// N5: an event dated after `now` must not be counted.
	events := []event.Event{ev("c1", "content.sent", 2*time.Hour, map[string]any{"recipient_count": float64(999)})}
	now := at(time.Hour)
	if got := sends10mMax(events, now, base); got != 0 {
		t.Errorf("sends_10m_max with a future-dated event = %v, want 0", got)
	}
}

func TestRecipientCountOf_PerEventCap(t *testing.T) {
	e := ev("c1", "content.sent", 0, map[string]any{"recipient_count": float64(10000)})
	if got := recipientCountOf(e); got != sendsVolumeCap {
		t.Errorf("recipientCountOf with an oversized recipient_count = %v, want capped at %v", got, sendsVolumeCap)
	}
}

func TestRecipientCountOf_FallsBackToOne(t *testing.T) {
	tests := []map[string]any{
		nil,
		{"recipient_count": float64(0)},
		{"recipient_count": float64(-1)},
		{"recipient_count": float64(2.5)},
	}
	for _, data := range tests {
		e := ev("c1", "content.sent", 0, data)
		if got := recipientCountOf(e); got != 1 {
			t.Errorf("recipientCountOf(%v) = %v, want 1", data, got)
		}
	}
}

// --- S2b: webmail features (S7) -----------------------------------------

func TestWebmailSends1h_ComputedDirectlyNotShareTimesVolume(t *testing.T) {
	webmail := NewWebmailSet([]string{"gmail.com"})
	firstSeenAt := base
	// Lifetime: 1 webmail send of 10, 1 non-webmail send of 90 (so the
	// LIFETIME webmail share is 10/100 = 0.1). But the last hour is
	// ENTIRELY webmail (40 recipients) — share*volume would have reported
	// 0.1*40 = 4, which is wrong; the direct computation must report 40.
	events := []event.Event{
		ev("c1", "content.sent", 0, map[string]any{"recipient_domain": "gmail.com", "recipient_count": float64(10)}),
		ev("c2", "content.sent", time.Minute, map[string]any{"recipient_domain": "corp-example.test", "recipient_count": float64(90)}),
		ev("c3", "content.sent", 50*time.Minute, map[string]any{"recipient_domain": "gmail.com", "recipient_count": float64(40)}),
	}
	now := at(time.Hour)
	got := webmailSends1h(events, now, firstSeenAt, time.Hour, webmail)
	if got != 40 {
		t.Errorf("webmail_sends_1h = %v, want 40 (direct computation, not share*volume)", got)
	}
}

func TestWebmailRecipientShare_LifetimeAndNotGated(t *testing.T) {
	webmail := NewWebmailSet([]string{"gmail.com"})
	firstSeenAt := base
	events := []event.Event{
		ev("c1", "content.sent", 0, map[string]any{"recipient_domain": "gmail.com", "recipient_count": float64(10)}),
		ev("c2", "content.sent", time.Minute, map[string]any{"recipient_domain": "corp-example.test", "recipient_count": float64(90)}),
	}
	// Evaluated 60 days later: still 0.1, since this feature is a
	// lifetime ratio, never gated by account age.
	now := firstSeenAt.Add(60 * 24 * time.Hour)
	got := webmailRecipientShare(events, now, webmail)
	if math.Abs(got-0.1) > 1e-9 {
		t.Errorf("webmail_recipient_share = %v, want 0.1", got)
	}
}

// --- S2b: subject_brand_match (S1, S2) -----------------------------------

func TestSubjectBrandMatch_NotGatedBySubjectsOwnWords(t *testing.T) {
	brands := smallTestBrands()
	// S1: "tracking" inside the SUBJECT LINE itself must not suppress the
	// match (only the account's own resource/agent name can).
	events := []event.Event{ev("c1", "content.sent", 0, map[string]any{"subject_line": "Your PayPal package tracking update"})}
	got := subjectBrandMatch(events, at(30*time.Minute), time.Hour, brands, nil, nil)
	if got != 1 {
		t.Errorf("subject_brand_match = %v, want 1 (subject's own words must not gate this)", got)
	}
}

// TestSubjectBrandMatch_ExemptsOnlyTheAdjacentBrand is round 2's R2: only
// the brand adjacent to the integration token in a live agent's name is
// exempted from subject matching — a DIFFERENT brand mentioned in a
// subject line must still count.
func TestSubjectBrandMatch_ExemptsOnlyTheAdjacentBrand(t *testing.T) {
	brands := mechanismBrands() // PayPal (+ alias), Apple, Amazon, Stripe, Wells Fargo, Bank of America
	events := []event.Event{ev("c1", "content.sent", 0, map[string]any{"subject_line": "Your PayPal account was flagged, also check Stripe"})}
	exempt := map[string]struct{}{"PayPal": {}}
	got := subjectBrandMatch(events, at(30*time.Minute), time.Hour, brands, nil, exempt)
	if got != 1 {
		t.Errorf("subject_brand_match = %v, want 1 (Stripe must still count; only PayPal is exempt)", got)
	}
}

func TestSubjectBrandMatch_ExcludesBrandsAlreadyNamed(t *testing.T) {
	brands := smallTestBrands()
	events := []event.Event{ev("c1", "content.sent", 0, map[string]any{"subject_line": "Your PayPal account"})}
	alreadyNamed := map[string]struct{}{"PayPal": {}}
	got := subjectBrandMatch(events, at(time.Hour), time.Hour, brands, alreadyNamed, nil)
	if got != 0 {
		t.Errorf("subject_brand_match = %v, want 0 (S2: already counted by name_brand_match)", got)
	}
}

func TestSubjectBrandMatch_CapsAtThree(t *testing.T) {
	brands := NewBrandSet([]BrandEntry{{Name: "Fictaone"}, {Name: "Fictatwo"}, {Name: "Fictathree"}, {Name: "Fictafour"}})
	events := []event.Event{ev("c1", "content.sent", 0, map[string]any{"subject_line": "Fictaone Fictatwo Fictathree Fictafour update"})}
	got := subjectBrandMatch(events, at(30*time.Minute), time.Hour, brands, nil, nil)
	if got != subjectBrandMatchCap {
		t.Errorf("subject_brand_match = %v, want capped at %v", got, subjectBrandMatchCap)
	}
}

// --- Round 2, R2: precise integration-name subject suppression ----------

// TestExemptSubjectBrands_KeyNamedAPIDoesNotSuppress is round 2's R2:
// keys never count, even when named with an integration token AND a
// brand.
func TestExemptSubjectBrands_KeyNamedAPIDoesNotSuppress(t *testing.T) {
	brands := mechanismBrands()
	events := []event.Event{ev("r1", "resource.created", 0, map[string]any{"kind": "key", "name": "Stripe API Key"})}
	got := exemptSubjectBrands(events, brands)
	if len(got) != 0 {
		t.Errorf("exemptSubjectBrands (key resource) = %v, want empty — keys never count", got)
	}
}

// TestExemptSubjectBrands_DeletedAgentNamedSyncDoesNotSuppress is round
// 2's R2: only a LIVE (not later deleted) agent counts.
func TestExemptSubjectBrands_DeletedAgentNamedSyncDoesNotSuppress(t *testing.T) {
	brands := mechanismBrands()
	events := []event.Event{
		ev("r1", "resource.created", 0, map[string]any{"kind": "agent", "name": "Stripe Sync"}),
		ev("r2", "resource.deleted", time.Minute, map[string]any{"kind": "agent", "name": "Stripe Sync"}),
	}
	got := exemptSubjectBrands(events, brands)
	if len(got) != 0 {
		t.Errorf("exemptSubjectBrands (deleted agent) = %v, want empty — a deleted agent never counts", got)
	}
}

// TestExemptSubjectBrands_LiveAgentExemptsOnlyItsOwnBrand is round 2's
// R2's core positive case: a live agent named after an integration
// exempts ONLY the brand adjacent to the integration token in ITS OWN
// name, not every brand the account has ever mentioned anywhere.
func TestExemptSubjectBrands_LiveAgentExemptsOnlyItsOwnBrand(t *testing.T) {
	brands := mechanismBrands()
	events := []event.Event{
		ev("r1", "resource.created", 0, map[string]any{"kind": "agent", "name": "Stripe Webhook Relay"}),
	}
	got := exemptSubjectBrands(events, brands)
	if _, ok := got["Stripe"]; !ok || len(got) != 1 {
		t.Errorf("exemptSubjectBrands = %v, want exactly {Stripe}", got)
	}
}

// TestExemptSubjectBrands_NoIntegrationTokenExemptsNothing is round 2's
// R2: a live agent whose name matches a brand but carries no integration
// token at all must not exempt anything (that's an ordinary
// brand-impersonating name, already caught by name_brand_match/S2 — not
// a legitimate-integration signal).
func TestExemptSubjectBrands_NoIntegrationTokenExemptsNothing(t *testing.T) {
	brands := mechanismBrands()
	events := []event.Event{ev("r1", "resource.created", 0, map[string]any{"kind": "agent", "name": "PayPal Alert"})}
	got := exemptSubjectBrands(events, brands)
	if len(got) != 0 {
		t.Errorf("exemptSubjectBrands = %v, want empty (no integration token in the name)", got)
	}
}

// TestExtract_SubjectBrandMatchIntegratesWithNamedBrandNames is an
// Extract-level check that Features.NameBrandMatch and
// Features.SubjectBrandMatch never double-count the same brand (S2).
func TestExtract_SubjectBrandMatchDoesNotDoubleCount(t *testing.T) {
	brands := smallTestBrands()
	events := []event.Event{
		ev("r1", "resource.created", 0, map[string]any{"name": "PayPal Alert"}),
		ev("c1", "content.sent", time.Minute, map[string]any{"subject_line": "Your PayPal account"}),
	}
	res, err := Extract(context.Background(), "e2a", "acct_test", events, nil, defaultWindows(time.Hour), brands, WebmailSet{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.Features.NameBrandMatch != 1 {
		t.Errorf("NameBrandMatch = %v, want 1", res.Features.NameBrandMatch)
	}
	if res.Features.SubjectBrandMatch != 0 {
		t.Errorf("SubjectBrandMatch = %v, want 0 (S2: PayPal already counted via NameBrandMatch)", res.Features.SubjectBrandMatch)
	}
}

// --- Round 2, R1: history-relative volume signal, no hard age cliff ----

// TestAgeDecayFactor_NoCliff is R1: probing 6d23h, 7d and 7d1h must show
// smooth continuity (each step differs only by the ordinary slope of the
// ramp), never a jump the way the old hard 7-day gate produced (1 -> 0).
func TestAgeDecayFactor_NoCliff(t *testing.T) {
	firstSeenAt := base
	at6d23h := ageDecayFactor(firstSeenAt, firstSeenAt.Add(6*24*time.Hour+23*time.Hour))
	at7d := ageDecayFactor(firstSeenAt, firstSeenAt.Add(7*24*time.Hour))
	at7d1h := ageDecayFactor(firstSeenAt, firstSeenAt.Add(7*24*time.Hour+time.Hour))
	at8d := ageDecayFactor(firstSeenAt, firstSeenAt.Add(8*24*time.Hour))

	// No cliff: consecutive probes differ by a small, continuous amount,
	// not by anywhere near the old gate's full 1 -> 0 jump.
	const maxStepAcrossOneHour = 0.01
	if diff := at7d - at7d1h; diff < 0 || diff > maxStepAcrossOneHour {
		t.Errorf("ageDecayFactor(7d)=%v -> ageDecayFactor(7d1h)=%v moved by %v, want a small continuous step (<= %v)", at7d, at7d1h, diff, maxStepAcrossOneHour)
	}
	if diff := at6d23h - at7d; diff < 0 || diff > maxStepAcrossOneHour {
		t.Errorf("ageDecayFactor(6d23h)=%v -> ageDecayFactor(7d)=%v moved by %v, want a small continuous step (<= %v)", at6d23h, at7d, diff, maxStepAcrossOneHour)
	}
	// Monotonically non-increasing with age over this range.
	if !(at6d23h >= at7d && at7d >= at7d1h && at7d1h >= at8d) {
		t.Errorf("ageDecayFactor must be non-increasing with age: 6d23h=%v 7d=%v 7d1h=%v 8d=%v", at6d23h, at7d, at7d1h, at8d)
	}
}

// TestAgeDecayFactor_Bounds is R1's formula: clamp(1 - (age_days-3)/27,
// 0.2, 1) — full weight through day 3, floor of 0.2 from ~day 25 on.
func TestAgeDecayFactor_Bounds(t *testing.T) {
	firstSeenAt := base
	if got := ageDecayFactor(firstSeenAt, firstSeenAt); got != 1 {
		t.Errorf("ageDecayFactor(age=0) = %v, want 1", got)
	}
	if got := ageDecayFactor(firstSeenAt, firstSeenAt.Add(3*24*time.Hour)); got != 1 {
		t.Errorf("ageDecayFactor(age=3d) = %v, want 1", got)
	}
	if got := ageDecayFactor(firstSeenAt, firstSeenAt.Add(90*24*time.Hour)); got != 0.2 {
		t.Errorf("ageDecayFactor(age=90d) = %v, want the 0.2 floor", got)
	}
	// Never a hard 0: a genuinely established sender still gets SOME
	// weight from a volume signal, just heavily discounted.
	if got := ageDecayFactor(firstSeenAt, firstSeenAt.Add(365*24*time.Hour)); got != 0.2 {
		t.Errorf("ageDecayFactor(age=365d) = %v, want the 0.2 floor (never 0)", got)
	}
}

// TestBurstFactor_HistoryRelative is R1: the SAME current burst reads as
// far less unusual for a subject with a substantial prior peak than for
// one with none.
func TestBurstFactor_HistoryRelative(t *testing.T) {
	noHistory := burstFactor(300, 0)
	establishedHistory := burstFactor(300, 40)
	if noHistory <= establishedHistory {
		t.Errorf("burstFactor(300, no history)=%v, burstFactor(300, established)=%v — a subject with real prior volume must read as LESS unusual", noHistory, establishedHistory)
	}
	if noHistory != 300 {
		t.Errorf("burstFactor(300, 0) = %v, want 300 (no history: baseline floors at 1, ratio = current)", noHistory)
	}
	if got := burstFactor(300, 40); math.Abs(got-7.5) > 1e-9 {
		t.Errorf("burstFactor(300, 40) = %v, want 7.5", got)
	}
}

// TestSends10mMax_TrailingWindowNotWholeHistory is R1: "Replace
// sends_10m_max's whole-history maximum with a trailing window, so a
// burst stops contributing once it leaves the window" — an old burst
// (more than currentBurstWindow ago) must no longer count toward the
// CURRENT search, even though it's still within the 30-day history
// lookback that feeds the baseline.
func TestSends10mMax_TrailingWindowNotWholeHistory(t *testing.T) {
	// A burst 2 days ago (well outside the 24h "current" window, but
	// inside the 30-day history lookback) followed by total silence.
	events := []event.Event{
		ev("c1", "content.sent", 0, map[string]any{"recipient_count": float64(200), "recipient_domain": "corp.example.test"}),
	}
	now := at(48 * time.Hour) // 2 days after the old burst
	got := sends10mMax(events, now, base)
	if got != 0 {
		t.Errorf("sends_10m_max = %v, want 0 (the only burst is outside the 24h current window)", got)
	}
}
