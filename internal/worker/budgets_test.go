package worker

import (
	"context"
	"testing"
	"time"
)

// mustAllow/mustRecord wrap Budgets.Allow/Record with context.Background()
// and fail the test on the (persisted-mode-only) error return, so the
// in-memory-mode tests below — which never error — stay uncluttered.
func mustAllow(t *testing.T, b *Budgets, adapter, tenant, subject string, elevated bool, now time.Time) (bool, string) {
	t.Helper()
	allowed, code, err := b.Allow(context.Background(), adapter, tenant, subject, elevated, now)
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}
	return allowed, code
}

func mustRecord(t *testing.T, b *Budgets, adapter, tenant, subject string, now time.Time) {
	t.Helper()
	if err := b.Record(context.Background(), adapter, tenant, subject, now); err != nil {
		t.Fatalf("Record: %v", err)
	}
}

func TestBudgets_PerAdapterCap(t *testing.T) {
	b := NewBudgets(2, -1, -1)
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)

	for i := 0; i < 2; i++ {
		if allowed, _ := mustAllow(t, b, "vendor", "e2a", "acct_1", false, now); !allowed {
			t.Fatalf("call %d: expected allowed", i)
		}
		mustRecord(t, b, "vendor", "e2a", "acct_1", now)
	}
	allowed, code := mustAllow(t, b, "vendor", "e2a", "acct_1", false, now)
	if allowed {
		t.Fatalf("3rd call: expected denied once the adapter cap (2) is reached")
	}
	if code != "cost_cap" {
		t.Errorf("code = %q, want cost_cap", code)
	}
}

func TestBudgets_PerSubjectCap(t *testing.T) {
	b := NewBudgets(-1, 2, -1)
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)

	mustRecord(t, b, "vendor", "e2a", "acct_1", now)
	mustRecord(t, b, "vendor", "e2a", "acct_1", now)
	if allowed, _ := mustAllow(t, b, "vendor", "e2a", "acct_1", false, now); allowed {
		t.Fatalf("expected acct_1 denied at its per-subject cap")
	}
	// A different subject has its own, independent counter.
	if allowed, _ := mustAllow(t, b, "vendor", "e2a", "acct_2", false, now); !allowed {
		t.Fatalf("expected acct_2 allowed (independent per-subject counter)")
	}
}

func TestBudgets_PerSubjectDefaultsTo20(t *testing.T) {
	b := NewBudgets(-1, 0, -1)
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < DefaultPerSubjectDailyBudget; i++ {
		if allowed, _ := mustAllow(t, b, "vendor", "e2a", "acct_1", false, now); !allowed {
			t.Fatalf("call %d: expected allowed (under the default cap of %d)", i, DefaultPerSubjectDailyBudget)
		}
		mustRecord(t, b, "vendor", "e2a", "acct_1", now)
	}
	if allowed, _ := mustAllow(t, b, "vendor", "e2a", "acct_1", false, now); allowed {
		t.Fatalf("expected denied at the default per-subject cap of %d", DefaultPerSubjectDailyBudget)
	}
}

func TestBudgets_PerTenantCap(t *testing.T) {
	b := NewBudgets(-1, -1, 1)
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	mustRecord(t, b, "vendor", "e2a", "acct_1", now)
	if allowed, _ := mustAllow(t, b, "vendor", "e2a", "acct_2", false, now); allowed {
		t.Fatalf("expected acct_2 denied: the tenant-wide cap is shared across subjects")
	}
}

func TestBudgets_ReservedHeadroomForElevatedSubjects(t *testing.T) {
	// A cap of 4 reserves 25% (1 call) for elevated subjects. A
	// non-elevated caller can use at most 3; the 4th is reserved.
	b := NewBudgets(4, -1, -1)
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)

	for i := 0; i < 3; i++ {
		if allowed, _ := mustAllow(t, b, "vendor", "e2a", "acct_1", false, now); !allowed {
			t.Fatalf("non-elevated call %d: expected allowed", i)
		}
		mustRecord(t, b, "vendor", "e2a", "acct_1", now)
	}
	if allowed, _ := mustAllow(t, b, "vendor", "e2a", "acct_1", false, now); allowed {
		t.Fatalf("4th non-elevated call: expected denied (reserved headroom)")
	}
	if allowed, _ := mustAllow(t, b, "vendor", "e2a", "acct_2", true, now); !allowed {
		t.Fatalf("elevated call: expected allowed to dip into the reserved headroom")
	}
}

