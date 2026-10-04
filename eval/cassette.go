package eval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/tokencanopy/abusekit/internal/feature/registry"
	"os"
	"sort"
	"sync"

	"github.com/tokencanopy/abusekit/internal/model"
)

// CassetteKey identifies one recorded scorer call (design §4.10:
// "recorded cassettes keyed by (model, checkpoint, render, input_hash)").
// S4 keys on (scorer, model, prompt_version, input_hash) per the task
// brief — Model and PromptVersion play the design's "checkpoint"/"render"
// role here, since a ScoreRequest alone doesn't carry a checkpoint (that's
// the RESULT's field); a cassette is keyed on what's being ASKED, which
// is Scorer+Model (usually identical — Model is the scorer's OWN Name(),
// not a caller-supplied override) + PromptVersion + the request's content
// hash. Fix round S4 adds ScorerVersion (model.Scorer.Version(),
// queryable BEFORE a Score call, unlike the result's own Checkpoint): a
// vendor rotating its model, or a weights/prompt change, must never
// silently replay a stale cached answer under an otherwise-identical
// key — the same reasoning internal/core.Plan's own input hash already
// applies to a WORKER's skip-if-unchanged check.
type CassetteKey struct {
	Scorer        string `json:"scorer"`
	ScorerVersion string `json:"scorer_version"`
	Model         string `json:"model"`
	PromptVersion string `json:"prompt_version"`
	InputHash     string `json:"input_hash"`
}

func (k CassetteKey) storageKey() string {
	return k.Scorer + "|" + k.ScorerVersion + "|" + k.Model + "|" + k.PromptVersion + "|" + k.InputHash
}

// cassetteFile is the on-disk JSON shape of a saved Cassette: a flat list
// (not a map) so the file's key order is stable across saves regardless
// of Go's map iteration — WriteCassette always sorts by storageKey
// before marshaling. DatasetSHA (fix round S8) records which corpus this
// cassette's entries were recorded against — see Cassette.VerifyDatasetSHA.
type cassetteFile struct {
	FeatureKeySpace string              `json:"feature_key_space"`
	DatasetSHA      string              `json:"dataset_sha,omitempty"`
	Entries         []cassetteFileEntry `json:"entries"`
}

type cassetteFileEntry struct {
	Key    CassetteKey       `json:"key"`
	Result model.ScoreResult `json:"result"`
}

// Cassette is a record/replay store for scorer calls (design §4.10),
// keyed by CassetteKey. Safe for concurrent use.
type Cassette struct {
	mu         sync.Mutex
	path       string
	entries    map[string]cassetteFileEntry
	datasetSHA string
	dirty      bool
}

// LoadCassette reads path's cassette file, or returns an empty, writable
// Cassette if path doesn't exist yet (a fresh --record run creates the
// file on its first Save).
func LoadCassette(path string) (*Cassette, error) {
	c := &Cassette{path: path, entries: map[string]cassetteFileEntry{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, fmt.Errorf("eval: read cassette %s: %w", path, err)
	}
	var f cassetteFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("eval: parse cassette %s: %w", path, err)
	}
	if f.FeatureKeySpace != registry.KeySpace {
		return nil, fmt.Errorf("eval: cassette feature_key_space %q does not match %s", f.FeatureKeySpace, registry.KeySpace)
	}
	c.datasetSHA = f.DatasetSHA
	for _, e := range f.Entries {
		c.entries[e.Key.storageKey()] = e
	}
	return c, nil
}

// SetDatasetSHA records which corpus this cassette is being used against
// (fix round S8) — a no-op (and doesn't mark the cassette dirty) if sha
// already matches what's on record, so loading and using a cassette
// read-only never rewrites its file just to restate the same value.
func (c *Cassette) SetDatasetSHA(sha string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.datasetSHA != sha {
		c.datasetSHA = sha
		c.dirty = true
	}
}

// VerifyDatasetSHA reports an error if this cassette was previously
// recorded against a DIFFERENT corpus than expected (fix round S8's CI
// check: "committed cassettes reference only the committed synthetic
// corpus sha"). A cassette with no recorded dataset_sha (never run
// through SetDatasetSHA — every cassette from before this fix round)
// passes silently: there's nothing to contradict.
func (c *Cassette) VerifyDatasetSHA(expected string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.datasetSHA != "" && c.datasetSHA != expected {
		return fmt.Errorf("eval: cassette %s was recorded against dataset_sha %s, but the corpus being scored now hashes to %s (this cassette is stale or belongs to a different corpus)",
			c.path, c.datasetSHA, expected)
	}
	return nil
}

