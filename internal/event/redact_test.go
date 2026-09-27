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
		// B1: listed fields must be type-checked scalars — an object or
		// array under a listed key is rejected, not silently stored or
		// stringified.
		{
			name:     "object under a listed skeleton field is rejected",
			typ:      "resource.created",
			data:     map[string]any{"name": map[string]any{"x": "alice@example.com"}},
			wantCode: CodeRedactionFailed,
		},
		{
			name:     "object under a listed non-skeleton field is rejected",
			typ:      "resource.created",
			data:     map[string]any{"kind": map[string]any{"x": "safe"}},
			wantCode: CodeRedactionFailed,
		},
		{
			name:     "array under a listed field is rejected",
			typ:      "resource.created",
			data:     map[string]any{"kind": []any{"bob@example.com"}},
			wantCode: CodeRedactionFailed,
		},
		{
			name:     "array under a listed field is rejected even without an embedded email",
			typ:      "resource.created",
			data:     map[string]any{"kind": []any{"agent", "widget"}},
			wantCode: CodeRedactionFailed,
		},
		{
			name:     "large array under subject_line is rejected, not size-capped",
			typ:      "content.sent",
			data:     map[string]any{"subject_line": makeStringSlice(300, "x")},
			wantCode: CodeRedactionFailed,
		},
		// B1: unknown event types must be walked recursively — an email
		// nested inside an object (at any depth) must be caught, not just
		// top-level string values.
		{
			name:     "unknown type: nested email is rejected",
			typ:      "some.custom_type",
			data:     map[string]any{"nested": map[string]any{"to": "victim@example.com"}},
			wantCode: CodeRedactionFailed,
		},
		{
			name:     "unknown type: email nested inside an array of objects is rejected",
			typ:      "some.custom_type",
			data:     map[string]any{"list": []any{map[string]any{"email": "deep@example.com"}}},
			wantCode: CodeRedactionFailed,
		},
		{
			name: "unknown type: nested non-email data passes through",
			typ:  "some.custom_type",
			data: map[string]any{"nested": map[string]any{"kind": "agent"}},
			check: func(t *testing.T, out map[string]any) {
				nested, ok := out["nested"].(map[string]any)
				if !ok || nested["kind"] != "agent" {
					t.Fatalf("expected nested non-email data to pass through, got %#v", out)
				}
			},
		},
		// B1: the email check must run on NFKC-folded text and accept
		// Unicode local parts/domains, including full-width '＠'.
		{
			name:     "full-width at-sign is still recognized as an email",
			typ:      "some.custom_type",
			data:     map[string]any{"anything": "alice＠example.com"},
			wantCode: CodeRedactionFailed,
		},
		{
			name:     "unicode local part is recognized as an email",
			typ:      "some.custom_type",
			data:     map[string]any{"anything": "ü@example.com"},
			wantCode: CodeRedactionFailed,
		},
		{
			name:     "unicode domain (IDN) is recognized as an email",
			typ:      "some.custom_type",
			data:     map[string]any{"anything": "alice@例え.テスト"},
			wantCode: CodeRedactionFailed,
		},
		// B1: NUL and other control characters must be rejected in every
		// string value, so a Postgres 22021/22P05 insert error can never
		// happen downstream.
		{
			name:     "NUL byte in a string value is rejected",
			typ:      "payment.attempt",
			data:     map[string]any{"reason": "bad\x00value"},
			wantCode: CodeRedactionFailed,
		},
		{
			name:     "NUL byte in an unlisted field is still rejected",
			typ:      "subject.created",
			data:     map[string]any{"totally_unlisted": "bad\x00value"},
			wantCode: CodeRedactionFailed,
		},
		{
			name:     "other control character (bell) is rejected",
			typ:      "payment.attempt",
			data:     map[string]any{"reason": "bad\x07value"},
			wantCode: CodeRedactionFailed,
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

// makeStringSlice builds a []any of n copies of s, mimicking what a
// producer sending a JSON array (rather than the expected string) for a
// listed field would decode into.
func makeStringSlice(n int, s string) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = s
	}
	return out
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
