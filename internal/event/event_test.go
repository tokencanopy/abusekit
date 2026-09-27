package event

import (
	"strings"
	"testing"
	"time"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse time %q: %v", s, err)
	}
	return tm
}

func TestEvent_Validate(t *testing.T) {
	now := mustTime(t, "2026-09-27T12:00:00Z")
	hash := strings.Repeat("a", LinkHashLen)

	base := func() Event {
		return Event{
			ID:      "evt_example_1",
			Subject: "acct_example_1",
			Type:    "subject.created",
			At:      now.Add(-time.Minute),
		}
	}

	tests := []struct {
		name     string
		mutate   func(e *Event)
		opts     ValidateOptions
		wantCode Code
		wantOK   bool
	}{
		{name: "valid event", wantOK: true},
		{name: "missing id", mutate: func(e *Event) { e.ID = "" }, wantCode: CodeBadID},
		{name: "id too long", mutate: func(e *Event) { e.ID = strings.Repeat("x", MaxIDLen+1) }, wantCode: CodeBadID},
		{name: "id at max is ok", mutate: func(e *Event) { e.ID = strings.Repeat("x", MaxIDLen) }, wantOK: true},
		{name: "missing subject", mutate: func(e *Event) { e.Subject = "" }, wantCode: CodeBadSubject},
		{name: "subject too long", mutate: func(e *Event) { e.Subject = strings.Repeat("x", MaxSubjectLen+1) }, wantCode: CodeBadSubject},
		{name: "missing type", mutate: func(e *Event) { e.Type = "" }, wantCode: CodeBadType},
		{name: "type with uppercase rejected", mutate: func(e *Event) { e.Type = "Subject.Created" }, wantCode: CodeBadType},
		{name: "type with digits rejected", mutate: func(e *Event) { e.Type = "resource.created2" }, wantCode: CodeBadType},
		{name: "type too long", mutate: func(e *Event) { e.Type = strings.Repeat("a", MaxTypeLen+1) }, wantCode: CodeBadType},
		{name: "custom type with dots and underscores ok", mutate: func(e *Event) { e.Type = "my_custom.event_type" }, wantOK: true},
		{name: "missing timestamp", mutate: func(e *Event) { e.At = time.Time{} }, wantCode: CodeBadTimestamp},
		{
			name:     "timestamp too far in past",
			mutate:   func(e *Event) { e.At = now.Add(-25 * time.Hour) },
			wantCode: CodeBadTimestamp,
		},
		{
			name:     "timestamp too far in future",
			mutate:   func(e *Event) { e.At = now.Add(25 * time.Hour) },
			wantCode: CodeBadTimestamp,
		},
		{
			name:   "timestamp within skew window is ok",
			mutate: func(e *Event) { e.At = now.Add(-23 * time.Hour) },
			wantOK: true,
		},
		{
			name:   "backfill scope allows arbitrarily old timestamp",
			mutate: func(e *Event) { e.At = now.Add(-24 * 365 * time.Hour) },
			opts:   ValidateOptions{Now: now, Backfill: true},
			wantOK: true,
		},
		{
			name:     "bad email hash length",
			mutate:   func(e *Event) { e.Links.EmailHash = "abc" },
			wantCode: CodeBadLinks,
		},
		{
			name:     "bad email hash not hex",
			mutate:   func(e *Event) { e.Links.EmailHash = strings.Repeat("g", LinkHashLen) },
			wantCode: CodeBadLinks,
		},
		{
			name:   "valid email hash ok",
			mutate: func(e *Event) { e.Links.EmailHash = hash },
			wantOK: true,
		},
		{
			name:   "valid links with asn ok",
			mutate: func(e *Event) { e.Links.ASN = "AS64512" },
			wantOK: true,
		},
		{
			name:     "asn with whitespace rejected",
			mutate:   func(e *Event) { e.Links.ASN = "AS 64512" },
			wantCode: CodeBadLinks,
		},
		{
			name:     "asn too long",
			mutate:   func(e *Event) { e.Links.ASN = strings.Repeat("1", MaxASNLen+1) },
			wantCode: CodeBadLinks,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := base()
			if tc.mutate != nil {
				tc.mutate(&e)
			}
			opts := tc.opts
			if opts.Now.IsZero() {
				opts.Now = now
			}
			err := e.Validate(opts)
			if tc.wantOK {
				if err != nil {
					t.Fatalf("expected valid, got error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error code %s, got nil", tc.wantCode)
			}
			verr, ok := err.(*ValidationError)
			if !ok {
				t.Fatalf("expected *ValidationError, got %T", err)
			}
			if verr.Code != tc.wantCode {
				t.Fatalf("expected code %s, got %s (%v)", tc.wantCode, verr.Code, err)
			}
		})
	}
}

func TestEvent_BodyHash(t *testing.T) {
	now := mustTime(t, "2026-09-27T12:00:00Z")
	e1 := Event{Subject: "s1", Type: "subject.created", At: now, Data: map[string]any{"channel": "api"}}
	e2 := Event{Subject: "s1", Type: "subject.created", At: now, Data: map[string]any{"channel": "api"}}
	e3 := Event{Subject: "s1", Type: "subject.created", At: now, Data: map[string]any{"channel": "web"}}

	h1, err := e1.BodyHash()
	if err != nil {
		t.Fatalf("BodyHash: %v", err)
	}
	h2, err := e2.BodyHash()
	if err != nil {
		t.Fatalf("BodyHash: %v", err)
	}
	h3, err := e3.BodyHash()
	if err != nil {
		t.Fatalf("BodyHash: %v", err)
	}

	if h1 != h2 {
		t.Errorf("expected identical bodies to hash identically: %s != %s", h1, h2)
	}
	if h1 == h3 {
		t.Errorf("expected different bodies to hash differently")
	}
	if len(h1) != 64 {
		t.Errorf("expected 64 hex chars (sha256), got %d", len(h1))
	}
}
