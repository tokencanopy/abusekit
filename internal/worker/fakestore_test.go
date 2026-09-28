package worker

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/tokencanopy/abusekit/internal/store"
)

// fakeStore is a minimal, in-memory implementation of the Store interface
// for tests that want to inject a specific failure without a database —
// codebase-design's payoff for keeping Store a small interface. It wraps a
// real *store.Store for every method a given test doesn't care about
// overriding, and records calls the test wants to assert on.
type fakeStore struct {
	real *store.Store

	mu                  sync.Mutex
	eventsForSubjectErr error
	recordFailureCalls  []recordFailureCall
}

type recordFailureCall struct {
	Tenant, Subject string
	NextAttemptAt   time.Time
}

func (f *fakeStore) EventsForSubject(ctx context.Context, tenant, subject string) ([]store.StoredEvent, error) {
	f.mu.Lock()
	err := f.eventsForSubjectErr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return f.real.EventsForSubject(ctx, tenant, subject)
}

func (f *fakeStore) ClaimDirtySubjects(ctx context.Context, now time.Time, limit int) ([]store.DirtySubject, error) {
	return f.real.ClaimDirtySubjects(ctx, now, limit)
}

func (f *fakeStore) LatestVerdicts(ctx context.Context, tenant, subject string) (map[string]store.LatestVerdict, error) {
	return f.real.LatestVerdicts(ctx, tenant, subject)
}

func (f *fakeStore) UpsertVerdicts(ctx context.Context, tenant, subject string, dirtySeqAtStart int64, records []store.VerdictRecord, summary store.SubjectSummary) ([]int64, error) {
	return f.real.UpsertVerdicts(ctx, tenant, subject, dirtySeqAtStart, records, summary)
}

func (f *fakeStore) GetRuleBackoff(ctx context.Context, tenant, subject, rule string) (store.RuleBackoff, error) {
	return f.real.GetRuleBackoff(ctx, tenant, subject, rule)
}

func (f *fakeStore) RecordRuleError(ctx context.Context, tenant, subject, rule string, retryAt time.Time, lastError string) error {
	return f.real.RecordRuleError(ctx, tenant, subject, rule, retryAt, lastError)
}

func (f *fakeStore) ClearRuleBackoff(ctx context.Context, tenant, subject, rule string) error {
	return f.real.ClearRuleBackoff(ctx, tenant, subject, rule)
}

func (f *fakeStore) PruneRuleState(ctx context.Context, tenant, subject string, currentRules []string) error {
	return f.real.PruneRuleState(ctx, tenant, subject, currentRules)
}

func (f *fakeStore) RecordSubjectFailure(ctx context.Context, tenant, subject string, nextAttemptAt time.Time) error {
	f.mu.Lock()
	f.recordFailureCalls = append(f.recordFailureCalls, recordFailureCall{tenant, subject, nextAttemptAt})
	f.mu.Unlock()
	return f.real.RecordSubjectFailure(ctx, tenant, subject, nextAttemptAt)
}

func (f *fakeStore) ReleaseClaim(ctx context.Context, tenant, subject string) error {
	return f.real.ReleaseClaim(ctx, tenant, subject)
}

func (f *fakeStore) ExtendClaims(ctx context.Context, tenants, subjects []string, now time.Time) error {
	return f.real.ExtendClaims(ctx, tenants, subjects, now)
}

func (f *fakeStore) QueueStats(ctx context.Context, now time.Time) (int, time.Duration, error) {
	return f.real.QueueStats(ctx, now)
}

func (f *fakeStore) ClaimSubjectForEvaluate(ctx context.Context, tenant, subject string, now time.Time) (store.DirtySubject, error) {
	return f.real.ClaimSubjectForEvaluate(ctx, tenant, subject, now)
}

func (f *fakeStore) setEventsForSubjectErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.eventsForSubjectErr = err
}

func (f *fakeStore) recordedFailures() []recordFailureCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordFailureCall, len(f.recordFailureCalls))
	copy(out, f.recordFailureCalls)
	return out
}

var errInjected = errors.New("injected failure")
