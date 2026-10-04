package eval

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/tokencanopy/abusekit/internal/event"
	"github.com/tokencanopy/abusekit/internal/feature"
)

// evalTenant is the fixed pseudo-tenant used internally when calling
// internal/feature.Extract over a replay dataset. It never leaves this
// package and has no bearing on scoring: feature.Extract only threads it
// through error messages, and datasetNeighbors ignores it entirely (a
// dataset's neighbour graph is self-contained — see neighbors.go).
const evalTenant = "eval"

// labelEventType is the event-replay corpus's OWN convention (fix round
// B1) for "an operator/outcome label was recorded for this subject at
// this instant" — NOT a real design §4.3 vocabulary entry (production
// labels live in a separate `labels` table, never the `events` stream;
// see design §4.9). A replay corpus that wants core.linked_labelled_abusive_n
// to be exercised at all includes rows of this type; one that doesn't
// simply gets that feature held at 0 for every subject, which is honest
// (nothing "knew" a neighbour was labelled). Data shape: `{"label":
// "<value>"}`, value compared against whichever rule is being scored's
// BenignLabel (see neighbors.go's labelledAsOf) — never a hardcoded
// "abusive".
const labelEventType = "label"

// eventRow is the wire shape of one event-replay events-file line (task
// brief): `{subject,type,at,data,links?}`. `id` is not part of that
// literal shape but IS required by internal/event.Event.Validate — a row
// that omits it gets a synthesized one (line-numbered, deterministic) so
// the real ingest Validate/Redact path (identical to what
// internal/worker's own replay tests use — see loadFixture/ingestFixture
// there) still runs unchanged over a row that never had to think about
// event ids.
type eventRow struct {
	Producer string         `json:"producer"`
	ID       string         `json:"id"`
	Subject  string         `json:"subject"`
	Type     string         `json:"type"`
	At       string         `json:"at"`
	Links    event.Links    `json:"links"`
	Data     map[string]any `json:"data"`
}

// LabelRow is the wire shape of one event-replay labels-file line (task
// brief): `{subject,label,category?,source,decision_at:{<slice>:
// <RFC3339>}}`. Exported so a corpus-building tool (eval/gen, or a
// private-corpus builder outside this repo) can construct rows
// programmatically and json.Marshal them directly, rather than
// hand-assembling the JSON.
type LabelRow struct {
	Subject    string            `json:"subject"`
	Label      string            `json:"label"`
	Category   string            `json:"category"`
	Source     string            `json:"source"`
	DecisionAt map[string]string `json:"decision_at"`
}

// parseRFC3339 parses s as RFC3339 (with or without fractional seconds),
// preserving whatever UTC-offset it carries — deliberately NOT
// normalized to UTC before any caller that also runs
// internal/event.Event.Validate over it, since Validate's own job is to
// reject a non-zero offset (design §4.3: "at must be UTC"); pre-
// normalizing here would silently launder exactly the input Validate
// exists to catch.
func parseRFC3339(s string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, s)
}

// isTextFieldKey reports whether a Data key is one of the text-bearing
// fields a rule's `text:` list can select (design's shipped example:
// `text: [subject_line_skeleton, first_link_host]`; internal/event's
// redaction schema also produces `name_skeleton` for
// resource.created/deleted). Generic by suffix rather than an exact
// enumerated list, so a future skeleton-bearing field flows through
// without a change here.
func isTextFieldKey(k string) bool {
	return strings.HasSuffix(k, "_skeleton") || k == "first_link_host"
}

// extractTextFields collects every text-bearing field value across
// events, keyed by field name, in event order — the same convention
// LoadSnapshotCorpus's input.text uses, so a rule's `text:` selection
// works identically against either corpus shape.
func extractTextFields(events []event.Event) map[string][]string {
	var out map[string][]string
	for _, e := range events {
		for k, v := range e.Data {
			if !isTextFieldKey(k) {
				continue
			}
			s, ok := v.(string)
			if !ok || s == "" {
				continue
			}
			if out == nil {
				out = map[string][]string{}
			}
			out[k] = append(out[k], s)
		}
	}
	return out
}