// Get returns the recorded result for key, if any.
func (c *Cassette) Get(key CassetteKey) (model.ScoreResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key.storageKey()]
	return e.Result, ok
}

// Put records result under key (overwriting any prior entry for the same
// key — a re-record intentionally replaces, it doesn't grow the file
// unboundedly across repeated runs against the same inputs).
func (c *Cassette) Put(key CassetteKey, result model.ScoreResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key.storageKey()] = cassetteFileEntry{Key: key, Result: result}
	c.dirty = true
}

// Save writes c to its path if anything changed since LoadCassette (or
// the last Save). A no-op on an unmodified Cassette, so a --scorer local
// run that never touches a cassette at all never rewrites one it merely
// loaded for informational purposes.
func (c *Cassette) Save() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.dirty {
		return nil
	}
	entries := make([]cassetteFileEntry, 0, len(c.entries))
	for _, e := range c.entries {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key.storageKey() < entries[j].Key.storageKey() })
	b, err := json.MarshalIndent(cassetteFile{FeatureKeySpace: registry.KeySpace, DatasetSHA: c.datasetSHA, Entries: entries}, "", "  ")
	if err != nil {
		return fmt.Errorf("eval: marshal cassette: %w", err)
	}
	if err := os.WriteFile(c.path, b, 0o644); err != nil {
		return fmt.Errorf("eval: write cassette %s: %w", c.path, err)
	}
	c.dirty = false
	return nil
}

// CassetteMode selects CassetteScorer's behavior on a miss.
type CassetteMode int

const (
	// CassetteReplay never calls the wrapped scorer: a miss is a loud
	// error (task brief: "a cassette miss must be a loud failure in CI
	// mode"). This is the mode `make gate`/CI always uses.
	CassetteReplay CassetteMode = iota
	// CassetteRecord calls the wrapped scorer on a miss and persists the
	// result — used by a `--record` run against a live adapter (S5+),
	// never by CI.
	CassetteRecord
)

// ErrCassetteMiss is returned (wrapped with the key's details) when a
// CassetteReplay-mode CassetteScorer has no recorded entry for a request.
var ErrCassetteMiss = errors.New("eval: cassette miss")

// CassetteScorer wraps a model.Scorer with record/replay: CI (and
// `make gate`) always drives a non-local scorer through one in
// CassetteReplay mode, so a config/corpus change that would have called
// a vendor instead fails loudly and immediately, never spends money or
// reaches the network. The local scorer never needs one (task brief:
// "Local needs no cassette" — it has no network to record in the first
// place).
type CassetteScorer struct {
	Inner         model.Scorer
	Cassette      *Cassette
	Mode          CassetteMode
	PromptVersion string
}

func (c *CassetteScorer) Name() string                     { return c.Inner.Name() }
func (c *CassetteScorer) Capabilities() model.Capabilities { return c.Inner.Capabilities() }
func (c *CassetteScorer) Policy() model.DataPolicy         { return c.Inner.Policy() }
func (c *CassetteScorer) Version() string                  { return c.Inner.Version() }

// Score implements model.Scorer, consulting c.Cassette before ever
// touching c.Inner.
func (c *CassetteScorer) Score(ctx context.Context, req model.ScoreRequest) (model.ScoreResult, error) {
	key := CassetteKey{
		Scorer:        c.Inner.Name(),
		ScorerVersion: c.Inner.Version(),
		Model:         c.Inner.Name(),
		PromptVersion: c.PromptVersion,
		InputHash:     hashScoreRequest(req),
	}
	if res, ok := c.Cassette.Get(key); ok {
		return res, nil
	}
	if c.Mode == CassetteReplay {
		return model.ScoreResult{}, fmt.Errorf("%w: scorer=%s prompt_version=%s input_hash=%s (no recorded cassette entry; CI never calls a vendor — re-record locally with --record)",
			ErrCassetteMiss, key.Scorer, key.PromptVersion, key.InputHash)
	}
	res, err := c.Inner.Score(ctx, req)
	if err != nil {
		return res, err
	}
	c.Cassette.Put(key, res)
	return res, nil
}

// hashScoreRequest is the cassette key's input_hash: a stable digest of
// everything that determines a scorer's answer to req (labels, features,
// text, context, render version). json.Marshal serializes map keys in
// sorted order, so this is stable regardless of Go's randomized map
// iteration.
func hashScoreRequest(req model.ScoreRequest) string {
	b, err := json.Marshal(struct {
		Labels        []string
		Features      map[string]float64
		Text          []string
		Context       string
		RenderVersion string
	}{req.Labels, req.Features, req.Text, req.Context, req.RenderVersion})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
