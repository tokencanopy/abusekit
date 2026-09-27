// Package fake provides an injectable model.Scorer for exercising the
// adapter contract suite (internal/model/contract_test.go) and, from S2
// onward, the worker's error-handling paths, without a real vendor
// dependency. It is not a production scorer and is never registered by
// cmd/abusekit.
package fake

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"sort"
	"time"

	"github.com/tokencanopy/abusekit/internal/model"
)

// Scorer is a fully injectable model.Scorer. Every exported field has a
// working zero-value default (deterministic hash-based scoring, no
// delay, no error), so a test only sets the fields it cares about.
type Scorer struct {
	// NameValue is returned by Name(). Defaults to "fake".
	NameValue string
	// Caps is returned by Capabilities().
	Caps model.Capabilities
	// DataPolicyValue is returned by Policy().
	DataPolicyValue model.DataPolicy
	// VersionValue is returned by Version(). Defaults to "fake-v1" so a
	// test that doesn't care about versioning still gets a non-empty,
	// stable value.
	VersionValue string

	// ScoreFunc, if set, replaces the default scoring logic entirely.
	ScoreFunc func(ctx context.Context, req model.ScoreRequest) (model.ScoreResult, error)
	// Delay, if positive, makes the default Score block for Delay before
	// answering, honouring ctx cancellation/deadline in the meantime —
	// use this to exercise a real "timeout -> error" path via a
	// short-deadline context, as opposed to an already-cancelled one.
	Delay time.Duration
	// Err, if set, makes the default Score return this error immediately
	// (after the Delay/ctx check above).
	Err error
}

// New returns a Scorer with reasonable contract-suite defaults: open
// label mode, accepts both text and features, uncapped tokens,
// uncalibrated (so callers can also exercise the "missing calibration"
// config-validation path without a second type).
func New() *Scorer {
	return &Scorer{
		NameValue: "fake",
		Caps: model.Capabilities{
			LabelMode:       model.OpenLabelMode(),
			AcceptsText:     true,
			AcceptsFeatures: true,
			MaxTokens:       0,
			Calibrated:      true,
		},
	}
}

func (s *Scorer) Name() string {
	if s.NameValue == "" {
		return "fake"
	}
	return s.NameValue
}

func (s *Scorer) Capabilities() model.Capabilities { return s.Caps }
func (s *Scorer) Policy() model.DataPolicy         { return s.DataPolicyValue }

func (s *Scorer) Version() string {
	if s.VersionValue == "" {
		return "fake-v1"
	}
	return s.VersionValue
}

// Score honours ctx first (covering the universal "cancelled context ->
// error" contract check), then Delay (for a genuine deadline-timeout
// test), then Err, then ScoreFunc, then falls back to a deterministic
// default: probabilities derived from a SHA-256 of the request's labels,
// features and text, so repeated calls with the same request produce the
// same result (the contract suite's determinism check) while different
// requests plausibly differ.
//
// The default respects Capabilities: it errors on an unaccepted label
// set, on text when !AcceptsText, on features when !AcceptsFeatures, and
// reports Truncated when Caps.MaxTokens > 0 and the joined text exceeds
// it (measured in bytes, a deliberately crude proxy for "tokens" that is
// enough to exercise the Truncated field without a real tokenizer).
func (s *Scorer) Score(ctx context.Context, req model.ScoreRequest) (model.ScoreResult, error) {
	if err := ctx.Err(); err != nil {
		return model.ScoreResult{}, err
	}
	if s.Delay > 0 {
		select {
		case <-time.After(s.Delay):
		case <-ctx.Done():
			return model.ScoreResult{}, ctx.Err()
		}
	}
	if s.Err != nil {
		return model.ScoreResult{}, s.Err
	}
	if s.ScoreFunc != nil {
		return s.ScoreFunc(ctx, req)
	}

	if !s.Caps.LabelMode.Accepts(req.Labels) {
		return model.ScoreResult{}, model.ErrUnknownLabel
	}
	if len(req.Text) > 0 && !s.Caps.AcceptsText {
		return model.ScoreResult{}, model.ErrTextNotAccepted
	}
	if len(req.Features) > 0 && !s.Caps.AcceptsFeatures {
		return model.ScoreResult{}, model.ErrFeaturesNotAccepted
	}

	truncated := false
	textLen := 0
	for _, t := range req.Text {
		textLen += len(t)
	}
	if s.Caps.MaxTokens > 0 && textLen > s.Caps.MaxTokens {
		truncated = true
	}

	probs := deterministicProbs(req)

	return model.ScoreResult{
		Probs:      probs,
		Model:      s.Name(),
		Checkpoint: "fake-1",
		Render:     req.RenderVersion,
		LatencyMS:  0,
		CostMicro:  0,
		Truncated:  truncated,
	}, nil
}

// deterministicProbs hashes the request into a reproducible weight per
// label, then normalizes so the result sums to exactly 1.
func deterministicProbs(req model.ScoreRequest) map[string]float64 {
	labels := append([]string(nil), req.Labels...)
	sort.Strings(labels)

	weights := make([]float64, len(labels))
	var total float64
	for i, label := range labels {
		h := sha256.New()
		h.Write([]byte(label))
		for _, t := range req.Text {
			h.Write([]byte{0})
			h.Write([]byte(t))
		}
		featureKeys := make([]string, 0, len(req.Features))
		for k := range req.Features {
			featureKeys = append(featureKeys, k)
		}
		sort.Strings(featureKeys)
		for _, k := range featureKeys {
			h.Write([]byte{1})
			h.Write([]byte(k))
			var buf [8]byte
			binary.LittleEndian.PutUint64(buf[:], uint64(req.Features[k]*1000))
			h.Write(buf[:])
		}
		sum := h.Sum(nil)
		// +1 keeps every label's weight strictly positive so a
		// single-label request never divides by zero.
		w := float64(binary.LittleEndian.Uint32(sum[:4])%10000) + 1
		weights[i] = w
		total += w
	}

	probs := make(map[string]float64, len(labels))
	for i, label := range labels {
		probs[label] = weights[i] / total
	}
	return probs
}