func TestBudgets_ResetsOnNewUTCDay(t *testing.T) {
	b := NewBudgets(1, -1, -1)
	day1 := time.Date(2031, 1, 1, 23, 59, 0, 0, time.UTC)
	day2 := time.Date(2031, 1, 2, 0, 0, 1, 0, time.UTC)

	mustRecord(t, b, "vendor", "e2a", "acct_1", day1)
	if allowed, _ := mustAllow(t, b, "vendor", "e2a", "acct_1", false, day1); allowed {
		t.Fatalf("expected denied: at the cap for day1")
	}
	if allowed, _ := mustAllow(t, b, "vendor", "e2a", "acct_1", false, day2); !allowed {
		t.Fatalf("expected allowed: day2's counters should have reset")
	}
}

func TestBudgets_UnlimitedByDefault(t *testing.T) {
	b := NewBudgets(-1, -1, -1)
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 1000; i++ {
		if allowed, _ := mustAllow(t, b, "vendor", "e2a", "acct_1", false, now); !allowed {
			t.Fatalf("call %d: expected allowed (every dimension unlimited)", i)
		}
		mustRecord(t, b, "vendor", "e2a", "acct_1", now)
	}
}

func TestNewPersistedBudgets_PanicsOnNilStore(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatalf("expected NewPersistedBudgets(nil, ...) to panic")
		}
	}()
	NewPersistedBudgets(nil, 1, 1, 1)
}

func TestPersistedBudgets_SharedAcrossInstances(t *testing.T) {
	// S7 fix round: a restart or a second worker instance must share the
	// same daily counters, not each keep (and silently reset) its own.
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)

	instanceA := NewPersistedBudgets(s, 2, -1, -1)
	instanceB := NewPersistedBudgets(s, 2, -1, -1) // e.g. a second process, or the same one after a restart

	allowed, _, err := instanceA.Allow(ctx, "vendor", "e2a", "acct_1", false, now)
	if err != nil {
		t.Fatalf("Allow (A, call 1): %v", err)
	}
	if !allowed {
		t.Fatalf("Allow (A, call 1): expected allowed")
	}
	if err := instanceA.Record(ctx, "vendor", "e2a", "acct_1", now); err != nil {
		t.Fatalf("Record (A, call 1): %v", err)
	}

	allowed, _, err = instanceB.Allow(ctx, "vendor", "e2a", "acct_1", false, now)
	if err != nil {
		t.Fatalf("Allow (B, call 2): %v", err)
	}
	if !allowed {
		t.Fatalf("Allow (B, call 2): expected allowed")
	}
	if err := instanceB.Record(ctx, "vendor", "e2a", "acct_1", now); err != nil {
		t.Fatalf("Record (B, call 2): %v", err)
	}

	// The cap is 2, and two calls have now been recorded ACROSS the two
	// instances — a third call, from EITHER instance, must be denied.
	allowedA, _, err := instanceA.Allow(ctx, "vendor", "e2a", "acct_1", false, now)
	if err != nil {
		t.Fatalf("Allow (A, call 3): %v", err)
	}
	if allowedA {
		t.Fatalf("instance A: expected denied — the shared cap of 2 was already reached by instance B's call")
	}
	allowedB, _, err := instanceB.Allow(ctx, "vendor", "e2a", "acct_1", false, now)
	if err != nil {
		t.Fatalf("Allow (B, call 3): %v", err)
	}
	if allowedB {
		t.Fatalf("instance B: expected denied")
	}
}
