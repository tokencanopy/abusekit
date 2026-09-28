package event

import (
	"encoding/json"
	"fmt"
	"math"
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
//
// S2b bumped this from 1 to 2: content.sent's `recipient_hash` now has a
// closed format (S4), `recipient_count` must be a positive integer (N6),
// a recipient_hash paired with recipient_count > 1 is rejected (S6), and
// `subject_line` masks an email-shaped substring instead of rejecting the
// whole event (S5) — each changes what an already-stored row's `data`
// means relative to a freshly-redacted one.
const RedactionSchemaVersion = 2

// fieldKind is a listed field's declared value type (R2 round-2 review:
// "listed fields typed only as 'some scalar'"). Before this, a switch on
// the Go dynamic type accepted ANY of string/float64/bool for every
// listed field, so a number field silently took a giant string, a bool
// field took a number, and — worse — an enum (text) field given a number
// or a null skipped the enum check entirely (that branch of the switch
// never ran isEnumValue). The zero value is kindText, so most schema
// entries (nearly all fields are text) don't need to set this explicitly.
type fieldKind int

const (
	kindText fieldKind = iota
	kindNumber
	kindBool
)

func (k fieldKind) String() string {
	switch k {
	case kindNumber:
		return "a number"
	case kindBool:
		return "a bool"
	default:
		return "a string"
	}
}

// fieldSpec describes how one `data` key of a known event type is handled.
type fieldSpec struct {
	// kind is the field's declared value type; a value of a different
	// dynamic type (including JSON null, for any listed field) is
	// rejected with CodeRedactionFailed rather than silently stored,
	// stringified, or coerced.
	kind fieldKind
	// maxLen caps a string value's byte length (kindText only); values
	// over the cap are truncated (at a rune boundary), not rejected. Zero
	// means "no cap".
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
	// string field, still capped/skeletoned as configured above). Only
	// meaningful on a kindText field.
	enum []string
	// format, when non-nil, is a closed shape a string value must fully
	// match (S2b's S4 fix round: content.sent's recipient_hash must match
	// ^[A-Za-z0-9_:+/=-]{8,128}$, rejecting anything containing '@' or
	// '%', any whitespace, or anything outside that set). A field with
	// format set is validated INSTEAD of enum/maxLen truncation — the
	// format's own bounds (recipientHashRe's own {8,128}) are exact, so
	// truncating an over-length value first (as maxLen would) could
	// silently turn an invalid hash into one that happens to match after
	// losing its tail; a mismatch is always a hard reject, never a lossy
	// truncation. Only meaningful on a kindText field.
	format *regexp.Regexp
	// maskEmail, when true (content.sent's subject_line, S2b's S5 fix
	// round), replaces an email-shaped substring with "@" instead of
	// rejecting the whole event the way every other field's email check
	// does (scanForLeaks) — see Redact's own doc comment for the exact
	// order this runs in. Only meaningful on a kindText field, and
	// mutually exclusive with format (no field needs both).
	maskEmail bool
	// positiveInteger, when true (content.sent's recipient_count, S2b's
	// N6 fix round), additionally rejects a kindNumber value that is
	// zero, negative, or not a whole number — a producer's own event
	// vocabulary says this field counts recipients, and a fractional or
	// non-positive count is never a valid count of anything. Only
	// meaningful on a kindNumber field.
	positiveInteger bool
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
		// [round 2] R8: an optional producer-supplied real account-
		// creation instant, preferred by internal/feature's Extract over
		// its own derived "earliest ingested event" firstSeenAt when
		// present (see accountCreatedAt in internal/feature/windows.go).
		// Without it, an established account onboarded onto abusekit
		// well after its real signup reads as brand new, defeating every
		// history-relative/age-decay feature exactly for the accounts
		// they exist to protect. Validated exactly against RFC 3339
		// (format, not just maxLen) so a malformed value fails loudly at
		// ingest instead of silently failing time.Parse deep inside
		// feature extraction.
		"account_created_at": {format: accountCreatedAtRe},
	},
	"subject.deleted": {
		"mode": {maxLen: 16, enum: []string{"trash", "permanent"}},
	},
	"payment.attempt": {
		"outcome":      {maxLen: 32, enum: []string{"succeeded", "declined", "blocked"}},
		"reason":       {maxLen: 128},
		"funding":      {maxLen: 16, enum: []string{"prepaid", "debit", "credit", "unknown"}},
		"amount_minor": {kind: kindNumber},
		"currency":     {maxLen: 8},
	},
	"subscription.changed": {
		"plan":         {maxLen: 64},
		"status":       {maxLen: 32},
		"amount_minor": {kind: kindNumber},
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
		// S2b's S5 fix round: an email-shaped substring is MASKED (not a
		// whole-event reject) — see Redact's own doc comment.
		"subject_line":     {maxLen: 200, skeleton: true, maskEmail: true},
		"recipient_domain": {maxLen: 253},
		// S2b's N6 fix round: recipient_count, if present, must be a
		// positive integer.
		"recipient_count": {kind: kindNumber, positiveInteger: true},
		// S2b's S4 fix round: recipient_hash must match a closed,
		// non-PII-shaped format — see recipientHashRe.
		"recipient_hash":            {format: recipientHashRe},
		"recipient_is_own_identity": {kind: kindBool},
		"first_link_host":           {maxLen: 253},
	},
	"content.verdict": {
		"source":   {maxLen: 64},
		"category": {maxLen: 64},
		"score":    {kind: kindNumber},
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

// maskEmails replaces every email-shaped substring in s (after NFKC
// folding, the same normalization looksLikeEmail already matches against
// — see its own doc comment for why) with a literal "@", rather than
// rejecting the whole value (S2b's S5 fix round). Multiple email-shaped
// substrings are each replaced independently.
func maskEmails(s string) string {
	return emailRe.ReplaceAllString(norm.NFKC.String(s), "@")
}

// recipientHashRe is content.sent's recipient_hash format (S2b's S4 fix
// round): 8 to 128 characters from a closed, non-PII-shaped set — letters,
// digits, and the punctuation a base64url/hex/opaque-token encoding
// commonly uses (underscore, colon, plus, slash, equals, hyphen).
// Deliberately excludes '@' and '%' and any whitespace: a keyed hash the
// producer computed should never look like an email address or a
// URL-escaped value, and requiring the closed set rather than only
// blocklisting '@'/'%'/whitespace catches anything else unanticipated
// too (design's own "a keyed hash the producer holds" contract, §4.3).
var recipientHashRe = regexp.MustCompile(`^[A-Za-z0-9_:+/=-]{8,128}$`)

// accountCreatedAtRe is subject.created's optional account_created_at
// format ([round 2] R8): a full RFC 3339 date-time (date, "T", time,
// optional fractional seconds, and a "Z" or numeric UTC offset) —
// anything looser (a bare date, a Unix timestamp, free text) is rejected
// rather than accepted and later failing time.Parse deep inside feature
// extraction. Deliberately doesn't use time.Parse itself here: Redact's
// other format fields are all regex-validated, and a regex keeps this
// field's validation in the same place/style as the rest of the schema.
var accountCreatedAtRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d{1,9})?(Z|[+-]\d{2}:\d{2})$`)

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

// emailMaskExemptKey returns the one top-level `data` key (if any) for
// which scanForLeaks' email check must be skipped in favour of masking
// (S2b's S5 fix round) — content.sent's subject_line. Every other field,
// of every event type, keeps rejecting an embedded email-shaped substring
// outright.
func emailMaskExemptKey(eventType string) string {
	if eventType == "content.sent" {
		return "subject_line"
	}
	return ""
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
//     recognize. The ONE exception (S2b's S5 fix round) is content.sent's
//     top-level subject_line: an email-shaped substring there is masked,
//     not rejected — see emailMaskExemptKey and the field loop below.
//   - For a known type: listed keys pass through, but ONLY as a scalar
//     (string, number, or bool) — an object or array under a listed key is
//     rejected with CodeRedactionFailed rather than silently stored,
//     stringified, or size-capped, since the field's cap/format/skeleton
//     handling only make sense for a single scalar value. Unlisted keys
//     are dropped (not hashed). A string field with `skeleton: true` also
//     gets a computed `<key>_skeleton` sibling (any producer-supplied
//     value under that name is dropped as unlisted, then replaced by our
//     own computation); an over-cap string is truncated, not rejected,
//     UNLESS the field has a `format` (S4), which is validated exactly
//     with no truncation. A number field with `positiveInteger` (N6)
//     additionally rejects zero, negative or fractional values.
//   - content.sent additionally rejects a recipient_hash paired with a
//     recipient_count > 1 (S2b's S6 fix round): design's redaction
//     section documents that a set recipient_hash represents exactly one
//     recipient.
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

	exemptKey := emailMaskExemptKey(e.Type)
	if err := scanForLeaks(e.Data, "data", exemptKey); err != nil {
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
		// R3 (round 2): a listed field must match its DECLARED kind, not
		// just be "some scalar" — a null is rejected for every listed
		// field (a producer that doesn't want to set one should omit the
		// key, not send null), and a value of the wrong dynamic type
		// (a number for a text/enum field, a string for a number field, a
		// bool where a number is expected, ...) is rejected rather than
		// silently stored, coerced, or — for an enum field given a
		// non-string — let past the enum check entirely.
		if v == nil {
			return badErr(CodeRedactionFailed, fmt.Sprintf("data.%s must be %s, got null", k, spec.kind))
		}
		switch val := v.(type) {
		case string:
			if spec.kind != kindText {
				return badErr(CodeRedactionFailed, fmt.Sprintf("data.%s must be %s, got a string", k, spec.kind))
			}
			s := val
			switch {
			case spec.format != nil:
				// S4: a formatted field is validated exactly, never
				// truncated — see fieldSpec.format's own doc comment for
				// why truncating first would be unsafe here.
				if !spec.format.MatchString(s) {
					return badErr(CodeRedactionFailed, fmt.Sprintf("data.%s %q does not match the required format", k, s))
				}
			case spec.maskEmail && looksLikeEmail(s):
				// S5: mask rather than reject. maskEmails NFKC-folds s
				// before replacing (matching looksLikeEmail's own fold),
				// so the stored value is always the masked text, not the
				// original bytes, whenever a match is found.
				s = maskEmails(s)
				fallthrough
			default:
				if !spec.isEnumValue(s) {
					return badErr(CodeRedactionFailed, fmt.Sprintf("data.%s %q is not one of %v", k, s, spec.enum))
				}
				if spec.maxLen > 0 && len(s) > spec.maxLen {
					s = truncateUTF8(s, spec.maxLen)
				}
			}
			out[k] = s
			if spec.skeleton {
				out[k+"_skeleton"] = Skeleton(s)
			}
		case float64:
			if spec.kind != kindNumber {
				return badErr(CodeRedactionFailed, fmt.Sprintf("data.%s must be %s, got a number", k, spec.kind))
			}
			if spec.positiveInteger && (val <= 0 || val != math.Trunc(val)) {
				return badErr(CodeRedactionFailed, fmt.Sprintf("data.%s must be a positive integer, got %v", k, val))
			}
			out[k] = val
		case bool:
			if spec.kind != kindBool {
				return badErr(CodeRedactionFailed, fmt.Sprintf("data.%s must be %s, got a bool", k, spec.kind))
			}
			out[k] = val
		default:
			return badErr(CodeRedactionFailed, fmt.Sprintf("data.%s must be a string, number, or bool, got %T", k, v))
		}
	}

	if err := validateContentSentCrossFields(e.Type, out); err != nil {
		return err
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

// validateContentSentCrossFields is S2b's S6 fix round: a content.sent
// event that sets recipient_hash represents exactly one recipient, so
// pairing it with a recipient_count > 1 is a contradiction, rejected
// rather than silently stored. A no-op for every other event type, and
// for content.sent without both fields set.
func validateContentSentCrossFields(eventType string, out map[string]any) error {
	if eventType != "content.sent" {
		return nil
	}
	hash, hasHash := out["recipient_hash"].(string)
	count, hasCount := out["recipient_count"].(float64)
	if hasHash && hash != "" && hasCount && count > 1 {
		return badErr(CodeRedactionFailed, "data.recipient_hash represents exactly one recipient and cannot be paired with data.recipient_count > 1")
	}
	return nil
}

// scanForLeaks walks v recursively (v is always one of the types
// encoding/json produces into an `any`: string, float64, bool, nil,
// []any, or map[string]any — including when a test constructs a value by
// hand rather than through json.Unmarshal), checking every string value
// and every map key against hasControlChar/looksLikeEmail. path is used
// only to build a human-readable error message.
//
// emailMaskExempt (S2b's S5 fix round), when non-empty, is the ONE
// top-level `data` key whose value is exempt from this function's
// looksLikeEmail check specifically — every other field, including a
// NESTED occurrence of a key with the same name, keeps rejecting an
// embedded email-shaped substring outright. The control-character/
// invalid-UTF-8 checks are never exempted for any field.
func scanForLeaks(v any, path, emailMaskExempt string) error {
	switch val := v.(type) {
	case string:
		exempt := emailMaskExempt != "" && path == "data."+emailMaskExempt
		return checkLeakString(val, path, exempt)
	case map[string]any:
		for k, vv := range val {
			if err := checkLeakString(k, path+"."+k+" (key)", false); err != nil {
				return err
			}
			if err := scanForLeaks(vv, path+"."+k, emailMaskExempt); err != nil {
				return err
			}
		}
	case []any:
		for i, vv := range val {
			if err := scanForLeaks(vv, fmt.Sprintf("%s[%d]", path, i), emailMaskExempt); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkLeakString(s, path string, allowEmailShape bool) error {
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
	if !allowEmailShape && looksLikeEmail(s) {
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
