package eval

import (
	"bytes"
	"context"
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
// through error messages, and datasetNeighbors.Evidence ignores it
// entirely (a dataset's neighbour graph is self-contained — see
// neighbors.go).
const evalTenant = "eval"

// eventRow is the wire shape of one event-replay events-file line (task
// brief): `{subject,type,at,data,links?}`. `id` is not part of that
// literal shape but IS required by internal/event.Event.Validate — a row
// that omits it gets a synthesized one (line-numbered, deterministic) so
// the real ingest Validate/Redact path (identical to what
// internal/worker's own replay tests use — see loadFixture/ingestFixture
// there) still runs unchanged over a row that never had to think about
// event ids.
type eventRow struct {
	ID      string         `json:"id"`
	Subject string         `json:"subject"`
	Type    string         `json:"type"`
	At      string         `json:"at"`
	Links   event.Links    `json:"links"`
	Data    map[string]any `json:"data"`
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

// LoadReplayDataset parses an event-replay pair (task brief / design
// §4.6's second corpus shape): eventsR is a JSONL file of
// `{subject,type,at,data,links?}` rows (internal/event's own wire
// vocabulary), labelsR is a JSONL file of
// `{subject,label,category?,source,decision_at:{<slice>:<RFC3339>}}`
// rows. brands feeds internal/feature.Extract's name_brand_match the
// same way it does in production (config/brands.yaml, loaded by the
// caller).
//
// For every label row, a Point is built for every Slice whose
// decision_at instant is resolvable:
//   - "full" always resolves — decision_at.full if present, else the
//     subject's last event's At plus 1ns (so every event counts).
//   - "first_send"/"early_15m" resolve only when decision_at names them;
//     a labels row that never mentions "first_send" simply has no Point
//     for that Slice (not a row error — task brief: "--slice
//     first_send|early_15m|full (default full)" implies not every corpus
//     defines every cut).
//
// A Point's features are rebuilt from events STRICTLY BEFORE its
// decision_at (e.At.Before(decisionAt)) — never at or after it, per the
// task brief's "rebuild each subject's feature vector from events
// strictly before the chosen decision_at slice". Same-tenant neighbour
// evidence (internal/feature.Neighbors) is resolved once from the WHOLE
// dataset's events/labels (see neighbors.go), not re-sliced per subject —
// mirroring how a live store's Neighbors query reads a neighbour's
// current state regardless of when the subject being scored is
// evaluated.
//
// Every malformed or unresolvable row — bad JSON, a missing
// subject/type/at, an event that fails internal/event's own
// Validate/Redact, a label naming a subject with zero events, an empty
// decision_at map — is collected as a RowError with its 1-based line
// number (events-file line numbers and labels-file line numbers are
// reported in the same, single []RowError slice, since both files can
// have mistakes; a caller that cares which file a given RowError came
// from can inspect Err's message, which always names it). LoadReplayDataset
// returns a non-nil error (a *SchemaError) whenever rowErrs is non-empty,
// mirroring LoadSnapshotCorpus.
func LoadReplayDataset(eventsR, labelsR io.Reader, brands feature.BrandSet) (Dataset, []RowError, error) {
	eventsBySubject, rowErrs, err := parseEventRows(eventsR)
	if err != nil {
		return Dataset{}, nil, err
	}
	for s := range eventsBySubject {
		sort.Slice(eventsBySubject[s], func(i, j int) bool { return eventsBySubject[s][i].At.Before(eventsBySubject[s][j].At) })
	}

	labels, labelErrs, err := parseLabelRows(labelsR)
	rowErrs = append(rowErrs, labelErrs...)
	if err != nil {
		return Dataset{}, nil, err
	}

	labelBySubject := make(map[string]string, len(labels))
	for _, l := range labels {
		labelBySubject[l.row.Subject] = l.row.Label
	}
	neighbors := newDatasetNeighbors(eventsBySubject, labelBySubject)

	subjects := make([]Subject, 0, len(labels))
	ctx := context.Background()
	for _, l := range labels {
		row := l.row
		events := eventsBySubject[row.Subject]
		if len(events) == 0 {
			rowErrs = append(rowErrs, RowError{Line: l.line, Err: fmt.Errorf("labels: subject %q has zero events in the events file", row.Subject)})
			continue
		}
		if len(row.DecisionAt) == 0 {
			rowErrs = append(rowErrs, RowError{Line: l.line, Err: fmt.Errorf("labels: subject %q has an empty decision_at", row.Subject)})
			continue
		}

		lastAt := events[len(events)-1].At
		points := map[Slice]Point{}
		resolvedAt := map[string]string{}
		var badRow bool
		for _, slice := range sliceOrder {
			raw, named := row.DecisionAt[string(slice)]
			var decisionAt time.Time
			switch {
			case named:
				t, err := parseRFC3339(raw)
				if err != nil {
					rowErrs = append(rowErrs, RowError{Line: l.line, Err: fmt.Errorf("labels: subject %q decision_at.%s: %w", row.Subject, slice, err)})
					badRow = true
					continue
				}
				decisionAt = t
			case slice == SliceFull:
				decisionAt = lastAt.Add(time.Nanosecond)
			default:
				continue // first_send/early_15m left unresolved: not an error, see doc comment
			}

			var filtered []event.Event
			for _, e := range events {
				if e.At.Before(decisionAt) {
					filtered = append(filtered, e)
				}
			}
			windows := feature.DefaultWindows(decisionAt)
			fr, err := feature.Extract(ctx, evalTenant, row.Subject, filtered, asOfNeighbors{n: neighbors, asOf: decisionAt}, windows, brands)
			if err != nil {
				rowErrs = append(rowErrs, RowError{Line: l.line, Err: fmt.Errorf("labels: subject %q slice %s: extract features: %w", row.Subject, slice, err)})
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
			rowErrs = append(rowErrs, RowError{Line: l.line, Err: fmt.Errorf("labels: subject %q resolved zero slices", row.Subject)})
			continue
		}

		subjects = append(subjects, Subject{
			ID:       row.Subject,
			Label:    row.Label,
			Category: row.Category,
			Source:   row.Source,
			Meta:     map[string]any{"decision_at": resolvedAt, "event_count": len(events)},
			Points:   points,
		})
	}

	if len(rowErrs) > 0 {
		return Dataset{}, rowErrs, &SchemaError{Rows: rowErrs}
	}
	return Dataset{Subjects: subjects, Replay: true}, nil, nil
}

// parseEventRows reads eventsR into a map of subject -> its raw events
// (validated and redacted via internal/event's real ingest path, exactly
// like internal/worker's own replay tests — see loadFixture/ingestFixture
// there), collecting a RowError per malformed line/event.
func parseEventRows(r io.Reader) (map[string][]event.Event, []RowError, error) {
	events := map[string][]event.Event{}
	var rowErrs []RowError
	err := scanJSONL(r, func(line int, raw []byte) error {
		var row eventRow
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&row); err != nil {
			rowErrs = append(rowErrs, RowError{Line: line, Err: fmt.Errorf("events: invalid JSON or unknown field: %w", err)})
			return nil
		}
		if row.Subject == "" || row.Type == "" || row.At == "" {
			rowErrs = append(rowErrs, RowError{Line: line, Err: fmt.Errorf("events: subject, type, and at are all required")})
			return nil
		}
		at, err := parseRFC3339(row.At)
		if err != nil {
			rowErrs = append(rowErrs, RowError{Line: line, Err: fmt.Errorf("events: at: %w", err)})
			return nil
		}
		id := row.ID
		if id == "" {
			id = fmt.Sprintf("replay-line-%d", line) // task brief's events-row shape omits id; synthesize one (see eventRow's doc comment)
		}
		e := event.Event{ID: id, Subject: row.Subject, Type: row.Type, At: at, Links: row.Links, Data: row.Data}
		// Validated against its OWN `at` as "now" (matching
		// internal/worker's ingestFixture): a replay of historical or
		// fictional-timestamped data has no business being checked
		// against the real wall clock's ±24h window.
		if err := e.Validate(event.ValidateOptions{Now: at}); err != nil {
			rowErrs = append(rowErrs, RowError{Line: line, Err: fmt.Errorf("events: %w", err)})
			return nil
		}
		if err := e.Redact(); err != nil {
			rowErrs = append(rowErrs, RowError{Line: line, Err: fmt.Errorf("events: %w", err)})
			return nil
		}
		events[row.Subject] = append(events[row.Subject], e)
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("eval: read events file: %w", err)
	}
	return events, rowErrs, nil
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
func parseLabelRows(r io.Reader) ([]lineLabelRow, []RowError, error) {
	var rows []lineLabelRow
	var rowErrs []RowError
	seen := map[string]int{}
	err := scanJSONL(r, func(line int, raw []byte) error {
		var row LabelRow
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&row); err != nil {
			rowErrs = append(rowErrs, RowError{Line: line, Err: fmt.Errorf("labels: invalid JSON or unknown field: %w", err)})
			return nil
		}
		if row.Subject == "" {
			rowErrs = append(rowErrs, RowError{Line: line, Err: fmt.Errorf("labels: subject is required")})
			return nil
		}
		if row.Label == "" {
			rowErrs = append(rowErrs, RowError{Line: line, Err: fmt.Errorf("labels: subject %q: label is required", row.Subject)})
			return nil
		}
		if row.Source == "" {
			rowErrs = append(rowErrs, RowError{Line: line, Err: fmt.Errorf("labels: subject %q: source is required", row.Subject)})
			return nil
		}
		if prev, dup := seen[row.Subject]; dup {
			rowErrs = append(rowErrs, RowError{Line: line, Err: fmt.Errorf("labels: duplicate subject %q (first seen at line %d)", row.Subject, prev)})
			return nil
		}
		seen[row.Subject] = line
		rows = append(rows, lineLabelRow{line: line, row: row})
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("eval: read labels file: %w", err)
	}
	return rows, rowErrs, nil
}
