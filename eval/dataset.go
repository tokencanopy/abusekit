package eval

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// maxJSONLLineBytes bounds one JSONL row (dataset, labels, or corpus
// files) — generous relative to internal/event's own 8 KiB post-redaction
// data cap, since an unredacted replay row (raw `data`, no cap applied
// yet) or a corpus row's full event_slice can run larger.
const maxJSONLLineBytes = 1 << 20 // 1 MiB

// RowError is one line's schema-validation failure (task brief: "report
// per-row errors with the line number"). Line is 1-based, matching what
// an editor or `sed -n '<n>p'` would show.
type RowError struct {
	Line int
	Err  error
}

func (e RowError) Error() string { return fmt.Sprintf("line %d: %v", e.Line, e.Err) }

// SchemaError collects every RowError found while loading a dataset or
// labels file — LoadX returns every violation at once (rather than
// stopping at the first) so a caller fixing a corpus doesn't have to
// re-run the loader once per mistake.
type SchemaError struct {
	Rows []RowError
}

func (e *SchemaError) Error() string {
	if len(e.Rows) == 1 {
		return "eval: 1 row failed schema validation: " + e.Rows[0].Error()
	}
	msg := fmt.Sprintf("eval: %d rows failed schema validation:", len(e.Rows))
	for _, r := range e.Rows {
		msg += "\n  " + r.Error()
	}
	return msg
}

// scanJSONL calls fn once per non-blank line of r, in order, with a
// 1-based line number. A line that is not valid JSON at all is reported
// to fn as a json.SyntaxError-wrapped RowError via fn's own return value
// mechanism — scanJSONL itself only ever returns a genuine I/O error
// (including "line too long"), never a schema problem.
func scanJSONL(r io.Reader, fn func(line int, raw []byte) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxJSONLLineBytes)
	line := 0
	for scanner.Scan() {
		line++
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 {
			continue
		}
		if err := fn(line, append([]byte(nil), raw...)); err != nil {
			return err
		}
	}
	return scanner.Err()
}

// snapshotRow is the wire shape of one label-snapshot corpus row (task
// brief / design §4.6): `{id, input:{features,text,context}, label,
// split, source, meta}`. input.text is a map keyed by field name (e.g.
// "subject_line_skeleton", "first_link_host") rather than a flat list —
// the design's own abbreviated shape doesn't spell out text's exact
// type, and keying it by name is what lets ANY rule's `text:` selection
// pull the right subset out of one corpus row, the same convention
// event-replay Points use (see replay.go).
type snapshotRow struct {
	ID    string `json:"id"`
	Input struct {
		Features map[string]float64  `json:"features"`
		Text     map[string][]string `json:"text"`
		Context  string              `json:"context"`
	} `json:"input"`
	Label  string         `json:"label"`
	Split  string         `json:"split"`
	Source string         `json:"source"`
	Meta   map[string]any `json:"meta"`
}

// validSplits enumerates the only accepted `split` values (empty is
// allowed too — design's `--split all` reads every row regardless of
// this field).
var validSplits = map[string]bool{"": true, "train": true, "test": true}

// LoadSnapshotCorpus parses a label-snapshot corpus (design §4.6) from
// r: one JSON object per line, `{id, input:{features,text,context},
// label, split, source, meta}`. Every row is validated against the
// schema in eval/schema/corpus-v1.schema.json (mirrored here in Go, not
// re-read from that file at runtime — see the schema file's own header
// comment); a row that fails is reported as a RowError with its 1-based
// line number, collected into rowErrs, and excluded from the returned
// Dataset. A malformed JSON line, an empty id, an empty label, or a
// features+text both empty are all row-level failures; scanJSONL itself
// only returns a genuine I/O error (rowErrs is nil in that case, since
// nothing was validated at all).
//
// Every row lands under SliceFull — a label-snapshot corpus captures
// exactly one decision point per row, already resolved by whoever wrote
// it (design's own worked example: a label API write snapshotting
// features "as extracted at that time"). Dataset.Replay is always false,
// so Run never attempts the lead-time metric against it.
func LoadSnapshotCorpus(r io.Reader) (Dataset, []RowError, error) {
	var subjects []Subject
	var rowErrs []RowError
	seenIDs := map[string]int{}

	err := scanJSONL(r, func(line int, raw []byte) error {
		var row snapshotRow
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&row); err != nil {
			rowErrs = append(rowErrs, RowError{Line: line, Err: fmt.Errorf("invalid JSON or unknown field: %w", err)})
			return nil
		}
		if row.ID == "" {
			rowErrs = append(rowErrs, RowError{Line: line, Err: fmt.Errorf("id is required")})
			return nil
		}
		if prev, dup := seenIDs[row.ID]; dup {
			rowErrs = append(rowErrs, RowError{Line: line, Err: fmt.Errorf("duplicate id %q (first seen at line %d)", row.ID, prev)})
			return nil
		}
		seenIDs[row.ID] = line
		if row.Label == "" {
			rowErrs = append(rowErrs, RowError{Line: line, Err: fmt.Errorf("label is required")})
			return nil
		}
		if !validSplits[row.Split] {
			rowErrs = append(rowErrs, RowError{Line: line, Err: fmt.Errorf("split %q must be \"train\", \"test\", or omitted", row.Split)})
			return nil
		}
		if len(row.Input.Features) == 0 && len(row.Input.Text) == 0 {
			rowErrs = append(rowErrs, RowError{Line: line, Err: fmt.Errorf("input.features and input.text are both empty")})
			return nil
		}

		meta := row.Meta
		if meta == nil {
			meta = map[string]any{}
		}
		meta["split"] = row.Split

		subjects = append(subjects, Subject{
			ID:     row.ID,
			Label:  row.Label,
			Source: row.Source,
			Meta:   meta,
			Points: map[Slice]Point{SliceFull: {Features: row.Input.Features, Text: row.Input.Text, Context: row.Input.Context}},
		})
		return nil
	})
	if err != nil {
		return Dataset{}, nil, fmt.Errorf("eval: read snapshot corpus: %w", err)
	}
	if len(rowErrs) > 0 {
		return Dataset{}, rowErrs, &SchemaError{Rows: rowErrs}
	}
	return Dataset{Subjects: subjects, Replay: false}, nil, nil
}