// ReplayInput names the two files LoadReplayDataset reads (fix round
// P1): a struct rather than four positional parameters, so each Reader
// is paired unambiguously with the real path RowError.Source should
// report — a bug in the original S4 landing concatenated both paths
// into one bogus label and reported labels-file line numbers against
// events-file rows (and vice versa) whenever both files had mistakes.
// EventsPath/LabelsPath are used ONLY for error messages; nothing here
// re-reads the file from disk (Events/Labels are read exactly once,
// from the given Readers).
type ReplayInput struct {
	EventsPath string
	Events     io.Reader
	LabelsPath string
	Labels     io.Reader
}

// LoadReplayDataset parses an event-replay pair (task brief / design
// §4.6's second corpus shape). benignLabel is whichever rule this
// dataset will be scored against's BenignLabel (fix round B1: needed to
// resolve `label`-typed events' positive/negative class — see
// neighbors.go's labelledAsOf); pass config.Rule.BenignLabel, never a
// literal. webmail is threaded straight through to every feature.Extract
// call below (S2b's email.webmail_recipient_share/email.webmail_sends_1h need it);
// the zero value (feature.WebmailSet{}) matches every domain as
// non-webmail, same as passing no webmail config at all.
//
// For every label row, a Point is built for every Slice whose
// decision_at instant is resolvable and has at least one qualifying
// event:
//   - "full" always resolves — decision_at.full if present, else the
//     subject's last event's At plus 1ns (so every event counts).
//   - "first_send"/"early_15m" resolve only when decision_at names them;
//     a labels row that never mentions "first_send" simply has no Point
//     for that Slice (not a row error — task brief: "--slice
//     first_send|early_15m|full (default full)" implies not every corpus
//     defines every cut).
//   - fix round B2: a resolved decision_at with ZERO qualifying events
//     (every one of the subject's events falls at or after it) produces
//     NO Point either, rather than a spuriously all-zero-feature one —
//     Run reports this subject×slice as `missing_slice`, excluded from
//     precision and counted as a miss for recall (see eval.go's
//     scoredRecord handling), never silently scored as a confident "low".
//
// A Point's features are rebuilt from events STRICTLY BEFORE its
// decision_at (e.At.Before(decisionAt)) — never at or after it. Same-
// tenant neighbour evidence (internal/feature.Neighbors) is resolved
// from an index built once over the WHOLE dataset's events (see
// neighbors.go), but evaluated AS OF each Point's own decisionAt, never
// as of the end of the dataset.
//
// Fix round B2 also REJECTS (a RowError, not a soft missing_slice) any
// label row whose resolved slices are out of chronological order
// (first_send <= early_15m <= full, for whichever of the three are
// present) or whose decision_at falls AFTER the subject's own permanent
// deletion — either is an authoring mistake in the corpus, not a
// legitimate "nothing happened yet" case.
//
// Every malformed or unresolvable row is collected as a RowError naming
// its REAL source file (in.EventsPath or in.LabelsPath) and 1-based line
// number (fix round P1). The returned Dataset always contains every
// subject that DID resolve successfully, even when rowErrs is non-empty
// (fix round P2 — see LoadSnapshotCorpus's identical note); a caller
// wanting S4's original strict behavior treats a non-nil error as fatal
// and ignores the Dataset, unchanged.
func LoadReplayDataset(in ReplayInput, brands feature.BrandSet, webmail feature.WebmailSet, benignLabel string) (Dataset, []RowError, error) {
	eventsBySubject, labelEvents, skippedEventSubjects, rowErrs, err := parseEventRows(in.EventsPath, in.Events)
	if err != nil {
		return Dataset{}, nil, err
	}
	for s := range eventsBySubject {
		sort.Slice(eventsBySubject[s], func(i, j int) bool { return event.Less(eventsBySubject[s][i], eventsBySubject[s][j]) })
	}

	labels, labelErrs, err := parseLabelRows(in.LabelsPath, in.Labels)
	rowErrs = append(rowErrs, labelErrs...)
	if err != nil {
		return Dataset{}, nil, err
	}

	neighbors := newDatasetNeighbors(eventsBySubject, labelEvents)

	subjects := make([]Subject, 0, len(labels))
	subjectIDs := make([]string, 0, len(labels))
	ctx := context.Background()
	for _, l := range labels {
		row := l.row
		events := eventsBySubject[row.Subject]
		if len(events) == 0 {
			rowErrs = append(rowErrs, RowError{Source: in.LabelsPath, Line: l.line, Code: "zero_events", Err: fmt.Errorf("labels: subject %q has zero events in the events file", row.Subject)})
			continue
		}
		if len(row.DecisionAt) == 0 {
			rowErrs = append(rowErrs, RowError{Source: in.LabelsPath, Line: l.line, Code: "empty_decision_at", Err: fmt.Errorf("labels: subject %q has an empty decision_at", row.Subject)})
			continue
		}

		lastAt := events[len(events)-1].At
		deletedAt, hasDeletion := permanentDeletionAt(events)

		resolved := map[Slice]time.Time{}
		var badRow bool
		for _, slice := range sliceOrder {
			raw, named := row.DecisionAt[string(slice)]
			switch {
			case named:
				t, err := parseRFC3339(raw)
				if err != nil {
					rowErrs = append(rowErrs, RowError{Source: in.LabelsPath, Line: l.line, Code: "bad_decision_at", Err: fmt.Errorf("labels: subject %q decision_at.%s: %w", row.Subject, slice, err)})
					badRow = true
					continue
				}
				resolved[slice] = t
			case slice == SliceFull:
				resolved[slice] = lastAt.Add(time.Nanosecond)
			}
		}
		if badRow {
			continue
		}

		// Fix round B2: chronological order first_send <= early_15m <=
		// full for whichever are present, and no resolved slice may fall
		// after the subject's own permanent deletion.
		if err := validateSliceOrder(resolved); err != nil {
			rowErrs = append(rowErrs, RowError{Source: in.LabelsPath, Line: l.line, Code: "slice_order", Err: fmt.Errorf("labels: subject %q: %w", row.Subject, err)})
			continue
		}
		if hasDeletion {
			if err := validateBeforeDeletion(resolved, deletedAt); err != nil {
				rowErrs = append(rowErrs, RowError{Source: in.LabelsPath, Line: l.line, Code: "decision_after_deletion", Err: fmt.Errorf("labels: subject %q: %w", row.Subject, err)})
				continue
			}
		}

		points := map[Slice]Point{}
		resolvedAt := map[string]string{}
		for _, slice := range sliceOrder {
			decisionAt, ok := resolved[slice]
			if !ok {
				continue
			}
			var filtered []event.Event
			for _, e := range events {
				if e.At.Before(decisionAt) {
					filtered = append(filtered, e)
				}
			}
			if len(filtered) == 0 {
				// Fix round B2: zero qualifying events is `missing_slice`
				// at scoring time, not an all-zero-feature Point that
				// Run would confidently score as "low".
				continue
			}
			windows := feature.DefaultWindows(decisionAt)
			fr, err := feature.Extract(ctx, evalTenant, row.Subject, filtered, asOfNeighbors{n: neighbors, asOf: decisionAt, benignLabel: benignLabel}, windows, brands, webmail)
			if err != nil {
				rowErrs = append(rowErrs, RowError{Source: in.LabelsPath, Line: l.line, Code: "extract_failed", Err: fmt.Errorf("labels: subject %q slice %s: extract features: %w", row.Subject, slice, err)})
				badRow = true
				continue
			}
			points[slice] = Point{Features: fr.Features.Map(), Text: extractTextFields(filtered)}
			resolvedAt[string(slice)] = decisionAt.UTC().Format(time.RFC3339Nano)
		}
		if badRow {
			continue
		}
		if len(points) == 0 {
			rowErrs = append(rowErrs, RowError{Source: in.LabelsPath, Line: l.line, Code: "zero_slices", Err: fmt.Errorf("labels: subject %q resolved zero slices with any qualifying events", row.Subject)})
			continue
		}

		subjects = append(subjects, Subject{
			ID:               row.Subject,
			Label:            row.Label,
			Category:         row.Category,
			Source:           row.Source,
			Meta:             map[string]any{"decision_at": resolvedAt, "event_count": len(events)},
			Points:           points,
			HasSkippedEvents: skippedEventSubjects[row.Subject], // fix round T2
		})
		subjectIDs = append(subjectIDs, row.Subject)
	}

	// Fix round S7: split derived from a keyed hash of the subject's link
	// cluster (a connected component over every link kind present in the
	// dataset), falling back to the subject id alone when it shares no
	// link with anyone. TODO(S3 gap, unrelated to this fix): the STORE's
	// own corpus_examples.split (internal/serve's snapshotCorpusExample)
	// is still per-subject, not per-cluster — a real churn chain's
	// incarnations can land in different splits there. This eval-side
	// split is independent of that and does not fix it.
	splits := computeSplits(eventsBySubject, subjectIDs)
	for i := range subjects {
		subjects[i].Split = splits[subjects[i].ID]
	}

	dataset := Dataset{Subjects: subjects, Replay: true}
	if len(rowErrs) > 0 {
		return dataset, rowErrs, &SchemaError{Rows: rowErrs}
	}
	return dataset, nil, nil
}

