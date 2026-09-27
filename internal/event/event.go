// Package event defines abusekit's wire vocabulary: the typed Event a
// producer sends to POST /v1/events, its structural validation, and the
// static per-type redaction schema (see redact.go). Nothing in this
// package talks to Postgres or a scorer — it is the boundary that turns an
// arbitrary JSON body into a value the rest of abusekit can trust.
//
// Validate and Redact are deliberately separate: Validate checks shape
// (id, subject, type, timestamp, links) and never inspects data; Redact
// only ever touches data. The two error sets are disjoint by design so a
// caller can always attribute a rejected item to exactly one cause, which
// is what the per-item codes in the design (§4.3) require.
package event

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Size and format limits from design §4.3. Exported so producers and tests
// can reference the same numbers instead of re-guessing them.
const (
	MaxIDLen      = 64
	MaxSubjectLen = 256
	MaxTypeLen    = 64
	MaxDataBytes  = 8 * 1024 // 8 KiB, post-redaction

	// LinkHashLen is the hex-encoded length of an HMAC-SHA256 link hash
	// (32 bytes -> 64 hex chars). The five keyed-hash link fields must be
	// either empty or exactly this long.
	LinkHashLen = 64
	// MaxASNLen bounds the one link field that is NOT a keyed hash (an
	// autonomous system number is already coarse, non-identifying routing
	// metadata, so it travels in the clear rather than hashed).
	MaxASNLen = 32

	// skewDefault / skewBackfill are the clock-skew windows from §4.3 and
	// §5 ("±24h on events (except backfill scope)").
	skewDefault  = 24 * time.Hour
	skewBackfill = 0 // backfill scope: no skew check at all (see Validate)
)

// typeRe enforces the event.Type grammar from §4.3: lower-case ASCII
// letters, underscore and dot, e.g. "resource.created".
var typeRe = regexp.MustCompile(`^[a-z_.]+$`)

// hexRe matches a lower-case hex string, used to validate the five keyed
// hash link fields.
var hexRe = regexp.MustCompile(`^[0-9a-f]+$`)

// Links carries the keyed-hash identity signals a producer attaches to an
// event (design §4.2/§4.3). Every field is optional; a present field must
// match its expected format or the event is rejected with CodeBadLinks.
//
// The five *Hash fields are HMAC-SHA256 under a per-tenant key the
// producer holds — abusekit never sees the raw value, only the hash. ASN
// is carried in the clear: an autonomous system number is already coarse
// routing metadata, not personal data, so hashing it would only destroy
// its usefulness as a join key without adding privacy.
type Links struct {
	EmailHash           string `json:"email_hash,omitempty"`
	CardFingerprintHash string `json:"card_fingerprint_hash,omitempty"`
	IP24Hash            string `json:"ip24_hash,omitempty"`
	ASN                 string `json:"asn,omitempty"`
	UAHash              string `json:"ua_hash,omitempty"`
	DeviceHash          string `json:"device_hash,omitempty"`
}

// IsEmpty reports whether no link key was set.
func (l Links) IsEmpty() bool {
	return l == Links{}
}

// Kinds returns the set (kind, hash) pairs present on l, in a fixed order.
// Used by the store to upsert one links row per present key.
func (l Links) Kinds() []struct{ Kind, Hash string } {
	out := make([]struct{ Kind, Hash string }, 0, 6)
	add := func(kind, hash string) {
		if hash != "" {
			out = append(out, struct{ Kind, Hash string }{kind, hash})
		}
	}
	add("email_hash", l.EmailHash)
	add("card_fingerprint_hash", l.CardFingerprintHash)
	add("ip24_hash", l.IP24Hash)
	add("asn", l.ASN)
	add("ua_hash", l.UAHash)
	add("device_hash", l.DeviceHash)
	return out
}

// Event is the validated, in-memory form of one item from a
// POST /v1/events batch. Construct it from the wire JSON, then call
// Validate (structure) and Redact (data) before it reaches the store.
type Event struct {
	ID      string         `json:"id"`
	Subject string         `json:"subject"`
	Type    string         `json:"type"`
	At      time.Time      `json:"at"`
	Links   Links          `json:"links,omitempty"`
	Data    map[string]any `json:"data,omitempty"`
}

// Code enumerates the per-item rejection reasons from design §4.3.
// `conflict` and `duplicate` are deliberately not produced by this
// package: they depend on what the store already has for
// (tenant, producer, id), which Validate/Redact cannot see.
type Code string

const (
	CodeBadID           Code = "bad_id"
	CodeBadSubject      Code = "bad_subject"
	CodeBadType         Code = "bad_type"
	CodeBadTimestamp    Code = "bad_timestamp"
	CodeBadLinks        Code = "bad_links"
	CodeTooLarge        Code = "too_large"
	CodeRedactionFailed Code = "redaction_failed"
	CodeConflict        Code = "conflict"  // store-assigned only
	CodeDuplicate       Code = "duplicate" // store-assigned only
)

// ValidationError pairs one Code with a human-readable message. It is the
// only error type this package returns from Validate/Redact, so callers
// can type-assert once and always find a Code.
type ValidationError struct {
	Code    Code
	Message string
}

func (e *ValidationError) Error() string { return string(e.Code) + ": " + e.Message }

func badErr(code Code, msg string) *ValidationError {
	return &ValidationError{Code: code, Message: msg}
}

