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
		// S8: closed-set enum fields reject an out-of-set value rather
		// than silently storing it — design §4.3's built-in vocabulary
		// table.
		{
			name: "email_domain_class accepts a listed value",
			typ:  "subject.created",
			data: map[string]any{"email_domain_class": "webmail"},
			check: func(t *testing.T, out map[string]any) {
				if out["email_domain_class"] != "webmail" {
					t.Fatalf("expected webmail to pass through, got %#v", out)
				}
			},
		},
		{
			name:     "email_domain_class rejects an out-of-set value",
			typ:      "subject.created",
			data:     map[string]any{"email_domain_class": "definitely_not_a_real_class"},
			wantCode: CodeRedactionFailed,
		},
		{
			name:     "subject.deleted mode rejects an out-of-set value",
			typ:      "subject.deleted",
			data:     map[string]any{"mode": "vaporized"},
			wantCode: CodeRedactionFailed,
		},
		{
			name: "subject.deleted mode accepts a listed value",
			typ:  "subject.deleted",
			data: map[string]any{"mode": "permanent"},
			check: func(t *testing.T, out map[string]any) {
				if out["mode"] != "permanent" {
					t.Fatalf("expected permanent to pass through, got %#v", out)
				}
			},
		},
		{
			name:     "payment.attempt outcome rejects an out-of-set value",
			typ:      "payment.attempt",
			data:     map[string]any{"outcome": "maybe"},
			wantCode: CodeRedactionFailed,
		},
		{
			name:     "payment.attempt funding rejects an out-of-set value",
			typ:      "payment.attempt",
			data:     map[string]any{"funding": "monopoly_money"},
			wantCode: CodeRedactionFailed,
		},
		{
			name: "payment.attempt funding accepts a listed value",
			typ:  "payment.attempt",
			data: map[string]any{"funding": "prepaid"},
			check: func(t *testing.T, out map[string]any) {
				if out["funding"] != "prepaid" {
					t.Fatalf("expected prepaid to pass through, got %#v", out)
				}
			},
		},
		{
			name:     "subject.class rejects an out-of-set value",
			typ:      "subject.class",
			data:     map[string]any{"class": "vip"},
			wantCode: CodeRedactionFailed,
		},
		{
			name: "subject.class accepts a listed value",
			typ:  "subject.class",
			data: map[string]any{"class": "synthetic"},
			check: func(t *testing.T, out map[string]any) {
				if out["class"] != "synthetic" {
					t.Fatalf("expected synthetic to pass through, got %#v", out)
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
		// N1: I/l/ı/1 must fold together, and folding must happen BEFORE
		// lowercasing so a capital "I" (impersonating lowercase "l") and
		// an unrelated lowercase "i" don't collapse into the same thing
		// for the wrong reason.
		{"capital I folds with lowercase l", "PayPaI", "PayPal"},
		{"dotless i folds with lowercase l", "paypaı", "paypal"},
		{"isolated digit 1 folds with lowercase l", "supp1y", "supply"},
		{"I, l, ı, and isolated 1 are all the same skeleton", "I", "l"},
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

	t.Run("all of I, l, ı, 1 collapse to the same skeleton", func(t *testing.T) {
		want := Skeleton("l")
		for _, s := range []string{"I", "ı", "1"} {
			if got := Skeleton(s); got != want {
				t.Errorf("Skeleton(%q) = %q, want %q (same as Skeleton(\"l\"))", s, got, want)
			}
		}
	})

	// N1: a digit is only leet-mapped to a letter when it's NOT part of a
	// multi-digit run — a run of 2+ digits is almost always a genuine
	// number (an order id, a count), not per-letter leetspeak, so
	// "Order 12345" must not turn into "order l2eas".
	t.Run("digits inside a multi-digit run are not leet-mapped", func(t *testing.T) {
		got := Skeleton("Order 12345")
		want := "order 12345"
		if got != want {
			t.Errorf("Skeleton(%q) = %q, want %q (digits in a run must stay literal)", "Order 12345", got, want)
		}
	})

	t.Run("an isolated leet digit is still mapped inside a word", func(t *testing.T) {
		if got, want := Skeleton("am4z0n"), Skeleton("amazon"); got != want {
			t.Errorf("Skeleton(%q) = %q, want %q", "am4z0n", got, want)
		}
	})
}