// permanentDeletionAt returns the earliest permanent subject.deleted
// event's At among events, if any.
func permanentDeletionAt(events []event.Event) (time.Time, bool) {
	var at time.Time
	var ok bool
	for _, e := range events {
		if e.Type != "subject.deleted" {
			continue
		}
		if mode, o := dataStringField(e.Data, "mode"); !o || mode != "permanent" {
			continue
		}
		if !ok || e.At.Before(at) {
			at = e.At
			ok = true
		}
	}
	return at, ok
}

// validateSliceOrder checks first_send <= early_15m <= full for
// whichever of resolved's three keys are present (fix round B2).
func validateSliceOrder(resolved map[Slice]time.Time) error {
	fs, hasFS := resolved[SliceFirstSend]
	e15, hasE15 := resolved[SliceEarly15m]
	full, hasFull := resolved[SliceFull]
	if hasFS && hasE15 && fs.After(e15) {
		return fmt.Errorf("decision_at.first_send (%s) is after decision_at.early_15m (%s)", fs.Format(time.RFC3339), e15.Format(time.RFC3339))
	}
	if hasE15 && hasFull && e15.After(full) {
		return fmt.Errorf("decision_at.early_15m (%s) is after decision_at.full (%s)", e15.Format(time.RFC3339), full.Format(time.RFC3339))
	}
	if hasFS && hasFull && fs.After(full) {
		return fmt.Errorf("decision_at.first_send (%s) is after decision_at.full (%s)", fs.Format(time.RFC3339), full.Format(time.RFC3339))
	}
	return nil
}

