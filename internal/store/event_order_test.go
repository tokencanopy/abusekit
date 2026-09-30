package store_test

import (
	"context"
	"github.com/tokencanopy/abusekit/internal/event"
	"testing"
	"time"
)

func TestEventsForSubjectCanonicalOrder(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	at := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, pair := range [][2]string{{"z-producer", "a"}, {"a-producer", "z"}, {"a-producer", "a"}} {
		e := mkEvent(t, pair[1], "acct_order", "subject.created", at, map[string]any{})
		if _, err := s.AppendEvents(ctx, testTenant, pair[0], []event.Event{e}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.EventsForSubject(ctx, testTenant, "acct_order")
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"a", "z", "a"} {
		if rows[i].Event.ID != want {
			t.Fatalf("row %d=%s want %s", i, rows[i].Event.ID, want)
		}
	}
}
