package model

import (
	"fmt"
	"sort"
	"sync"
)

// Registry is a name-keyed set of Scorers (design §4.1: "model | Scorer,
// Explainer, Capabilities, registry, calibration"). internal/config looks
// scorers up here when validating and building rules; cmd/abusekit builds
// the production Registry by registering `local` (S1) and, from S5
// onward, the vendor adapters.
//
// Safe for concurrent use.
type Registry struct {
	mu      sync.RWMutex
	scorers map[string]Scorer
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{scorers: make(map[string]Scorer)}
}

// Register adds s under s.Name(). It returns an error, rather than
// panicking, when that name is already registered — library code must
// never panic (AGENTS.md), and a duplicate registration is a startup
// configuration mistake a caller should be able to report cleanly.
func (r *Registry) Register(s Scorer) error {
	if s == nil {
		return fmt.Errorf("model: cannot register a nil Scorer")
	}
	name := s.Name()
	if name == "" {
		return fmt.Errorf("model: cannot register a Scorer with an empty Name()")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.scorers[name]; exists {
		return fmt.Errorf("model: scorer %q already registered", name)
	}
	r.scorers[name] = s
	return nil
}

// Get returns the scorer registered under name, or (nil, false).
func (r *Registry) Get(name string) (Scorer, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.scorers[name]
	return s, ok
}

// Names returns every registered scorer name, sorted for deterministic
// output (error messages, config validation diagnostics).
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.scorers))
	for n := range r.scorers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