// validateBeforeDeletion checks that no resolved slice falls MEANINGFULLY
// after the subject's own permanent deletion (fix round B2) — the
// allowed ceiling is deletedAt plus 1ns, not deletedAt itself, since
// "full" is routinely computed as "the last event's At plus 1ns" (see
// LoadReplayDataset), and the last event legitimately IS the deletion
// itself for a subject whose whole history ends there (every churn
// fixture). That one-nanosecond "just barely captures the deletion event
// as part of its own history" case is not an authoring mistake; a
// decision_at set hours or days after the deletion is.
func validateBeforeDeletion(resolved map[Slice]time.Time, deletedAt time.Time) error {
	ceiling := deletedAt.Add(time.Nanosecond)
	for _, slice := range sliceOrder {
		t, ok := resolved[slice]
		if !ok {
			continue
		}
		if t.After(ceiling) {
			return fmt.Errorf("decision_at.%s (%s) is after the subject's own permanent deletion (%s)", slice, t.Format(time.RFC3339), deletedAt.Format(time.RFC3339))
		}
	}
	return nil
}

// stripNullFields deletes every key in data whose JSON value was `null`
// (fix round P3): encoding/json decodes a JSON null into a Go nil for a
// map[string]any, which internal/event.Redact treats as a HARD FAILURE
// for any listed field (its own R3 fix round: "a null is rejected for
// every listed field"). A real private corpus can legitimately carry an
// explicit null for an optional field it never got around to filling in
// (e.g. content.verdict's score/category when a message was never
// scanned) — this loader's own choice (documented, tested) is
// absent-is-null: treat that exactly like the key being omitted
// entirely, which Redact already handles gracefully (an omitted listed
// field is simply not in the output, no error). internal/event itself is
// NOT changed — every other caller of Redact (production ingest, the
// worker's own replay tests) keeps null-is-a-hard-failure, since a real
// producer sending an explicit null is a bug worth rejecting loudly;
// only THIS loader, reading a pre-existing offline corpus it can't ask
// to fix itself, is lenient.
func stripNullFields(data map[string]any) {
	for k, v := range data {
		if v == nil {
			delete(data, k)
		}
	}
}

