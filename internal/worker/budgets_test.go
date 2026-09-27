package worker

import (
	"testing"
	"time"
)

func TestBudgets_PerAdapterCap(t *testing.T) {
	b := NewBudgets(2, -1, -1)
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)

	for i := 0; i < 2; i++ {
		if allowed, _ := b.Allow("vendor", "e2a", "acct_1", false, now); !allowed {
			t.Fatalf("call %d: expected allowed", i)
		}
		b.Record("vendor", "e2a", "acct_1", now)
	}
	allowed, code := b.Allow("vendor", "e2a", "acct_1", false, now)
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

	b.Record("vendor", "e2a", "acct_1", now)
	b.Record("vendor", "e2a", "acct_1", now)
	if allowed, _ := b.Allow("vendor", "e2a", "acct_1", false, now); allowed {
		t.Fatalf("expected acct_1 denied at its per-subject cap")
	}
	// A different subject has its own, independent counter.
	if allowed, _ := b.Allow("vendor", "e2a", "acct_2", false, now); !allowed {
		t.Fatalf("expected acct_2 allowed (independent per-subject counter)")
	}
}

func TestBudgets_PerSubjectDefaultsTo20(t *testing.T) {
	b := NewBudgets(-1, 0, -1)
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < DefaultPerSubjectDailyBudget; i++ {
		if allowed, _ := b.Allow("vendor", "e2a", "acct_1", false, now); !allowed {
			t.Fatalf("call %d: expected allowed (under the default cap of %d)", i, DefaultPerSubjectDailyBudget)
		}
		b.Record("vendor", "e2a", "acct_1", now)
	}
	if allowed, _ := b.Allow("vendor", "e2a", "acct_1", false, now); allowed {
		t.Fatalf("expected denied at the default per-subject cap of %d", DefaultPerSubjectDailyBudget)
	}
}

func TestBudgets_PerTenantCap(t *testing.T) {
	b := NewBudgets(-1, -1, 1)
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	b.Record("vendor", "e2a", "acct_1", now)
	if allowed, _ := b.Allow("vendor", "e2a", "acct_2", false, now); allowed {
		t.Fatalf("expected acct_2 denied: the tenant-wide cap is shared across subjects")
	}
}

func TestBudgets_ReservedHeadroomForElevatedSubjects(t *testing.T) {
	// A cap of 4 reserves 25% (1 call) for elevated subjects. A
	// non-elevated caller can use at most 3; the 4th is reserved.
	b := NewBudgets(4, -1, -1)
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)

	for i := 0; i < 3; i++ {
		if allowed, _ := b.Allow("vendor", "e2a", "acct_1", false, now); !allowed {
			t.Fatalf("non-elevated call %d: expected allowed", i)
		}
		b.Record("vendor", "e2a", "acct_1", now)
	}
	if allowed, _ := b.Allow("vendor", "e2a", "acct_1", false, now); allowed {
		t.Fatalf("4th non-elevated call: expected denied (reserved headroom)")
	}
	if allowed, _ := b.Allow("vendor", "e2a", "acct_2", true, now); !allowed {
		t.Fatalf("elevated call: expected allowed to dip into the reserved headroom")
	}
}

func TestBudgets_ResetsOnNewUTCDay(t *testing.T) {
	b := NewBudgets(1, -1, -1)
	day1 := time.Date(2031, 1, 1, 23, 59, 0, 0, time.UTC)
	day2 := time.Date(2031, 1, 2, 0, 0, 1, 0, time.UTC)

	b.Record("vendor", "e2a", "acct_1", day1)
	if allowed, _ := b.Allow("vendor", "e2a", "acct_1", false, day1); allowed {
		t.Fatalf("expected denied: at the cap for day1")
	}
	if allowed, _ := b.Allow("vendor", "e2a", "acct_1", false, day2); !allowed {
		t.Fatalf("expected allowed: day2's counters should have reset")
	}
}

func TestBudgets_UnlimitedByDefault(t *testing.T) {
	b := NewBudgets(-1, -1, -1)
	now := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 1000; i++ {
		if allowed, _ := b.Allow("vendor", "e2a", "acct_1", false, now); !allowed {
			t.Fatalf("call %d: expected allowed (every dimension unlimited)", i)
		}
		b.Record("vendor", "e2a", "acct_1", now)
	}
}
