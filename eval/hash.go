package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"sort"

	"github.com/tokencanopy/abusekit/internal/config"
)

// SHA256File returns the hex-encoded SHA-256 digest of the file at path.
// The CLI uses this to fill Options.DatasetSHA/LabelsSHA from the exact
// bytes it read, so run.json's manifest reflects the real input file, not
// a re-serialization of what eval.LoadX parsed out of it.
func SHA256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// datasetSHA returns override if non-empty (the CLI's real file hash),
// else a content hash of dataset itself — used when a caller builds a
// Dataset directly in Go (a test, or a library consumer with no file on
// disk at all).
func datasetSHA(override string, dataset Dataset) string {
	if override != "" {
		return override
	}
	return datasetContentSHA(dataset)
}

// datasetContentSHA hashes a canonical JSON encoding of dataset: subjects
// sorted by ID (Run already does this before scoring, but this function
// is also called before that sort in some call sites, so it sorts its
// own copy independently), each Subject's Points encoded via Go's
// default map-key-sorted json.Marshal. Two Datasets with the same
// subjects/points/labels hash identically regardless of load order.
func datasetContentSHA(dataset Dataset) string {
	subjects := make([]Subject, len(dataset.Subjects))
	copy(subjects, dataset.Subjects)
	sort.Slice(subjects, func(i, j int) bool { return subjects[i].ID < subjects[j].ID })
	b, err := json.Marshal(struct {
		Subjects []Subject
		Replay   bool
	}{subjects, dataset.Replay})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ruleSHA hashes a canonical JSON encoding of rule's scoring-relevant
// fields — everything that changes what Run would compute for it. Stage
// keys are sorted implicitly by json.Marshal's map-key ordering.
func ruleSHA(rule config.Rule) string {
	b, err := json.Marshal(struct {
		Name        string
		Mode        config.Mode
		Scorer      string
		Inputs      []string
		Text        []string
		Labels      []string
		BenignLabel string
		Threshold   float64
		Stage       map[string]float64
	}{rule.Name, rule.Mode, rule.Scorer, rule.Inputs, rule.Text, rule.Labels, rule.BenignLabel, rule.Threshold, rule.Stage})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
