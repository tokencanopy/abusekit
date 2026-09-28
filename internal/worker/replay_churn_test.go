package worker

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/tokencanopy/abusekit/internal/event"
	"github.com/tokencanopy/abusekit/internal/feature"
)

// TestReplay_ChurnHighFromThirdSubjectOnward replays eval/fixtures/
// churn.jsonl — twelve subjects sharing a signup email hash and a card
// fingerprint, each: created, paid (prepaid, quick), its first
// resource.created, then a PERMANENT deletion — through the REAL
// feature.StoreNeighbors (not a fake), incrementally: after each
// subject's first resource.created (and before its own deletion), Tick
// and check its tier, exactly matching design §1 success criterion 2(c):
// "every subject from the third onward at high on its first
// resource.created" (B1 fix round, proven: with S1's original weights and
// unsaturated linked_deleted_n, the first subject to reach high was the
// ninth, not the third).
//
// This needs the real store (not a fake Neighbors) since it's the
// same-tenant link graph itself — built from every earlier subject's own
// events — that produces the evidence being asserted on.
func TestReplay_ChurnHighFromThirdSubjectOnward(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	cfg := loadShippedConfig(t)
	neighbors := feature.NewStoreNeighbors(s, feature.Config{})

	all := loadFixture(t, filepath.Join(repoRoot(t), "eval", "fixtures", "churn.jsonl"))
	const eventsPerSubject = 5 // created, payment.attempt, subscription.changed, resource.created, subject.deleted
	if len(all)%eventsPerSubject != 0 {
		t.Fatalf("churn.jsonl fixture shape assumption broken: %d events is not a multiple of %d", len(all), eventsPerSubject)
	}
	numSubjects := len(all) / eventsPerSubject

	for i := 0; i < numSubjects; i++ {
		subject := all[i*eventsPerSubject].Subject
		onboarding := all[i*eventsPerSubject : i*eventsPerSubject+4] // everything up to and including resource.created
		deletion := all[i*eventsPerSubject+4]

		ingestFixture(t, ctx, s, onboarding)
		now := lastEventAt(onboarding).Add(time.Second)

		w, err := New(Deps{Store: s, Config: cfg, Neighbors: neighbors, Brands: loadShippedBrands(t), Webmail: loadShippedWebmail(t), Now: func() time.Time { return now }})
		if err != nil {
			t.Fatalf("subject %d: New: %v", i+1, err)
		}
		result, err := w.Tick(ctx)
		if err != nil {
			t.Fatalf("subject %d: Tick: %v", i+1, err)
		}
		// >= 1, not == 1: a PRIOR subject's own subject.deleted event (its
		// permanent deletion, ingested at the end of its own loop
		// iteration) bumps ITS dirty_seq too, so this tick may also
		// reclaim and rescore it alongside the current subject — harmless
		// (it just means its own already-established tier gets
		// recomputed from the same evidence), but means this tick isn't
		// guaranteed to touch only the current subject.
		if result.Scored < 1 || len(result.Errors) != 0 {
			t.Fatalf("subject %d: Tick result = %+v, want at least one subject scored with no errors", i+1, result)
		}

		view, err := s.SubjectView(ctx, testTenant, subject, nil)
		if err != nil {
			t.Fatalf("subject %d: SubjectView: %v", i+1, err)
		}

		wantHigh := i+1 >= 3 // design §1.2(c): "from the third onward"
		if wantHigh && view.Tier != "high" {
			t.Errorf("churn subject %d (%s): tier = %q (score %v), want high\nsignals: %+v", i+1, subject, view.Tier, view.Score, view.Signals)
		}
		if !wantHigh && view.Tier == "high" {
			t.Errorf("churn subject %d (%s): tier = high (score %v) BEFORE the third subject — evidence accrued too early", i+1, subject, view.Score)
		}
		if i+1 == 3 {
			assertBand(t, "churn subject 3", view.Score, 0.8, 0.95)
		}
		if i+1 == numSubjects {
			// linked_deleted_n saturates at 3 (S1 fix round): the LAST
			// subject's score should be no different from the 4th's, not
			// growing without bound as more subjects churn.
			assertBand(t, "churn subject (saturated)", view.Score, 0.9, 1.0)
		}

		// Delete this subject (permanent) before moving to the next one,
		// so the NEXT subject's own first scoring round sees it as a
		// same-tenant neighbour that has been deleted.
		ingestFixture(t, ctx, s, []event.Event{deletion})
	}
}
