package worker

import (
	"context"
	"sync"
	"time"
)

// DefaultPerSubjectDailyBudget is design §4.8's "per subject daily cap
// (default 20 calls)".
const DefaultPerSubjectDailyBudget = 20

// reservedFraction is design §4.8's "25% of each adapter cap is reserved
// for subjects at medium or above".
const reservedFraction = 0.25

// BudgetStore is the persistence Budgets needs for its daily counters (S7
// fix round) — a narrow interface *store.Store satisfies automatically.
type BudgetStore interface {
	IncrementBudgetUsage(ctx context.Context, day, dim, key string) (int, error)
	GetBudgetUsage(ctx context.Context, day, dim, key string) (int, error)
}

// Budgets enforces design §4.8's per-adapter / per-subject / per-tenant
// daily call caps, reset by calendar day (UTC).
//
// v0 has no vendor adapters yet (S5), and the one registered scorer
// (local) is free and always answers regardless of budget (design: "the
// local rule is unaffected" / "the local rule keeps answering") —
// internal/worker never calls Allow/Record for a rule whose Scorer is
// "local". Budgets exists now so the worker's execute loop has the real
// enforcement point wired in before a paid adapter arrives; nothing today
// can actually exhaust it outside this package's own tests.
//
// Design §4.8 also calls for a per-producer cap; this implementation
// tracks per-TENANT instead (see NewBudgets' doc comment) since a subject
// isn't tied to one producer once claimed off the dirty queue.
//
// S7 fix round: NewBudgets' counters are in-memory only (reset by a
// process restart, and never shared across instances) — fine for a test
// or a genuinely single-instance deployment, but not what design §4.8
// actually needs in production. NewPersistedBudgets backs the identical
// Allow/Record API with store.Store's budget_usage table instead, so a
// restart or a second instance shares the same count.
//
// Safe for concurrent use.
type Budgets struct {
	perAdapterDaily int
	perSubjectDaily int
	perTenantDaily  int
	store           BudgetStore // nil: in-memory only. Non-nil: persisted (S7 fix round).

	mu          sync.Mutex
	day         string // UTC calendar day the in-memory counters below belong to, "" before first use. Unused when store != nil.
	adapterUsed map[string]int
	subjectUsed map[string]int
	tenantUsed  map[string]int
}

// NewBudgets returns a Budgets enforcing the given daily caps, counted
// in-memory only. A negative cap means "unlimited" for that dimension.
// perSubjectDaily == 0 specifically substitutes DefaultPerSubjectDailyBudget
// (design §4.8's stated default of 20) rather than being taken literally
// as "unlimited" — pass a negative perSubjectDaily if unlimited-per-subject
// is genuinely intended.
func NewBudgets(perAdapterDaily, perSubjectDaily, perTenantDaily int) *Budgets {
	return newBudgets(nil, perAdapterDaily, perSubjectDaily, perTenantDaily)
}

// NewPersistedBudgets is NewBudgets, backed by store's budget_usage table
// (S7 fix round) so counters survive a restart and are shared across
// every instance reading/writing the same database — the production
// configuration; NewBudgets remains available for a test, or a genuinely
// single-process deployment that doesn't need cross-restart persistence.
func NewPersistedBudgets(store BudgetStore, perAdapterDaily, perSubjectDaily, perTenantDaily int) *Budgets {
	if store == nil {
		panic("worker: NewPersistedBudgets requires a non-nil store")
	}
	return newBudgets(store, perAdapterDaily, perSubjectDaily, perTenantDaily)
}

func newBudgets(store BudgetStore, perAdapterDaily, perSubjectDaily, perTenantDaily int) *Budgets {
	if perSubjectDaily == 0 {
		perSubjectDaily = DefaultPerSubjectDailyBudget
	}
	return &Budgets{
		store:           store,
		perAdapterDaily: perAdapterDaily,
		perSubjectDaily: perSubjectDaily,
		perTenantDaily:  perTenantDaily,
		adapterUsed:     make(map[string]int),
		subjectUsed:     make(map[string]int),
		tenantUsed:      make(map[string]int),
	}
}

