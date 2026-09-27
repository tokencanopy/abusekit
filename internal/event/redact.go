package event

import (
	"encoding/json"
	"fmt"
	"regexp"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// RedactionSchemaVersion identifies the version of the static redaction
// schema (the `schema` table below) that produced a given event's stored
// `data` (N6). The store stamps every accepted event's `redaction_version`
// column with this constant at insert time (design §4.3: "versioned by
// being part of this package's source — a schema change ships as a code
// change and a new abusekit release"). Bump it whenever a change to
// `schema` (a field added/removed/re-capped, an enum tightened, a new
// event type) would change what an already-stored event's `data` means
// relative to a freshly-redacted one — that's what lets an operator (or a
// future migration) identify which rows were redacted under an older
// rule set without guessing from `received_at` timestamps.
const RedactionSchemaVersion = 1

// fieldSpec describes how one `data` key of a known event type is handled.
type fieldSpec struct {
	// maxLen caps a string value's byte length; values over the cap are
	// truncated (at a rune boundary), not rejected. Zero means "no cap"
	// (used for non-string fields such as counts and amounts).
	maxLen int
	// skeleton, when true, also stores a `<key>_skeleton` companion
	// computed by Skeleton (see skeleton.go) — used for the two fields
	// brand/lure matching keys off: resource.created/deleted's `name`
	// and content.sent's `subject_line`.
	skeleton bool
	// enum, when non-nil, is the closed set of values design §4.3's
	// built-in vocabulary table allows for this field (S8); a string
	// value outside it is rejected with CodeRedactionFailed rather than
	// silently stored. nil means no enum restriction (a free-form
	// string field, still capped/skeletoned as configured above).
	enum []string
}

// isEnumValue reports whether s is one of spec's allowed enum values.
// spec.enum == nil means "no restriction" and always reports true.
func (spec fieldSpec) isEnumValue(s string) bool {
	if spec.enum == nil {
		return true
	}
	for _, v := range spec.enum {
		if v == s {
			return true
		}
	}
	return false
}

// schema is the static, versioned redaction schema from design §4.3: for
// each built-in event type, the set of `data` keys that pass through (with
// their cap), and nothing else. It is versioned by being part of this
// package's source — a schema change ships as a code change and a new
// abusekit release, never as runtime config, so ingest's behavior for a
// given release is fully determined by its binary.
//
// Ingest never consults rules (design §4.3): this table is the only
// source of truth for what leaves the wire and reaches storage.
//
// Every listed field is a scalar (string, number, or bool) — see Redact's
// doc comment for why an object or array under a listed key is a hard
// rejection rather than a pass-through.
var schema = map[string]map[string]fieldSpec{
	"subject.created": {
		"channel":            {maxLen: 64},
		"email_domain_class": {maxLen: 32, enum: []string{"webmail", "corporate", "disposable", "unknown"}},
		"identity_kind":      {maxLen: 64},
	},
	"subject.deleted": {
		"mode": {maxLen: 16, enum: []string{"trash", "permanent"}},
	},
	"payment.attempt": {
		"outcome":      {maxLen: 32, enum: []string{"succeeded", "declined", "blocked"}},
		"reason":       {maxLen: 128},
		"funding":      {maxLen: 16, enum: []string{"prepaid", "debit", "credit", "unknown"}},
		"amount_minor": {},
		"currency":     {maxLen: 8},
	},
	"subscription.changed": {
		"plan":         {maxLen: 64},
		"status":       {maxLen: 32},
		"amount_minor": {},
	},
	"resource.created": {
		"kind":           {maxLen: 32},
		"name":           {maxLen: 200, skeleton: true},
		"address_domain": {maxLen: 253},
	},
	"resource.deleted": {
		"kind":           {maxLen: 32},
		"name":           {maxLen: 200, skeleton: true},
		"address_domain": {maxLen: 253},
	},
	"content.sent": {
		"subject_line":              {maxLen: 200, skeleton: true},
		"recipient_domain":          {maxLen: 253},
		"recipient_count":           {},
		"recipient_hash":            {maxLen: 128},
		"recipient_is_own_identity": {},
		"first_link_host":           {maxLen: 253},
	},
	"content.verdict": {
		"source":   {maxLen: 64},
		"category": {maxLen: 64},
		"score":    {},
	},
	"subject.class": {
		"class": {maxLen: 32, enum: []string{"customer", "internal", "synthetic"}},
	},
}

// emailRe flags a value that looks like an email address anywhere in a
// string, once the string has been NFKC-folded (see looksLikeEmail). It is
// intentionally simple (not a full RFC 5322 matcher): redaction's job is
// to catch accidental PII, not to validate addresses, and a stricter
// matcher would only create false negatives that let real emails through.
//
// \p{L} and \p{N} (rather than A-Za-z0-9) make the local part, domain and
// TLD Unicode-aware, so an internationalized address (e.g. a Unicode local
// part, or an IDN domain like "例え.テスト") is still caught — a plain ASCII
// matcher would silently let those through.
var emailRe = regexp.MustCompile(`[\p{L}\p{N}._%+\-]+@[\p{L}\p{N}.\-]+\.[\p{L}]{2,}`)

// looksLikeEmail reports whether s contains an email-shaped substring,
// after NFKC folding. NFKC normalizes compatibility characters to their
// canonical form — notably the full-width '＠' (U+FF20) folds to the ASCII
// '@' (U+0040) — so a producer cannot dodge the check by using a
// Unicode-compatible look-alike of '@' or of an ASCII letter/digit.
func looksLikeEmail(s string) bool {
	return emailRe.MatchString(norm.NFKC.String(s))
}

// hasControlChar reports whether s contains any Unicode control character
// (category Cc, which includes NUL and every other C0/C1 control code).
// Redact and Validate both reject these outright: a NUL byte in specific
// reaches Postgres as an untranslatable character (error 22021 in a
// non-UTF8-safe path, 22P05 from a text column), so rejecting it at the
// boundary is cheaper and clearer than letting the insert fail downstream.
func hasControlChar(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// Redact rewrites e.Data in place per the static schema for e.Type
// (design §4.3):
//
//   - First, e.Data is walked recursively — every map key and every string
//     value, at any depth — and rejected with CodeRedactionFailed if any
//     string contains a control character or looks like an email address.
//     This runs before any type-checking, dropping or truncation, so a
//     producer cannot dodge it by nesting a value inside an unlisted key,
//     an array, or a field belonging to an event type Redact doesn't
//     recognize.
//   - For a known type: listed keys pass through, but ONLY as a scalar
//     (string, number, or bool) — an object or array under a listed key is
//     rejected with CodeRedactionFailed rather than silently stored,
//     stringified, or size-capped, since the field's cap and skeleton
//     handling only make sense for a single scalar value. Unlisted keys
//     are dropped (not hashed). A string field with `skeleton: true` also
//     gets a computed `<key>_skeleton` sibling (any producer-supplied
//     value under that name is dropped as unlisted, then replaced by our
//     own computation); an over-cap string is truncated, not rejected.
//   - For an unknown type: every key is kept as-is (no allow-listing to
//     apply) — the recursive scan above already proved it clean.
//
// After rewriting, the JSON-encoded size of e.Data must be <= MaxDataBytes
// or the event fails with CodeTooLarge.
//
// Redact must run after Validate. It does not re-check id/subject/type/at.
func (e *Event) Redact() error {
	if len(e.Data) == 0 {
		e.Data = map[string]any{}
		return nil
	}

	if err := scanForLeaks(e.Data, "data"); err != nil {
		return err
	}

	fields, known := schema[e.Type]
	out := make(map[string]any, len(e.Data))

	for k, v := range e.Data {
		if !known {
			out[k] = v
			continue
		}
		spec, listed := fields[k]
		if !listed {
			continue // unlisted keys dropped, not an error
		}
		switch val := v.(type) {
		case string:
			s := val
			if !spec.isEnumValue(s) {
				return badErr(CodeRedactionFailed, fmt.Sprintf("data.%s %q is not one of %v", k, s, spec.enum))
			}
			if spec.maxLen > 0 && len(s) > spec.maxLen {
				s = truncateUTF8(s, spec.maxLen)
			}
			out[k] = s
			if spec.skeleton {
				out[k+"_skeleton"] = Skeleton(s)
			}
		case float64, bool, nil:
			out[k] = v
		default:
			return badErr(CodeRedactionFailed, fmt.Sprintf("data.%s must be a string, number, or bool, got %T", k, v))
		}
	}

	size, err := jsonSize(out)
	if err != nil {
		// A value that cannot round-trip through JSON (e.g. NaN) is not a
		// redaction failure in the PII sense, but it is definitely not
		// safe to store — treat it the same way.
		return badErr(CodeRedactionFailed, "data is not JSON-encodable: "+err.Error())
	}
	if size > MaxDataBytes {
		return badErr(CodeTooLarge, "data exceeds 8 KiB after redaction")
	}

	e.Data = out
	return nil
}

// scanForLeaks walks v recursively (v is always one of the types
// encoding/json produces into an `any`: string, float64, bool, nil,
// []any, or map[string]any — including when a test constructs a value by
// hand rather than through json.Unmarshal), checking every string value
// and every map key against hasControlChar/looksLikeEmail. path is used
// only to build a human-readable error message.
func scanForLeaks(v any, path string) error {
	switch val := v.(type) {
	case string:
		return checkLeakString(val, path)
	case map[string]any:
		for k, vv := range val {
			if err := checkLeakString(k, path+"."+k+" (key)"); err != nil {
				return err
			}
			if err := scanForLeaks(vv, path+"."+k); err != nil {
				return err
			}
		}
	case []any:
		for i, vv := range val {
			if err := scanForLeaks(vv, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkLeakString(s, path string) error {
	// R1 (round 2): ranging over invalid UTF-8 silently substitutes U+FFFD
	// per bad byte — hasControlChar never sees the original bytes, so an
	// invalid sequence would otherwise reach Postgres and fail the whole
	// batch at INSERT (the same class of leak as an unrejected NUL byte).
	if !utf8.ValidString(s) {
		return badErr(CodeRedactionFailed, path+" is not valid UTF-8")
	}
	if hasControlChar(s) {
		return badErr(CodeRedactionFailed, path+" contains a control character")
	}
	if looksLikeEmail(s) {
		return badErr(CodeRedactionFailed, path+" looks like an email address")
	}
	return nil
}

func jsonSize(v map[string]any) (int, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return 0, err
	}
	return len(b), nil
}

// truncateUTF8 shortens s to at most maxBytes bytes without splitting a
// multi-byte rune.
func truncateUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	b := s[:maxBytes]
	for len(b) > 0 && !utf8.ValidString(b) {
		b = b[:len(b)-1]
	}
	return b
}
