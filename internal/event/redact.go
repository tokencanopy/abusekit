package event

import (
	"encoding/json"
	"regexp"
	"unicode/utf8"
)

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
var schema = map[string]map[string]fieldSpec{
	"subject.created": {
		"channel":            {maxLen: 64},
		"email_domain_class": {maxLen: 32}, // webmail|corporate|disposable|unknown
		"identity_kind":      {maxLen: 64},
	},
	"subject.deleted": {
		"mode": {maxLen: 16}, // trash|permanent
	},
	"payment.attempt": {
		"outcome":      {maxLen: 32}, // succeeded|declined|blocked
		"reason":       {maxLen: 128},
		"funding":      {maxLen: 16}, // prepaid|debit|credit|unknown
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
		"class": {maxLen: 32}, // customer|internal|synthetic
	},
}

// emailRe flags a value that looks like an email address anywhere in a
// string. It is intentionally simple (not a full RFC 5322 matcher):
// redaction's job is to catch accidental PII, not to validate addresses,
// and a stricter matcher would only create false negatives that let real
// emails through.
var emailRe = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)

// Redact rewrites e.Data in place per the static schema for e.Type
// (design §4.3):
//   - For a known type: listed keys pass through capped (over-cap string
//     values are truncated, not rejected); unlisted keys are silently
//     dropped; a field with `skeleton: true` also gets a computed
//     `<key>_skeleton` sibling (any producer-supplied value under that
//     name is dropped as unlisted, then replaced by our own computation).
//   - For an unknown type: every key is kept as-is (no allow-listing to
//     apply), subject to the same email check and size cap.
//   - In every case: any string value anywhere in data that looks like an
//     email address fails the whole event with CodeRedactionFailed. This
//     runs before truncation/dropping so a producer cannot dodge it by
//     stuffing an address into an unlisted key.
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

	fields, known := schema[e.Type]
	out := make(map[string]any, len(e.Data))

	for k, v := range e.Data {
		s, isString := v.(string)
		if isString && emailRe.MatchString(s) {
			return badErr(CodeRedactionFailed, "data."+k+" looks like an email address")
		}

		if !known {
			out[k] = v
			continue
		}
		spec, listed := fields[k]
		if !listed {
			continue // unlisted keys dropped, not an error
		}
		if isString && spec.maxLen > 0 && len(s) > spec.maxLen {
			s = truncateUTF8(s, spec.maxLen)
			v = s
		}
		out[k] = v
		if spec.skeleton && isString {
			out[k+"_skeleton"] = Skeleton(s)
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
