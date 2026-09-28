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

// TestNormalizeResourceKind is [S2b]'s producer-contract test: one case per
// documented alias, proving each folds to its canonical kind — a real
// phishing campaign scored low in part because a producer's own "api_key"
// spelling was never recognized as resourceKindKey ("key") at all.
func TestNormalizeResourceKind(t *testing.T) {
	tests := []struct{ in, want string }{
		{"key", "key"},
		{"api_key", "key"},
		{"apikey", "key"},
		{"api-key", "key"},
		{"Key", "key"},
		{" API_KEY ", "key"},
		{"agent", "agent"},
		{"mailbox", "agent"},
		{"inbox", "agent"},
		{"Mailbox", "agent"},
		{" INBOX ", "agent"},
		{"webhook", "webhook"}, // an unrecognized kind passes through normalizeToken unchanged, not rejected
		{"", ""},
	}
	for _, tt := range tests {
		if got := normalizeResourceKind(tt.in); got != tt.want {
			t.Errorf("normalizeResourceKind(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestResourceCount_KindAliases proves resourceCount itself (not just the
// normalization helper in isolation) recognizes every alias when filtering
// by the canonical "key" kind.
func TestResourceCount_KindAliases(t *testing.T) {
	events := []event.Event{
		ev("r1", "resource.created", 0, map[string]any{"kind": "key"}),
		ev("r2", "resource.created", 0, map[string]any{"kind": "api_key"}),
		ev("r3", "resource.created", 0, map[string]any{"kind": "apikey"}),
		ev("r4", "resource.created", 0, map[string]any{"kind": "api-key"}),
		ev("r5", "resource.created", 0, map[string]any{"kind": "agent"}), // not a key alias: excluded
	}
	now := at(time.Minute)
	if got := resourceCount(events, "key", now, 0); got != 4 {
		t.Errorf("key_total with every key alias = %v, want 4", got)
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
		// Not "PayPal Bot" (R6 round 2): "bot" is now an integration-token
		// that suppresses a match on its own (BrandSet.Matches) — this test
		// is about resource.deleted being checked at all, not about that
		// gate, so it uses a name the gate leaves alone.
		events := []event.Event{ev("r1", "resource.deleted", 0, map[string]any{"name": "PayPal Alert"})}
		if got := nameBrandMatch(events, brands); got != 1 {
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

// --- [S2b]: send-volume, webmail, recipient and subject-brand features ---

// sendEvent builds one content.sent event with the given optional fields;
// zero/empty values are simply omitted from data (matching how a producer
// that doesn't set an optional field would emit it).
func sendEvent(id string, offset time.Duration, recipientCount float64, recipientDomain, recipientHash, subjectLine string) event.Event {
	data := map[string]any{}
	if recipientCount != 0 {
		data["recipient_count"] = recipientCount
	}
	if recipientDomain != "" {
		data["recipient_domain"] = recipientDomain
	}
	if recipientHash != "" {
		data["recipient_hash"] = recipientHash
	}
	if subjectLine != "" {
		data["subject_line"] = subjectLine
	}
	return ev(id, "content.sent", offset, data)
}

func TestRecipientCountOf(t *testing.T) {
	tests := []struct {
		name string
		data map[string]any
		want float64
	}{
		{"present and positive", map[string]any{"recipient_count": 5.0}, 5},
		{"absent falls back to 1", nil, 1},
		{"zero falls back to 1", map[string]any{"recipient_count": 0.0}, 1},
		{"negative falls back to 1", map[string]any{"recipient_count": -3.0}, 1},
		{"wrong type falls back to 1", map[string]any{"recipient_count": "5"}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := ev("c1", "content.sent", 0, tt.data)
			if got := recipientCountOf(e); got != tt.want {
				t.Errorf("recipientCountOf(%+v) = %v, want %v", tt.data, got, tt.want)
			}
		})
	}
}

func TestSends1h(t *testing.T) {
	window := time.Hour
	t.Run("sums recipient_count within the trailing window", func(t *testing.T) {
		events := []event.Event{
			sendEvent("c1", 0, 10, "a.example.test", "", ""),
			sendEvent("c2", 30*time.Minute, 5, "b.example.test", "", ""),
		}
		now := at(45 * time.Minute)
		want := 15.0
		if got := sends1h(events, now, window); got != want {
			t.Errorf("sends1h = %v, want %v", got, want)
		}
	})
	t.Run("falls back to 1 recipient when recipient_count is absent", func(t *testing.T) {
		events := []event.Event{sendEvent("c1", 0, 0, "a.example.test", "", "")}
		want := 1.0
		if got := sends1h(events, at(time.Minute), window); got != want {
			t.Errorf("sends1h = %v, want %v", got, want)
		}
	})
	t.Run("window boundary: exactly window-old is excluded", func(t *testing.T) {
		now := at(time.Hour)
		events := []event.Event{sendEvent("c1", 0, 10, "", "", "")} // exactly `now - window`
		if got := sends1h(events, now, window); got != 0 {
			t.Errorf("sends1h at the boundary = %v, want 0 (just expired)", got)
		}
	})
	t.Run("out-of-order delivery doesn't affect the sum", func(t *testing.T) {
		events := []event.Event{
			sendEvent("c2", 30*time.Minute, 5, "", "", ""),
			sendEvent("c1", 0, 10, "", "", ""), // chronologically earlier, delivered second
		}
		now := at(45 * time.Minute)
		want := 15.0
		if got := sends1h(events, now, window); got != want {
			t.Errorf("sends1h (out of order) = %v, want %v", got, want)
		}
	})
	t.Run("empty history is exactly zero", func(t *testing.T) {
		if got := sends1h(nil, at(0), window); got != 0 {
			t.Errorf("sends1h(nil) = %v, want 0", got)
		}
	})
	t.Run("non-content.sent events are ignored", func(t *testing.T) {
		events := []event.Event{ev("r1", "resource.created", 0, map[string]any{"kind": "agent"})}
		if got := sends1h(events, at(time.Minute), window); got != 0 {
			t.Errorf("sends1h = %v, want 0 (only content.sent counts)", got)
		}
	})
	t.Run("capped at sendsVolumeCap so a pathological volume can't swamp the model", func(t *testing.T) {
		events := []event.Event{sendEvent("c1", 0, sendsVolumeCap+500, "", "", "")}
		if got := sends1h(events, at(time.Minute), window); got != sendsVolumeCap {
			t.Errorf("sends1h = %v, want the cap %v", got, sendsVolumeCap)
		}
	})
}

func TestSendsFirstDay(t *testing.T) {
	day := 24 * time.Hour
	t.Run("sums recipient_count within the first day, boundary inclusive", func(t *testing.T) {
		events := []event.Event{
			sendEvent("c1", 0, 10, "", "", ""),
			sendEvent("c2", time.Hour, 5, "", "", ""),
			sendEvent("c3", day, 3, "", "", ""),               // exactly at the cutover: included
			sendEvent("c4", day+time.Second, 100, "", "", ""), // just past: excluded
		}
		want := 18.0
		if got := sendsFirstDay(events, base, day); got != want {
			t.Errorf("sendsFirstDay = %v, want %v", got, want)
		}
	})
	t.Run("sends after the first day don't count at all", func(t *testing.T) {
		events := []event.Event{sendEvent("c1", day+time.Hour, 50, "", "", "")}
		if got := sendsFirstDay(events, base, day); got != 0 {
			t.Errorf("sendsFirstDay = %v, want 0", got)
		}
	})
	t.Run("out-of-order delivery doesn't affect the sum", func(t *testing.T) {
		events := []event.Event{
			sendEvent("c2", time.Hour, 5, "", "", ""),
			sendEvent("c1", 0, 10, "", "", ""),
		}
		want := 15.0
		if got := sendsFirstDay(events, base, day); got != want {
			t.Errorf("sendsFirstDay (out of order) = %v, want %v", got, want)
		}
	})
	t.Run("no sends at all is exactly zero", func(t *testing.T) {
		if got := sendsFirstDay(nil, base, day); got != 0 {
			t.Errorf("sendsFirstDay(nil) = %v, want 0", got)
		}
	})
	t.Run("capped at sendsVolumeCap so a pathological volume can't swamp the model", func(t *testing.T) {
		events := []event.Event{sendEvent("c1", 0, sendsVolumeCap+500, "", "", "")}
		if got := sendsFirstDay(events, base, day); got != sendsVolumeCap {
			t.Errorf("sendsFirstDay = %v, want the cap %v", got, sendsVolumeCap)
		}
	})
}

func TestSends10mMax(t *testing.T) {
	window := 10 * time.Minute
	t.Run("finds the largest sum among several 10-minute windows", func(t *testing.T) {
		events := []event.Event{
			// A dense cluster of 100 in under 10 minutes...
			sendEvent("c1", 0, 40, "", "", ""),
			sendEvent("c2", 3*time.Minute, 30, "", "", ""),
			sendEvent("c3", 6*time.Minute, 30, "", "", ""),
			// ...then an isolated, much smaller send an hour later.
			sendEvent("c4", time.Hour, 5, "", "", ""),
		}
		want := 100.0
		if got := sends10mMax(events, window); got != want {
			t.Errorf("sends10mMax = %v, want %v (the dense cluster, not the later isolated send)", got, want)
		}
	})
	t.Run("boundary: exactly window-old falls out of a given window", func(t *testing.T) {
		events := []event.Event{
			sendEvent("c1", 0, 10, "", "", ""),
			sendEvent("c2", window, 20, "", "", ""), // exactly 10m after c1: c1 has just expired relative to c2
		}
		// The max single-event sum (20) beats any window containing both,
		// since c1 is not within 10m of c2 (boundary excluded, withinWindow's
		// own half-open convention).
		want := 20.0
		if got := sends10mMax(events, window); got != want {
			t.Errorf("sends10mMax at the boundary = %v, want %v", got, want)
		}
	})
	t.Run("out-of-order delivery still finds the true maximum", func(t *testing.T) {
		events := []event.Event{
			sendEvent("c3", 6*time.Minute, 30, "", "", ""),
			sendEvent("c1", 0, 40, "", "", ""),
			sendEvent("c2", 3*time.Minute, 30, "", "", ""),
		}
		want := 100.0
		if got := sends10mMax(events, window); got != want {
			t.Errorf("sends10mMax (out of order) = %v, want %v", got, want)
		}
	})
	t.Run("empty history is exactly zero", func(t *testing.T) {
		if got := sends10mMax(nil, window); got != 0 {
			t.Errorf("sends10mMax(nil) = %v, want 0", got)
		}
	})
	t.Run("a single event's own count is its own max", func(t *testing.T) {
		events := []event.Event{sendEvent("c1", 0, 7, "", "", "")}
		want := 7.0
		if got := sends10mMax(events, window); got != want {
			t.Errorf("sends10mMax = %v, want %v", got, want)
		}
	})
	t.Run("does not decay with time passing (a historical max, not a trailing window)", func(t *testing.T) {
		// sends10mMax has no "now" parameter at all -- it is computed purely
		// from event history, so this is really just documentation that its
		// signature has no way to make it decay; see its own doc comment.
		events := []event.Event{sendEvent("c1", 0, 100, "", "", "")}
		want := 100.0
		if got := sends10mMax(events, window); got != want {
			t.Errorf("sends10mMax = %v, want %v", got, want)
		}
	})
	t.Run("capped at sendsVolumeCap so a pathological volume can't swamp the model", func(t *testing.T) {
		events := []event.Event{sendEvent("c1", 0, sendsVolumeCap+500, "", "", "")}
		if got := sends10mMax(events, window); got != sendsVolumeCap {
			t.Errorf("sends10mMax = %v, want the cap %v", got, sendsVolumeCap)
		}
	})
}

func TestDistinctRecipients1h(t *testing.T) {
	window := time.Hour
	t.Run("dedupes by recipient_hash", func(t *testing.T) {
		events := []event.Event{
			sendEvent("c1", 0, 1, "", "hash-a", ""),
			sendEvent("c2", time.Minute, 1, "", "hash-a", ""), // same recipient again
			sendEvent("c3", 2*time.Minute, 1, "", "hash-b", ""),
		}
		want := 2.0
		if got := distinctRecipients1h(events, at(3*time.Minute), window); got != want {
			t.Errorf("distinctRecipients1h = %v, want %v (2 distinct hashes)", got, want)
		}
	})
	t.Run("falls back to adding recipient_count when recipient_hash is absent", func(t *testing.T) {
		events := []event.Event{
			sendEvent("c1", 0, 1, "", "hash-a", ""),
			sendEvent("c2", time.Minute, 5, "", "", ""), // no hash: adds 5, doesn't just count as 1
		}
		want := 1.0 + 5.0
		if got := distinctRecipients1h(events, at(2*time.Minute), window); got != want {
			t.Errorf("distinctRecipients1h = %v, want %v", got, want)
		}
	})
	t.Run("window boundary: exactly window-old is excluded", func(t *testing.T) {
		now := at(time.Hour)
		events := []event.Event{sendEvent("c1", 0, 1, "", "hash-a", "")}
		if got := distinctRecipients1h(events, now, window); got != 0 {
			t.Errorf("distinctRecipients1h at the boundary = %v, want 0", got)
		}
	})
	t.Run("out-of-order delivery doesn't affect the count", func(t *testing.T) {
		events := []event.Event{
			sendEvent("c2", time.Minute, 1, "", "hash-b", ""),
			sendEvent("c1", 0, 1, "", "hash-a", ""),
		}
		want := 2.0
		if got := distinctRecipients1h(events, at(2*time.Minute), window); got != want {
			t.Errorf("distinctRecipients1h (out of order) = %v, want %v", got, want)
		}
	})
	t.Run("empty history is exactly zero", func(t *testing.T) {
		if got := distinctRecipients1h(nil, at(0), window); got != 0 {
			t.Errorf("distinctRecipients1h(nil) = %v, want 0", got)
		}
	})
	t.Run("capped at sendsVolumeCap so a pathological volume can't swamp the model", func(t *testing.T) {
		events := []event.Event{sendEvent("c1", 0, sendsVolumeCap+500, "", "", "")} // no hash: falls back to the raw count
		if got := distinctRecipients1h(events, at(time.Minute), window); got != sendsVolumeCap {
			t.Errorf("distinctRecipients1h = %v, want the cap %v", got, sendsVolumeCap)
		}
	})
}

func TestWebmailRecipientShare(t *testing.T) {
	webmail := NewWebmailSet([]string{"gmail.com", "yahoo.com"})
	t.Run("mixed webmail and non-webmail recipients", func(t *testing.T) {
		events := []event.Event{
			sendEvent("c1", 0, 10, "gmail.com", "", ""),
			sendEvent("c2", time.Minute, 5, "corp.example.test", "", ""),
			sendEvent("c3", 2*time.Minute, 5, "Yahoo.COM", "", ""), // case-insensitive
		}
		want := 15.0 / 20.0
		if got := webmailRecipientShare(events, webmail); math.Abs(got-want) > 1e-9 {
			t.Errorf("webmailRecipientShare = %v, want %v", got, want)
		}
	})
	t.Run("no webmail recipients at all", func(t *testing.T) {
		events := []event.Event{sendEvent("c1", 0, 10, "corp.example.test", "", "")}
		if got := webmailRecipientShare(events, webmail); got != 0 {
			t.Errorf("webmailRecipientShare = %v, want 0", got)
		}
	})
	t.Run("no sends at all: no division by zero", func(t *testing.T) {
		if got := webmailRecipientShare(nil, webmail); got != 0 {
			t.Errorf("webmailRecipientShare(nil) = %v, want 0", got)
		}
	})
	t.Run("zero-value WebmailSet matches nothing", func(t *testing.T) {
		events := []event.Event{sendEvent("c1", 0, 10, "gmail.com", "", "")}
		if got := webmailRecipientShare(events, WebmailSet{}); got != 0 {
			t.Errorf("webmailRecipientShare with an unloaded WebmailSet = %v, want 0", got)
		}
	})
}

func TestExtract_WebmailSendsIsShareTimesSends1h(t *testing.T) {
	webmail := NewWebmailSet([]string{"gmail.com"})
	events := []event.Event{
		sendEvent("c1", 0, 10, "gmail.com", "", ""),
		sendEvent("c2", time.Minute, 10, "corp.example.test", "", ""),
	}
	res, err := Extract(context.Background(), "e2a", "acct_test", events, nil, defaultWindows(2*time.Minute), BrandSet{}, webmail)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	f := res.Features
	wantShare := 0.5
	if math.Abs(f.WebmailRecipientShare-wantShare) > 1e-9 {
		t.Fatalf("WebmailRecipientShare = %v, want %v", f.WebmailRecipientShare, wantShare)
	}
	want := f.Sends1h * wantShare
	if math.Abs(f.WebmailSends1h-want) > 1e-9 {
		t.Errorf("WebmailSends1h = %v, want Sends1h(%v) * share(%v) = %v", f.WebmailSends1h, f.Sends1h, wantShare, want)
	}
}

func TestSubjectBrandMatch(t *testing.T) {
	brands := smallTestBrands() // PayPal (+ alias "Pay Pal")
	window := 24 * time.Hour
	t.Run("counts one distinct brand even matched via multiple sends/aliases", func(t *testing.T) {
		events := []event.Event{
			sendEvent("c1", 0, 1, "", "", "PayPal Account Alert"),
			sendEvent("c2", time.Minute, 1, "", "", "Pay Pal Security Notice"), // same brand, via its alias
		}
		if got := subjectBrandMatch(events, at(2*time.Minute), window, brands); got != 1 {
			t.Errorf("subjectBrandMatch = %v, want 1", got)
		}
	})
	t.Run("caps at 3 distinct brands", func(t *testing.T) {
		many := NewBrandSet([]BrandEntry{{Name: "Alpha"}, {Name: "Bravo"}, {Name: "Charlie"}, {Name: "Delta"}})
		events := []event.Event{
			sendEvent("c1", 0, 1, "", "", "Alpha Verify"),
			sendEvent("c2", time.Minute, 1, "", "", "Bravo Verify"),
			sendEvent("c3", 2*time.Minute, 1, "", "", "Charlie Verify"),
			sendEvent("c4", 3*time.Minute, 1, "", "", "Delta Verify"),
		}
		if got := subjectBrandMatch(events, at(4*time.Minute), window, many); got != subjectBrandMatchCap {
			t.Errorf("subjectBrandMatch = %v, want the cap %v", got, subjectBrandMatchCap)
		}
	})
	t.Run("no match at all", func(t *testing.T) {
		events := []event.Event{sendEvent("c1", 0, 1, "", "", "Your weekly digest")}
		if got := subjectBrandMatch(events, at(time.Minute), window, brands); got != 0 {
			t.Errorf("subjectBrandMatch = %v, want 0", got)
		}
	})
	t.Run("the integration-token gate still applies", func(t *testing.T) {
		events := []event.Event{sendEvent("c1", 0, 1, "", "", "PayPal integration webhook")}
		if got := subjectBrandMatch(events, at(time.Minute), window, brands); got != 0 {
			t.Errorf("subjectBrandMatch = %v, want 0 (integration-token gate)", got)
		}
	})
	t.Run("window boundary: exactly window-old is excluded", func(t *testing.T) {
		now := at(window)
		events := []event.Event{sendEvent("c1", 0, 1, "", "", "PayPal Account Alert")}
		if got := subjectBrandMatch(events, now, window, brands); got != 0 {
			t.Errorf("subjectBrandMatch at the boundary = %v, want 0", got)
		}
	})
	t.Run("out-of-order delivery doesn't affect the distinct count", func(t *testing.T) {
		events := []event.Event{
			sendEvent("c2", time.Minute, 1, "", "", "PayPal Account Alert"),
			sendEvent("c1", 0, 1, "", "", "PayPal Account Alert"),
		}
		if got := subjectBrandMatch(events, at(2*time.Minute), window, brands); got != 1 {
			t.Errorf("subjectBrandMatch (out of order) = %v, want 1", got)
		}
	})
	t.Run("empty history is exactly zero", func(t *testing.T) {
		if got := subjectBrandMatch(nil, at(0), window, brands); got != 0 {
			t.Errorf("subjectBrandMatch(nil) = %v, want 0", got)
		}
	})
	t.Run("empty BrandSet never matches", func(t *testing.T) {
		events := []event.Event{sendEvent("c1", 0, 1, "", "", "PayPal Account Alert")}
		if got := subjectBrandMatch(events, at(time.Minute), window, BrandSet{}); got != 0 {
			t.Errorf("subjectBrandMatch with an empty BrandSet = %v, want 0", got)
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

// TestLoadBrandsFile_S2bMarketplaceSocialShippingBrands proves the [S2b]
// batch of marketplace/social/shipping brands (added alongside the
// send-volume/webmail/subject-line features — a real phishing campaign
// impersonated exactly this class of brand, missing from the pre-[S2b]
// list) loaded from the real shipped config/brands.yaml.
func TestLoadBrandsFile_S2bMarketplaceSocialShippingBrands(t *testing.T) {
	brands, err := LoadBrandsFile(filepath.Join(repoRoot(t), "config", "brands.yaml"))
	if err != nil {
		t.Fatalf("LoadBrandsFile: %v", err)
	}

	mustMatch := []string{
		"Your TikTok account has been flagged",
		"Confirm your Poshmark listing",
		"Vinted payment issue",
		"Depop order update",
		"Mercari shipping label",
		"Your Etsy shop needs attention",
		"eBay account suspended",
		"Facebook Security Alert",
		"Instagram Copyright Notice",
		"WhatsApp Verification Code",
		"Booking.com reservation confirmed",
		"Airbnb host payout issue",
		"UPS delivery exception",
		"Royal Mail parcel held",
		"DPD missed delivery",
		"Evri parcel update",
		"PostNL delivery notice",
		"Post NL delivery notice",
		"Canada Post customs fee",
		"Australia Post delivery notice",
	}
	for _, name := range mustMatch {
		if !brands.Matches(name) {
			t.Errorf("brands.Matches(%q) = false, want true", name)
		}
	}

	// Shopify is on the list, but "Your Etsy order #123 shipped" sent
	// through an agent literally NAMED "Shopify Integration" must not read
	// as an impersonation attempt on the name side — the integration-token
	// gate already covers this generically (documented tradeoff on
	// "tracking"/"DHL Package Tracking" above), proven here for the new
	// brand specifically.
	mustNotMatch := []string{
		"Shopify Order Sync",
		"Shopify Webhook Relay",
		"Etsy API Integration",
	}
	for _, name := range mustNotMatch {
		if brands.Matches(name) {
			t.Errorf("brands.Matches(%q) = true, want false (integration-token gate)", name)
		}
	}
}