// parseEventRows reads eventsR into (a) a map of subject -> its raw
// domain events (validated and redacted via internal/event's real
// ingest path, exactly like internal/worker's own replay tests) and (b)
// every `label`-typed row (fix round B1 — see labelEventType), kept
// OUT of (a) so it can never be read by feature.Extract as if it were
// real account activity.
// peekSubject leniently recovers a "subject" field from a raw JSON line
// that failed STRICT decoding (fix round T2) — used only to attribute a
// dropped row to a subject for HasSkippedEvents tracking; a truly
// unparseable line (invalid JSON syntax, not just an extra/unknown
// field) yields "", and that row simply can't be attributed to anyone.
func peekSubject(raw []byte) string {
	var probe struct {
		Subject string `json:"subject"`
	}
	if json.Unmarshal(raw, &probe) == nil {
		return probe.Subject
	}
	return ""
}

func parseEventRows(path string, r io.Reader) (map[string][]event.Event, map[string][]labelledAt, map[string]bool, []RowError, error) {
	events := map[string][]event.Event{}
	labelEvents := map[string][]labelledAt{}
	// skippedSubjects (fix round T2) collects every subject that had at
	// least one DOMAIN-event row dropped — never a `label`-typed row,
	// which isn't part of feature-relevant activity history at all (see
	// the bad_label_event case below, which deliberately does not taint
	// the subject). LoadReplayDataset sets Subject.HasSkippedEvents from
	// this so such a subject is never scored on its surviving partial
	// history, in ANY --skip-invalid run.
	skippedSubjects := map[string]bool{}
	var rowErrs []RowError
	err := scanJSONL(r, func(line int, raw []byte) error {
		fail := func(subject, code, format string, args ...any) error {
			rowErrs = append(rowErrs, RowError{Source: path, Line: line, Code: code, Err: fmt.Errorf(format, args...)})
			if subject != "" {
				skippedSubjects[subject] = true
			}
			return nil
		}
		var row eventRow
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&row); err != nil {
			return fail(peekSubject(raw), "bad_json", "events: invalid JSON or unknown field: %w", err)
		}
		var trailing any
		if err := dec.Decode(&trailing); err != io.EOF {
			return fail(row.Subject, "bad_json", "events: expected exactly one JSON object per line")
		}
		if row.Subject == "" || row.Type == "" || row.At == "" {
			return fail(row.Subject, "missing_field", "events: subject, type, and at are all required")
		}
		at, err := parseRFC3339(row.At)
		if err != nil {
			return fail(row.Subject, "bad_at", "events: at: %w", err)
		}

		id := row.ID
		if id == "" {
			id = fmt.Sprintf("replay-line-%d", line)
		}
		if row.Type == labelEventType {
			value, ok := dataStringField(row.Data, "label")
			if !ok || value == "" {
				// Deliberately NOT tainted via skippedSubjects: a `label`
				// event is never part of feature-relevant activity
				// history (see labelEventType's own doc comment), so
				// dropping one can't produce a partial ACTIVITY history —
				// only a gap in core.linked_labelled_abusive_n evidence, a
				// different and much softer concern T2 doesn't ask for.
				rowErrs = append(rowErrs, RowError{Source: path, Line: line, Code: "bad_label_event", Err: fmt.Errorf("events: subject %q: a %q event needs a non-empty string data.label", row.Subject, labelEventType)})
				return nil
			}
			labelEvents[row.Subject] = append(labelEvents[row.Subject], labelledAt{producer: row.Producer, id: id, at: at, value: value})
			return nil
		}

		stripNullFields(row.Data) // fix round P3: absent-is-null, before Redact ever sees it
		e := event.Event{Producer: row.Producer, ID: id, Subject: row.Subject, Type: row.Type, At: at, Links: row.Links, Data: row.Data}
		// Validated against its OWN `at` as "now" (matching
		// internal/worker's ingestFixture): a replay of historical or
		// fictional-timestamped data has no business being checked
		// against the real wall clock's ±24h window.
		if err := e.Validate(event.ValidateOptions{Now: at}); err != nil {
			return fail(row.Subject, "validate_failed", "events: %w", err)
		}
		if err := e.Redact(); err != nil {
			return fail(row.Subject, "redact_failed", "events: %w", err)
		}
		events[row.Subject] = append(events[row.Subject], e)
		return nil
	})
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("eval: read events file %s: %w", path, err)
	}
	return events, labelEvents, skippedSubjects, rowErrs, nil
}

type lineLabelRow struct {
	line int
	row  LabelRow
}

