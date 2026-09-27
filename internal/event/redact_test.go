package event

import (
	"strings"
	"testing"
)

func TestEvent_Redact(t *testing.T) {
	tests := []struct {
		name     string
		typ      string
		data     map[string]any
		wantCode Code // "" means no error expected
		check    func(t *testing.T, out map[string]any)
	}{
		{
			name: "known type keeps listed keys",
			typ:  "subject.created",
			data: map[string]any{"channel": "api", "email_domain_class": "corporate", "identity_kind": "human"},
			check: func(t *testing.T, out map[string]any) {
				if out["channel"] != "api" || out["email_domain_class"] != "corporate" {
					t.Fatalf("expected listed keys to survive, got %#v", out)
				}
			},
		},
		{
			name: "known type drops unlisted keys",
			typ:  "subject.created",
			data: map[string]any{"channel": "api", "totally_unlisted": "value"},
			check: func(t *testing.T, out map[string]any) {
				if _, ok := out["totally_unlisted"]; ok {
					t.Fatalf("expected unlisted key to be dropped, got %#v", out)
				}
				if out["channel"] != "api" {
					t.Fatalf("expected listed key to survive")
				}
			},
		},
		{
			name: "unknown type keeps all keys",
			typ:  "some.custom_type",
			data: map[string]any{"anything": "goes", "num": 3.0},
			check: func(t *testing.T, out map[string]any) {
				if out["anything"] != "goes" || out["num"] != 3.0 {
					t.Fatalf("expected unknown type to pass through, got %#v", out)
				}
			},
		},
		{
			name:     "email in listed field is rejected",
			typ:      "payment.attempt",
			data:     map[string]any{"outcome": "someone@example.com"},
			wantCode: CodeRedactionFailed,
		},
		{
			name:     "email in unlisted field is still rejected",
			typ:      "subject.created",
			data:     map[string]any{"unlisted_field": "someone@example.com"},
			wantCode: CodeRedactionFailed,
		},
		{
			name:     "email in unknown-type field is rejected",
			typ:      "some.custom_type",
			data:     map[string]any{"anything": "contact me at abuser@lure.test"},
			wantCode: CodeRedactionFailed,
		},
		{
			name: "resource.created computes name_skeleton",
			typ:  "resource.created",
			data: map[string]any{"kind": "agent", "name": "Support"},
			check: func(t *testing.T, out map[string]any) {
				sk, ok := out["name_skeleton"].(string)
				if !ok || sk == "" {
					t.Fatalf("expected name_skeleton to be computed, got %#v", out)
				}
			},
		},
		{
			name: "producer-supplied skeleton is ignored and recomputed",
			typ:  "resource.created",
			data: map[string]any{"name": "Support", "name_skeleton": "not-real"},
			check: func(t *testing.T, out map[string]any) {
				if out["name_skeleton"] == "not-real" {
					t.Fatalf("expected producer-supplied skeleton to be dropped and recomputed")
				}
			},
		},
		{
			name: "content.sent computes subject_line_skeleton",
			typ:  "content.sent",
			data: map[string]any{"subject_line": "Urgent Action Required"},
			check: func(t *testing.T, out map[string]any) {
				if out["subject_line_skeleton"] == "" || out["subject_line_skeleton"] == nil {
					t.Fatalf("expected subject_line_skeleton to be computed, got %#v", out)
				}
			},
		},
		{
			name: "over-cap string is truncated not rejected",
			typ:  "resource.created",
			data: map[string]any{"name": strings.Repeat("a", 500)},
			check: func(t *testing.T, out map[string]any) {
				s, _ := out["name"].(string)
				if len(s) != 200 {
					t.Fatalf("expected name truncated to 200 bytes, got %d", len(s))
				}
			},
		},
		{
			name:     "oversized data is too_large",
			typ:      "some.custom_type",
			data:     map[string]any{"blob": strings.Repeat("x", MaxDataBytes+1)},
			wantCode: CodeTooLarge,
		},
		{
			name: "empty data is fine",
			typ:  "subject.created",
			data: nil,
			check: func(t *testing.T, out map[string]any) {
				if out == nil {
					t.Fatalf("expected non-nil empty map")
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := &Event{Type: tc.typ, Data: tc.data}
			err := e.Redact()
			if tc.wantCode != "" {
				if err == nil {
					t.Fatalf("expected error code %s, got nil", tc.wantCode)
				}
				verr, ok := err.(*ValidationError)
				if !ok || verr.Code != tc.wantCode {
					t.Fatalf("expected code %s, got %v", tc.wantCode, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.check != nil {
				tc.check(t, e.Data)
			}
		})
	}
}

func TestSkeleton(t *testing.T) {
	tests := []struct {
		name string
		a, b string
	}{
		{"cyrillic a lookalike", "аmazon", "amazon"}, // leading char is Cyrillic а (U+0430)
		{"case folding", "Support", "support"},
		{"leetspeak digits", "supp0rt", "support"},
		{"diacritics stripped", "café", "cafe"},
		{"whitespace collapsed", "hello   world", "hello world"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Skeleton(tc.a)
			want := Skeleton(tc.b)
			if got != want {
				t.Errorf("Skeleton(%q) = %q, Skeleton(%q) = %q; want equal", tc.a, got, tc.b, want)
			}
		})
	}
}
