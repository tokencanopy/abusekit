package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"golang.org/x/sys/cpu"

	"github.com/tokencanopy/abusekit/internal/config"
	"github.com/tokencanopy/abusekit/internal/core"
	"github.com/tokencanopy/abusekit/internal/event"
	"github.com/tokencanopy/abusekit/internal/feature"
	"github.com/tokencanopy/abusekit/internal/feature/registry"
)

// GoldenRow pins the unbounded evaluator's exact output at an event or timer.
// Floats are hex IEEE-754 bits so comparison never introduces decimal tolerance.
type GoldenRow struct {
	NumericProfile string            `json:"numeric_profile"`
	Fixture        string            `json:"fixture"`
	Subject        string            `json:"subject"`
	At             string            `json:"at"`
	Trigger        string            `json:"trigger"`
	Producer       string            `json:"producer,omitempty"`
	EventID        string            `json:"event_id,omitempty"`
	Features       map[string]string `json:"features"`
	NextRescoreAt  string            `json:"next_rescore_at"`
	Rules          []GoldenRule      `json:"rules"`
	Score          string            `json:"score"`
	Tier           string            `json:"tier"`
	Degraded       bool              `json:"degraded"`
}

type GoldenRule struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	InputHash string `json:"input_hash"`
	Risk      string `json:"risk"`
	Status    string `json:"status"`
	Flagged   bool   `json:"flagged"`
}

// GoldenProfile identifies the math implementation independently of replay output.
// AMD64 baselines use GOAMD64=v1. Like Go's math.Exp dispatch, FMA needs AVX too.
func GoldenProfile() string {
	if runtime.GOARCH == "amd64" {
		if cpu.X86.HasAVX && cpu.X86.HasFMA {
			return "amd64-fma"
		}
		return "amd64-no-fma"
	}
	return runtime.GOARCH
}