// parseLabelRows reads labelsR, validating each row's required fields
// (subject, label, source non-empty) and rejecting a duplicate subject
// outright (design has no notion of two labels-file rows for the same
// subject in one dataset — a caller that wants to relabel re-runs the
// export, it doesn't hand the harness two answers for one subject).
func parseLabelRows(path string, r io.Reader) ([]lineLabelRow, []RowError, error) {
	var rows []lineLabelRow
	var rowErrs []RowError
	seen := map[string]int{}
	err := scanJSONL(r, func(line int, raw []byte) error {
		fail := func(code, format string, args ...any) error {
			rowErrs = append(rowErrs, RowError{Source: path, Line: line, Code: code, Err: fmt.Errorf(format, args...)})
			return nil
		}
		var row LabelRow
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&row); err != nil {
			return fail("bad_json", "labels: invalid JSON or unknown field: %w", err)
		}
		if row.Subject == "" {
			return fail("missing_subject", "labels: subject is required")
		}
		if row.Label == "" {
			return fail("missing_label", "labels: subject %q: label is required", row.Subject)
		}
		if row.Source == "" {
			return fail("missing_source", "labels: subject %q: source is required", row.Subject)
		}
		if prev, dup := seen[row.Subject]; dup {
			return fail("duplicate_subject", "labels: duplicate subject %q (first seen at line %d)", row.Subject, prev)
		}
		seen[row.Subject] = line
		rows = append(rows, lineLabelRow{line: line, row: row})
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("eval: read labels file %s: %w", path, err)
	}
	return rows, rowErrs, nil
}

// computeSplits assigns each of subjectIDs a "train"/"test" split (fix
// round S7) via a keyed hash of its LINK CLUSTER: a connected component
// over every link kind present in eventsBySubject (union-find on shared
// (kind, hash) pairs, unrestricted by neighbors.go's default-kinds
// policy — that policy is about which links count as SCORING evidence,
// not about which ones define a cluster for split purposes), keyed by
// the lexicographically smallest subject id in the component so the
// whole cluster lands in the same split together. A subject sharing no
// link with anyone is its own one-member component, falling back to a
// hash of its own id alone.
func computeSplits(eventsBySubject map[string][]event.Event, subjectIDs []string) map[string]string {
	parent := map[string]string{}
	var find func(string) string
	find = func(x string) string {
		if _, ok := parent[x]; !ok {
			parent[x] = x
			return x
		}
		if parent[x] != x {
			parent[x] = find(parent[x])
		}
		return parent[x]
	}
	union := func(a, b string) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[ra] = rb
		}
	}

	byKindHash := map[string]map[string][]string{}
	for subject, events := range eventsBySubject {
		find(subject)
		for _, e := range events {
			for _, kh := range e.Links.Kinds() {
				if byKindHash[kh.Kind] == nil {
					byKindHash[kh.Kind] = map[string][]string{}
				}
				byKindHash[kh.Kind][kh.Hash] = append(byKindHash[kh.Kind][kh.Hash], subject)
			}
		}
	}
	for _, byHash := range byKindHash {
		for _, subs := range byHash {
			for i := 1; i < len(subs); i++ {
				union(subs[0], subs[i])
			}
		}
	}

	members := map[string][]string{}
	for _, id := range subjectIDs {
		root := find(id)
		members[root] = append(members[root], id)
	}
	splits := make(map[string]string, len(subjectIDs))
	for _, group := range members {
		sort.Strings(group)
		clusterKey := group[0]
		split := splitForCluster(clusterKey)
		for _, id := range group {
			splits[id] = split
		}
	}
	return splits
}

// splitForCluster is an 80/20 keyed-hash bucketing of clusterKey — the
// eval package's own convention (a sha256 of a fixed internal prefix,
// not a per-tenant HMAC secret; there is nothing sensitive to protect
// here, only a stable, non-gameable-by-guessing-a-subject-id bucketing),
// mirroring internal/serve's splitFor in spirit (same 80/20 cut, keyed
// rather than a bare hash) but computed over a link CLUSTER key, not a
// bare subject id.
func splitForCluster(clusterKey string) string {
	sum := sha256.Sum256([]byte("eval-split:" + clusterKey))
	bucket := binary.BigEndian.Uint64(sum[:8]) % 100
	if bucket < 80 {
		return "train"
	}
	return "test"
}