// Allow reports whether a call to adapter for (tenant, subject) is
// currently within budget, given whether the subject is currently
// elevated (tier medium or high — design's 25% reserve is available only
// to an elevated subject). On denial it also returns the error_code a
// caller should record on the affected signal ("cost_cap", design §4.8).
// err is non-nil only in persisted mode, when reading a counter fails —
// the caller (internal/worker) treats that as a whole-pass failure
// (B3 fix round's RecordSubjectFailure backoff), the safe direction when
// budget headroom can't be verified.
func (b *Budgets) Allow(ctx context.Context, adapter, tenant, subject string, elevated bool, now time.Time) (allowed bool, code string, err error) {
	day := now.UTC().Format("2006-01-02")
	adapterUsed, subjectUsed, tenantUsed, err := b.usage(ctx, day, adapter, tenant, subject)
	if err != nil {
		return false, "", err
	}

	if b.perAdapterDaily > 0 {
		limit := b.perAdapterDaily
		if !elevated {
			// A non-elevated subject can't dip into the reserved slice —
			// only an elevated one (tier medium/high) can use the last
			// reservedFraction of the adapter's daily cap.
			reserve := int(float64(b.perAdapterDaily) * reservedFraction)
			limit -= reserve
		}
		if adapterUsed >= limit {
			return false, "cost_cap", nil
		}
	}
	if b.perSubjectDaily > 0 && subjectUsed >= b.perSubjectDaily {
		return false, "cost_cap", nil
	}
	if b.perTenantDaily > 0 && tenantUsed >= b.perTenantDaily {
		return false, "cost_cap", nil
	}
	return true, "", nil
}

// Record accounts for one call to adapter for (tenant, subject) having
// actually been made. Callers must only call Record after a matching
// Allow returned true and the call was actually issued.
func (b *Budgets) Record(ctx context.Context, adapter, tenant, subject string, now time.Time) error {
	day := now.UTC().Format("2006-01-02")
	if b.store != nil {
		if _, err := b.store.IncrementBudgetUsage(ctx, day, "adapter", adapter); err != nil {
			return err
		}
		if _, err := b.store.IncrementBudgetUsage(ctx, day, "subject", subjectKey(tenant, subject)); err != nil {
			return err
		}
		if _, err := b.store.IncrementBudgetUsage(ctx, day, "tenant", tenant); err != nil {
			return err
		}
		return nil
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.resetIfNewDayLocked(day)
	b.adapterUsed[adapter]++
	b.subjectUsed[subjectKey(tenant, subject)]++
	b.tenantUsed[tenant]++
	return nil
}

// usage returns the current (adapterUsed, subjectUsed, tenantUsed) counts
// for day, from the store in persisted mode or the in-memory maps
// otherwise.
func (b *Budgets) usage(ctx context.Context, day, adapter, tenant, subject string) (adapterUsed, subjectUsed, tenantUsed int, err error) {
	if b.store != nil {
		adapterUsed, err = b.store.GetBudgetUsage(ctx, day, "adapter", adapter)
		if err != nil {
			return 0, 0, 0, err
		}
		subjectUsed, err = b.store.GetBudgetUsage(ctx, day, "subject", subjectKey(tenant, subject))
		if err != nil {
			return 0, 0, 0, err
		}
		tenantUsed, err = b.store.GetBudgetUsage(ctx, day, "tenant", tenant)
		if err != nil {
			return 0, 0, 0, err
		}
		return adapterUsed, subjectUsed, tenantUsed, nil
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.resetIfNewDayLocked(day)
	return b.adapterUsed[adapter], b.subjectUsed[subjectKey(tenant, subject)], b.tenantUsed[tenant], nil
}

// resetIfNewDayLocked clears every in-memory counter when day differs from
// the day they currently belong to. Caller must hold b.mu. A no-op in
// persisted mode (the store's own (day, dim, key) primary key is what
// makes a new day start at zero there).
func (b *Budgets) resetIfNewDayLocked(day string) {
	if day == b.day {
		return
	}
	b.day = day
	b.adapterUsed = make(map[string]int)
	b.subjectUsed = make(map[string]int)
	b.tenantUsed = make(map[string]int)
}

func subjectKey(tenant, subject string) string { return tenant + "/" + subject }
