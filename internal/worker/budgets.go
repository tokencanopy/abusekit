package worker

import (
	"sync"
	"time"
)

// DefaultPerSubjectDailyBudget is design §4.8's "per subject daily cap
// (default 20 calls)".
const DefaultPerSubjectDailyBudget = 20

// reservedFraction is design §4.8's "25% of each adapter cap is reserved
// for subjects at medium or above".
const reservedFraction = 0.25

// Budgets enforces design §4.8's per-adapter / per-subject / per-tenant
// daily call caps, in memory, reset by calendar day (UTC).
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
// tracks per-TENANT instead (see New's doc comment) since a subject isn't
// tied to one producer once claimed off the dirty queue.
//
// Safe for concurrent use.
type Budgets struct {
	perAdapterDaily int
	perSubjectDaily int
	perTenantDaily  int

	mu          sync.Mutex
	day         string // UTC calendar day the counters below belong to, "" before first use
	adapterUsed map[string]int
	subjectUsed map[string]int
	tenantUsed  map[string]int
}

// NewBudgets returns a Budgets enforcing the given daily caps. A negative
// cap means "unlimited" for that dimension. perSubjectDaily == 0
// specifically substitutes DefaultPerSubjectDailyBudget (design §4.8's
// stated default of 20) rather than being taken literally as "unlimited" —
// pass a negative perSubjectDaily if unlimited-per-subject is genuinely
// intended.
//
// design §4.8 calls for a per-PRODUCER cap; the worker only knows a
// subject's tenant by the time it's deciding whether to call a scorer (a
// subject isn't tied to a single producer once it's a row in the dirty
// queue — see internal/store.DirtySubject), so this tracks per-TENANT
// instead. Re-introducing true per-producer accounting would need the
// worker to also carry producer identity through EventsForSubject/
// ClaimDirtySubjects, which S2's replay-fixture scope doesn't need.
func NewBudgets(perAdapterDaily, perSubjectDaily, perTenantDaily int) *Budgets {
	if perSubjectDaily == 0 {
		perSubjectDaily = DefaultPerSubjectDailyBudget
	}
	return &Budgets{
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
func (b *Budgets) Allow(adapter, tenant, subject string, elevated bool, now time.Time) (allowed bool, code string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.resetIfNewDayLocked(now)

	if b.perAdapterDaily > 0 {
		limit := b.perAdapterDaily
		if !elevated {
			// A non-elevated subject can't dip into the reserved slice —
			// only an elevated one (tier medium/high) can use the last
			// reservedFraction of the adapter's daily cap.
			reserve := int(float64(b.perAdapterDaily) * reservedFraction)
			limit -= reserve
		}
		if b.adapterUsed[adapter] >= limit {
			return false, "cost_cap"
		}
	}
	if b.perSubjectDaily > 0 && b.subjectUsed[subjectKey(tenant, subject)] >= b.perSubjectDaily {
		return false, "cost_cap"
	}
	if b.perTenantDaily > 0 && b.tenantUsed[tenant] >= b.perTenantDaily {
		return false, "cost_cap"
	}
	return true, ""
}

// Record accounts for one call to adapter for (tenant, subject) having
// actually been made. Callers must only call Record after a matching
// Allow returned true and the call was actually issued.
func (b *Budgets) Record(adapter, tenant, subject string, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.resetIfNewDayLocked(now)

	b.adapterUsed[adapter]++
	b.subjectUsed[subjectKey(tenant, subject)]++
	b.tenantUsed[tenant]++
}

// resetIfNewDayLocked clears every counter when now's UTC calendar day
// differs from the day the counters currently belong to. Caller must hold
// b.mu.
func (b *Budgets) resetIfNewDayLocked(now time.Time) {
	day := now.UTC().Format("2006-01-02")
	if day == b.day {
		return
	}
	b.day = day
	b.adapterUsed = make(map[string]int)
	b.subjectUsed = make(map[string]int)
	b.tenantUsed = make(map[string]int)
}

func subjectKey(tenant, subject string) string { return tenant + "/" + subject }