func floatBits(v float64) string { return fmt.Sprintf("%016x", math.Float64bits(v)) }
func goldenTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// WriteGolden replays one event file in canonical order. Every event is followed
// by a fresh score of its subject; timers run before events at the same instant.
// After the final event, all pending timers are drained. Labels come only from
// historical label events, never from the evaluation answer key. No vendor calls
// or database access are permitted. Histories and neighbor evidence contain only
// events already accepted by this replay, including within a timestamp tie.
func WriteGolden(ctx context.Context, out io.Writer, name string, input io.Reader, cfg *config.Config, brands feature.BrandSet, webmail feature.WebmailSet) error {
	if cfg == nil || len(cfg.Rules) == 0 {
		return fmt.Errorf("golden: rules are required")
	}
	for _, rule := range cfg.Rules {
		scorer, ok := cfg.ScorerFor(rule)
		if !ok || scorer.Name() != "local" {
			return fmt.Errorf("golden: rule %s must use local scorer", rule.Name)
		}
		if rule.BenignLabel != cfg.Rules[0].BenignLabel {
			return fmt.Errorf("golden: rules must share benign label")
		}
	}
	bySubject, labels, _, rowErrs, err := parseEventRows(name, input)
	if err != nil {
		return err
	}
	if len(rowErrs) > 0 {
		return fmt.Errorf("golden: %w", &SchemaError{Rows: rowErrs})
	}
	var events []event.Event
	for _, history := range bySubject {
		events = append(events, history...)
	}
	for subject, history := range labels {
		for _, l := range history {
			events = append(events, event.Event{ID: l.id, Producer: l.producer, Subject: subject, Type: labelEventType, At: l.at, Data: map[string]any{"label": l.value}})
		}
	}
	sort.Slice(events, func(i, j int) bool { return event.Less(events[i], events[j]) })
	if len(events) == 0 {
		return fmt.Errorf("golden: empty event file")
	}
	seen := map[string]bool{}
	for _, e := range events {
		key := e.Producer + "\x00" + e.ID
		if seen[key] {
			return fmt.Errorf("golden: duplicate producer/id %q/%q", e.Producer, e.ID)
		}
		seen[key] = true
	}
	accepted := map[string][]event.Event{}
	acceptedLabels := map[string][]labelledAt{}
	timers := map[string]time.Time{}
	enc := json.NewEncoder(out)
	score := func(subject string, now time.Time, trigger string, e event.Event) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		neighbors := asOfNeighbors{n: newDatasetNeighbors(accepted, acceptedLabels), asOf: now.Add(time.Nanosecond), benignLabel: cfg.Rules[0].BenignLabel}
		result, err := feature.Extract(ctx, evalTenant, subject, accepted[subject], neighbors, feature.DefaultWindows(now), brands, webmail)
		if err != nil {
			return err
		}
		timers[subject] = result.NextRescoreAt
		if !result.NextRescoreAt.IsZero() && !result.NextRescoreAt.After(now) {
			return fmt.Errorf("golden: non-advancing timer for %s", subject)
		}
		values := result.Features.Map()
		row := GoldenRow{NumericProfile: GoldenProfile(), Fixture: name, Subject: subject, At: goldenTime(now), Trigger: trigger, Producer: e.Producer, EventID: e.ID, Features: map[string]string{}, NextRescoreAt: goldenTime(result.NextRescoreAt)}
		for k, v := range values {
			row.Features[k] = floatBits(v)
		}
		states := make([]core.RuleState, 0, len(cfg.Rules))
		for _, rule := range cfg.Rules {
			scorer, _ := cfg.ScorerFor(rule)
			states = append(states, core.RuleState{Rule: rule, ScorerVersion: scorer.Version(), CalibrationID: "none"})
		}
		calls := core.Plan(core.Vector{Values: values, HashQuantum: registry.HashQuanta()}, extractTextFields(accepted[subject]), states)
		outcomes := make([]core.RuleOutcome, 0, len(calls))
		for i, call := range calls {
			scorer, _ := cfg.ScorerFor(call.Rule)
			gr := GoldenRule{Name: call.Rule.Name, Version: states[i].ScorerVersion, InputHash: call.InputHash}
			outcome := core.RuleOutcome{Rule: call.Rule}
			if call.Skip {
				outcome.Unscored = true
				outcome.ErrorCode = string(call.SkipReason)
			} else {
				res, err := scorer.Score(ctx, call.Request)
				if err != nil {
					return err
				}
				outcome.Result = &res
			}
			outcomes = append(outcomes, outcome)
			row.Rules = append(row.Rules, gr)
		}
		verdict := core.Combine(outcomes, core.CombineParams{Tiers: cfg.Tiers, MinScoredAdvise: cfg.MinScoredAdvise, TextRulesNeedFeatureSupport: cfg.TextRulesNeedFeatureSupport}, nil)
		for i, signal := range verdict.Signals {
			row.Rules[i].Risk = floatBits(signal.Risk)
			row.Rules[i].Status = signal.Status
			row.Rules[i].Flagged = signal.Flagged
		}
		row.Score = floatBits(verdict.Score)
		row.Tier = verdict.Tier
		row.Degraded = verdict.Degraded
		return enc.Encode(row)
	}
	drain := func(until time.Time) error {
		for {
			var next time.Time
			subject := ""
			for s, t := range timers {
				if t.IsZero() {
					continue
				}
				if next.IsZero() || t.Before(next) || (t.Equal(next) && s < subject) {
					next = t
					subject = s
				}
			}
			if next.IsZero() || (!until.IsZero() && next.After(until)) {
				return nil
			}
			if err := score(subject, next, "timer", event.Event{}); err != nil {
				return err
			}
		}
	}
	for _, e := range events {
		if err := drain(e.At); err != nil {
			return err
		}
		if e.Type == labelEventType {
			acceptedLabels[e.Subject] = append(acceptedLabels[e.Subject], labelledAt{at: e.At, value: e.Data["label"].(string)})
		} else {
			accepted[e.Subject] = append(accepted[e.Subject], e)
		}
		if err := score(e.Subject, e.At, "event", e); err != nil {
			return err
		}
	}
	return drain(time.Time{})
}

// WriteGoldenFixtures covers every committed event fixture and the synthetic
// corpus as isolated replay worlds; answer-key and feature-snapshot files are
// deliberately excluded. Filenames, subjects, event ties and timer ties are stable.
func WriteGoldenFixtures(ctx context.Context, out io.Writer, dir string, cfg *config.Config, brands feature.BrandSet, webmail feature.WebmailSet) error {
	paths, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return err
	}
	paths = append(paths, filepath.Join(dir, "synthetic", "events.jsonl"))
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		name, err := filepath.Rel(dir, path)
		if err != nil {
			f.Close()
			return err
		}
		err = WriteGolden(ctx, out, filepath.ToSlash(name), f, cfg, brands, webmail)
		f.Close()
		if err != nil {
			return fmt.Errorf("golden %s: %w", name, err)
		}
	}
	return nil
}