// ValidateOptions controls the parts of Validate that depend on the
// caller's context rather than the event itself.
type ValidateOptions struct {
	// Now is the reference time for the clock-skew check. Callers pass the
	// real wall clock in production and a fixed time in tests.
	Now time.Time
	// Backfill widens (removes) the timestamp skew check, per §4.3: "±24h
	// unless backfill scope". Only a producer key with the `backfill`
	// scope may set this (enforced by the HTTP layer in S3, not here).
	Backfill bool
}

// Validate checks e's structural fields — id, subject, type, timestamp,
// links — against design §4.3. It does not look at e.Data; call Redact
// separately for that. Returns the first violation found, in the order
// id, subject, type, timestamp, links, so a caller mapping errors to the
// enumerated codes never has to pick among several simultaneous problems.
//
// Validate never mutates e and never panics.
func (e *Event) Validate(opts ValidateOptions) error {
	if e.ID == "" || len(e.ID) > MaxIDLen || hasControlChar(e.ID) || !utf8.ValidString(e.ID) {
		return badErr(CodeBadID, "id must be 1..64 bytes of valid UTF-8 with no control characters")
	}
	if e.Subject == "" || len(e.Subject) > MaxSubjectLen || hasControlChar(e.Subject) || !utf8.ValidString(e.Subject) {
		return badErr(CodeBadSubject, "subject must be 1..256 bytes of valid UTF-8 with no control characters")
	}
	if e.Type == "" || len(e.Type) > MaxTypeLen || !typeRe.MatchString(e.Type) {
		return badErr(CodeBadType, "type must match ^[a-z_.]+$ and be 1..64 bytes")
	}
	if e.At.IsZero() {
		return badErr(CodeBadTimestamp, "at is required and must be RFC3339 UTC")
	}
	// S6: design says `at` is "RFC3339 UTC", not merely RFC3339 — reject a
	// non-zero offset outright rather than silently accepting it. Without
	// this, the SAME instant sent as "...Z" vs "...+02:00" round-trips
	// through Go's time.Time with different Location metadata and hashes
	// differently in BodyHash (proven: misreported as `conflict` instead
	// of `duplicate`); rejecting at the boundary is simpler and safer than
	// relying on every downstream consumer to normalize consistently.
	if _, offset := e.At.Zone(); offset != 0 {
		return badErr(CodeBadTimestamp, "at must be UTC (zero offset), e.g. \"...Z\", not a non-zero offset")
	}
	if !opts.Backfill {
		now := opts.Now
		if now.IsZero() {
			now = time.Now().UTC()
		}
		delta := now.Sub(e.At)
		if delta < 0 {
			delta = -delta
		}
		if delta > skewDefault {
			return badErr(CodeBadTimestamp, "at is outside the ±24h clock-skew window")
		}
	}
	if err := validateLinks(e.Links); err != nil {
		return err
	}
	return nil
}

// BodyHash returns a stable hex-encoded SHA-256 digest of e's full body
// (subject, type, at, links, and the — already redacted — data). The
// store uses this to tell a `duplicate` replay (same id, identical body)
// from a `conflict` (same id, different body): design §4.3.
//
// Call BodyHash only after Validate and Redact have both succeeded, so
// two producers that send semantically identical events hash identically
// regardless of which raw JSON keys they happened to include (redaction
// has already dropped anything unlisted). json.Marshal serializes map
// keys in sorted order, so the digest is stable across Go map iteration.
func (e *Event) BodyHash() (string, error) {
	// S6: normalize to UTC before hashing, defense-in-depth alongside
	// Validate's own rejection of a non-UTC offset — a caller that builds
	// an Event directly without going through Validate (some store tests
	// do this deliberately) still gets a hash that depends only on the
	// instant, never on which equivalent offset representation was used.
	b, err := json.Marshal(struct {
		Subject string         `json:"subject"`
		Type    string         `json:"type"`
		At      time.Time      `json:"at"`
		Links   Links          `json:"links"`
		Data    map[string]any `json:"data"`
	}{e.Subject, e.Type, e.At.UTC(), e.Links, e.Data})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func validateLinks(l Links) error {
	hashFields := []struct {
		name, val string
	}{
		{"email_hash", l.EmailHash},
		{"card_fingerprint_hash", l.CardFingerprintHash},
		{"ip24_hash", l.IP24Hash},
		{"ua_hash", l.UAHash},
		{"device_hash", l.DeviceHash},
	}
	for _, f := range hashFields {
		if f.val == "" {
			continue
		}
		if len(f.val) != LinkHashLen || !hexRe.MatchString(f.val) {
			return badErr(CodeBadLinks, "links."+f.name+" must be "+strconv.Itoa(LinkHashLen)+" lower-case hex characters")
		}
	}
	if l.ASN != "" {
		// R1 (round 2): a NUL/DEL byte in ASN passed here (only length and
		// whitespace were checked) and killed the whole batch at INSERT
		// time (Postgres 22P05) — the same class of leak B1 already closed
		// for id/subject/data.
		if len(l.ASN) > MaxASNLen || strings.ContainsAny(l.ASN, " \t\r\n") || hasControlChar(l.ASN) || !utf8.ValidString(l.ASN) {
			return badErr(CodeBadLinks, "links.asn must be non-empty, <=32 bytes of valid UTF-8, and contain no whitespace or control characters")
		}
	}
	return nil
}
