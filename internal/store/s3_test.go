package store_test

// Tests for the S3 (HTTP surface) additions to internal/store:
// ClaimSubjectForEvaluate, ListSubjects, HasAbusiveLabel/EraseSubject, and
// InsertCorpusExample. Kept in their own file, matching s2_test.go's own
// "one file per slice" convention for this package's test suite.

import (
	"context"
	"testing"
	"time"

	"github.com/tokencanopy/abusekit/internal/store"
)

func TestClaimSubjectForEvaluate_ClaimsAndReturnsState(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-09-27T12:00:00Z")

	appendAt(t, ctx, s, "acct_eval_1", "subject.created", now, nil)

	d, err := s.ClaimSubjectForEvaluate(ctx, testTenant, "acct_eval_1", now)
	if err != nil {
		t.Fatalf("ClaimSubjectForEvaluate: %v", err)
	}
	if d.Subject != "acct_eval_1" || d.DirtySeq != 1 || d.ScoredSeq != 0 {
		t.Fatalf("unexpected claim state: %+v", d)
	}
}

func TestClaimSubjectForEvaluate_NotFoundForUnseenSubject(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-09-27T12:00:00Z")

	_, err := s.ClaimSubjectForEvaluate(ctx, testTenant, "never_seen", now)
	if err != store.ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestClaimSubjectForEvaluate_RejectsInternalAndSynthetic(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-09-27T12:00:00Z")

	appendAt(t, ctx, s, "acct_internal_eval", "subject.created", now, nil)
	appendAt(t, ctx, s, "acct_internal_eval", "subject.class", now.Add(time.Second), map[string]any{"class": "internal"})

	if _, err := s.ClaimSubjectForEvaluate(ctx, testTenant, "acct_internal_eval", now); err == nil {
		t.Fatalf("expected an error claiming an internal-class subject for evaluate")
	}
}

// TestClaimSubjectForEvaluate_AlreadyClaimedByWorker proves evaluate and
// the worker's own ClaimDirtySubjects can never both be mid-scoring-round
// for the same subject at once — design's "same lease... rules" for
// evaluate.
func TestClaimSubjectForEvaluate_AlreadyClaimedByWorker(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-09-27T12:00:00Z")

	appendAt(t, ctx, s, "acct_eval_race", "subject.created", now, nil)

	dirty, err := s.ClaimDirtySubjects(ctx, now, 0)
	if err != nil {
		t.Fatalf("ClaimDirtySubjects: %v", err)
	}
	if len(dirty) != 1 {
		t.Fatalf("expected the worker to claim exactly one subject, got %d", len(dirty))
	}

	_, err = s.ClaimSubjectForEvaluate(ctx, testTenant, "acct_eval_race", now)
	if err != store.ErrAlreadyClaimed {
		t.Fatalf("expected ErrAlreadyClaimed while the worker holds the claim, got %v", err)
	}
}

func TestClaimSubjectForEvaluate_ReclaimableAfterRelease(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-09-27T12:00:00Z")

	appendAt(t, ctx, s, "acct_eval_release", "subject.created", now, nil)

	if _, err := s.ClaimSubjectForEvaluate(ctx, testTenant, "acct_eval_release", now); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if err := s.ReleaseClaim(ctx, testTenant, "acct_eval_release"); err != nil {
		t.Fatalf("ReleaseClaim: %v", err)
	}
	if _, err := s.ClaimSubjectForEvaluate(ctx, testTenant, "acct_eval_release", now); err != nil {
		t.Fatalf("second claim after release: %v", err)
	}
}

func TestListSubjects_FiltersAndPagesStably(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-09-27T12:00:00Z")

	// Three scored subjects (distinct tiers) plus one never-scored.
	appendAt(t, ctx, s, "acct_list_high", "subject.created", now, nil)
	appendAt(t, ctx, s, "acct_list_medium", "subject.created", now, nil)
	appendAt(t, ctx, s, "acct_list_low", "subject.created", now, nil)
	appendAt(t, ctx, s, "acct_list_unscored", "subject.created", now, nil)

	score := func(subject, tier string, val float64, scoredAt time.Time) {
		before, err := storeViewFor(t, ctx, s, subject)
		if err != nil {
			t.Fatalf("SubjectView(%s): %v", subject, err)
		}
		_, err = s.UpsertVerdicts(ctx, testTenant, subject, before.EventsSinceScore, []store.VerdictRecord{
			{Rule: "new_account_velocity", Mode: "advise", Scorer: "local", Model: "local", Checkpoint: "v1",
				Probs: map[string]float64{"benign": 1 - val, "abusive": val}, Risk: &val, Flagged: val >= 0.6,
				Reason: "r", InputHash: "h_" + subject, Status: "scored"},
		}, store.SubjectSummary{Tier: tier, Score: val})
		if err != nil {
			t.Fatalf("UpsertVerdicts(%s): %v", subject, err)
		}
		_ = scoredAt
	}
	score("acct_list_high", "high", 0.9, now)
	score("acct_list_medium", "medium", 0.5, now)
	score("acct_list_low", "low", 0.1, now)

	items, cursor, err := s.ListSubjects(ctx, testTenant, store.ListSubjectsOptions{Limit: 100})
	if err != nil {
		t.Fatalf("ListSubjects: %v", err)
	}
	if cursor != nil {
		t.Fatalf("expected no next cursor for a single full page, got %+v", cursor)
	}
	if len(items) != 4 {
		t.Fatalf("expected 4 subjects, got %d: %+v", len(items), items)
	}
	// The never-scored subject must sort LAST (NULL current_scored_at).
	if items[len(items)-1].Subject != "acct_list_unscored" {
		t.Fatalf("expected the never-scored subject last, got order %+v", items)
	}

	// Tier filter.
	highOnly, _, err := s.ListSubjects(ctx, testTenant, store.ListSubjectsOptions{Tier: "high"})
	if err != nil {
		t.Fatalf("ListSubjects(tier=high): %v", err)
	}
	if len(highOnly) != 1 || highOnly[0].Subject != "acct_list_high" {
		t.Fatalf("tier filter = %+v, want exactly acct_list_high", highOnly)
	}

	// Page size 1 walks every scored subject exactly once, in stable
	// order, with no duplicates or gaps — the real point of a keyset
	// cursor.
	seen := map[string]bool{}
	var cur *store.ListCursor
	for {
		page, next, err := s.ListSubjects(ctx, testTenant, store.ListSubjectsOptions{Limit: 1, Cursor: cur})
		if err != nil {
			t.Fatalf("ListSubjects page: %v", err)
		}
		if len(page) != 1 {
			t.Fatalf("expected exactly one row per page, got %d", len(page))
		}
		if seen[page[0].Subject] {
			t.Fatalf("subject %s returned twice across pages", page[0].Subject)
		}
		seen[page[0].Subject] = true
		if next == nil {
			break
		}
		cur = next
	}
	if len(seen) != 4 {
		t.Fatalf("walked %d subjects across pages, want 4: %v", len(seen), seen)
	}
}

func TestListSubjects_SinceFiltersByScoredAt(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-09-27T12:00:00Z")

	appendAt(t, ctx, s, "acct_since_1", "subject.created", now, nil)
	before, err := storeViewFor(t, ctx, s, "acct_since_1")
	if err != nil {
		t.Fatalf("SubjectView: %v", err)
	}
	risk := 0.9
	if _, err := s.UpsertVerdicts(ctx, testTenant, "acct_since_1", before.EventsSinceScore, []store.VerdictRecord{
		{Rule: "new_account_velocity", Mode: "advise", Scorer: "local", Model: "local", Checkpoint: "v1",
			Probs: map[string]float64{"benign": 0.1, "abusive": 0.9}, Risk: &risk, Flagged: true,
			Reason: "r", InputHash: "h", Status: "scored"},
	}, store.SubjectSummary{Tier: "high", Score: 0.9}); err != nil {
		t.Fatalf("UpsertVerdicts: %v", err)
	}

	// A `since` far in the future excludes the just-scored subject (its
	// real scored_at is real wall-clock "now", not the fictional `now`
	// above — UpsertVerdicts always stamps the DB's own now()).
	future := time.Now().UTC().Add(time.Hour)
	items, _, err := s.ListSubjects(ctx, testTenant, store.ListSubjectsOptions{Since: future})
	if err != nil {
		t.Fatalf("ListSubjects(since=future): %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected zero subjects scored after %v, got %+v", future, items)
	}

	past := time.Now().UTC().Add(-time.Hour)
	items, _, err = s.ListSubjects(ctx, testTenant, store.ListSubjectsOptions{Since: past})
	if err != nil {
		t.Fatalf("ListSubjects(since=past): %v", err)
	}
	if len(items) != 1 || items[0].Subject != "acct_since_1" {
		t.Fatalf("expected acct_since_1, got %+v", items)
	}
}

func TestEraseSubject_PurgesWhenNotAbusiveLabelled(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-09-27T12:00:00Z")

	appendAt(t, ctx, s, "acct_erase_purge", "subject.created", now, map[string]any{"channel": "signup"})

	result, err := s.EraseSubject(ctx, testTenant, "acct_erase_purge", now)
	if err != nil {
		t.Fatalf("EraseSubject: %v", err)
	}
	if result.Mode != store.ErasureModePurged || result.AlreadyErased {
		t.Fatalf("unexpected result: %+v", result)
	}

	if _, err := s.SubjectView(ctx, testTenant, "acct_erase_purge", nil); err != store.ErrNotFound {
		t.Fatalf("expected ErrNotFound after purge, got %v", err)
	}

	// A repeat DELETE on an already-purged subject is indistinguishable
	// from a never-seen one — see EraseSubject's own doc comment.
	if _, err := s.EraseSubject(ctx, testTenant, "acct_erase_purge", now); err != store.ErrNotFound {
		t.Fatalf("expected ErrNotFound on a repeat purge, got %v", err)
	}
}

func TestEraseSubject_TombstonesWhenAbusiveLabelled(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-09-27T12:00:00Z")

	appendAt(t, ctx, s, "acct_erase_tombstone", "subject.created", now, map[string]any{"channel": "signup"})
	before, err := storeViewFor(t, ctx, s, "acct_erase_tombstone")
	if err != nil {
		t.Fatalf("SubjectView: %v", err)
	}
	risk := 0.95
	if _, err := s.UpsertVerdicts(ctx, testTenant, "acct_erase_tombstone", before.EventsSinceScore, []store.VerdictRecord{
		{Rule: "new_account_velocity", Mode: "advise", Scorer: "local", Model: "local", Checkpoint: "v1",
			Probs: map[string]float64{"benign": 0.05, "abusive": 0.95}, Risk: &risk, Flagged: true,
			Reason: "high velocity, real reason text", InputHash: "h", Status: "scored"},
	}, store.SubjectSummary{Tier: "high", Score: 0.95}); err != nil {
		t.Fatalf("UpsertVerdicts: %v", err)
	}
	if _, err := s.PutLabel(ctx, testTenant, store.Label{Subject: "acct_erase_tombstone", Label: "abusive", Source: "operator", Actor: "test-operator"}); err != nil {
		t.Fatalf("PutLabel: %v", err)
	}

	has, err := s.HasAbusiveLabel(ctx, testTenant, "acct_erase_tombstone")
	if err != nil || !has {
		t.Fatalf("HasAbusiveLabel = %v, %v, want true, nil", has, err)
	}

	result, err := s.EraseSubject(ctx, testTenant, "acct_erase_tombstone", now)
	if err != nil {
		t.Fatalf("EraseSubject: %v", err)
	}
	if result.Mode != store.ErasureModeTombstoned || result.AlreadyErased {
		t.Fatalf("unexpected result: %+v", result)
	}

	// The subject's numeric score is retained — GET must still answer.
	view, err := s.SubjectView(ctx, testTenant, "acct_erase_tombstone", nil)
	if err != nil {
		t.Fatalf("SubjectView after tombstone: %v", err)
	}
	if view.Tier != "high" || view.Score != 0.95 {
		t.Fatalf("expected the numeric score/tier retained, got %+v", view)
	}
	if len(view.Signals) != 1 || view.Signals[0].Reason != "" {
		t.Fatalf("expected the verdict reason destroyed, got %+v", view.Signals)
	}

	// A repeat call is idempotent: same recorded outcome, no error.
	again, err := s.EraseSubject(ctx, testTenant, "acct_erase_tombstone", now.Add(time.Hour))
	if err != nil {
		t.Fatalf("repeat EraseSubject: %v", err)
	}
	if !again.AlreadyErased || again.Mode != store.ErasureModeTombstoned {
		t.Fatalf("expected an idempotent already-erased result, got %+v", again)
	}
}

func TestEraseSubject_NotFoundForUnseenSubject(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-09-27T12:00:00Z")

	if _, err := s.EraseSubject(ctx, testTenant, "never_seen_erase", now); err != store.ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestInsertCorpusExample_RoundTrips(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := mustTime(t, "2031-09-27T12:00:00Z")

	appendAt(t, ctx, s, "acct_corpus_1", "subject.created", now, map[string]any{"channel": "signup"})
	labelID, err := s.PutLabel(ctx, testTenant, store.Label{Subject: "acct_corpus_1", Label: "benign", Source: "operator", Actor: "test-operator"})
	if err != nil {
		t.Fatalf("PutLabel: %v", err)
	}

	id, err := s.InsertCorpusExample(ctx, testTenant, store.CorpusExample{
		Subject:    "acct_corpus_1",
		LabelID:    labelID,
		DecisionAt: now,
		EventSlice: []map[string]any{{"id": "e1", "type": "subject.created"}},
		Features:   map[string]float64{"subject_age_h": 0, "resource_total": 1},
		Split:      "train",
	})
	if err != nil {
		t.Fatalf("InsertCorpusExample: %v", err)
	}
	if id == 0 {
		t.Fatalf("expected a non-zero corpus example id")
	}
}

func storeViewFor(t *testing.T, ctx context.Context, s *store.Store, subject string) (*store.SubjectView, error) {
	t.Helper()
	return s.SubjectView(ctx, testTenant, subject, nil)
}
