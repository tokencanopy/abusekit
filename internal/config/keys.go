package config

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Scope is one capability a key can be granted (design §4.3: "Key scopes:
// events, labels, read, backfill" plus §4.4's operator-only "erase").
type Scope string

const (
	ScopeEvents   Scope = "events"
	ScopeLabels   Scope = "labels"
	ScopeRead     Scope = "read"
	ScopeBackfill Scope = "backfill"
	ScopeErase    Scope = "erase"
)

// knownScopes enumerates every scope internal/serve understands, for
// load-time validation.
var knownScopes = map[Scope]bool{
	ScopeEvents:   true,
	ScopeLabels:   true,
	ScopeRead:     true,
	ScopeBackfill: true,
	ScopeErase:    true,
}

// Key is one entry from config/keys.yaml: a shared HMAC secret bound to a
// single tenant, an optional producer identity (required for a key with
// the `events` scope — design §4.2: "Credentials are per producer"), and
// the scopes it may exercise. internal/serve looks a key up by its id (the
// `X-Abusekit-Key` header value) and never by secret.
//
// Secrets belong in config here the same way the rest of abusekit's
// runtime config does (AGENTS.md's config-via-file convention, mirroring
// e2a's billing sidecar): production points --keys/ABUSEKIT_KEYS_CONFIG at
// a file populated from a real secret store (GCP Secret Manager, in the
// hosted e2a-ops deployment) rather than committing a real secret to this
// public repo. config/keys.yaml ships only synthetic, clearly-labelled
// development values.
type Key struct {
	ID       string
	Secret   string
	Tenant   string
	Producer string
	Scopes   map[Scope]bool
}

// HasScope reports whether k may exercise scope s.
func (k Key) HasScope(s Scope) bool { return k.Scopes[s] }

// rawKeys mirrors config/keys.yaml's shape.
type rawKeys struct {
	Keys []rawKey `yaml:"keys"`
}

type rawKey struct {
	ID       string   `yaml:"id"`
	Secret   string   `yaml:"secret"`
	Tenant   string   `yaml:"tenant"`
	Producer string   `yaml:"producer"`
	Scopes   []string `yaml:"scopes"`
}

// LoadKeys parses config/keys.yaml (design §4.3's per-producer/per-operator
// credentials): each entry becomes a Key keyed by its id for direct use as
// internal/serve's key lookup table. Every failure is collected so a typo
// across several entries is reported in one pass, matching Load and
// LoadVendors' own strict-validation convention.
//
// Duplicate key ids, an empty id/secret/tenant, an events-scoped key with
// no producer, and an unknown scope name are all load errors — a config
// mistake here is a production authentication bug waiting to happen, not
// something to silently default around.
func LoadKeys(data []byte) (map[string]Key, error) {
	var raw rawKeys
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("config: parse keys yaml: %w", err)
	}

	var issues []string
	out := make(map[string]Key, len(raw.Keys))
	for i, rk := range raw.Keys {
		if rk.ID == "" {
			issues = append(issues, fmt.Sprintf("keys[%d]: missing id", i))
			continue
		}
		if _, exists := out[rk.ID]; exists {
			issues = append(issues, fmt.Sprintf("keys[%d]: duplicate id %q", i, rk.ID))
			continue
		}
		if rk.Secret == "" {
			issues = append(issues, fmt.Sprintf("key %q: missing secret", rk.ID))
		}
		if rk.Tenant == "" {
			issues = append(issues, fmt.Sprintf("key %q: missing tenant", rk.ID))
		}
		if len(rk.Scopes) == 0 {
			issues = append(issues, fmt.Sprintf("key %q: must declare at least one scope", rk.ID))
		}
		scopes := make(map[Scope]bool, len(rk.Scopes))
		for _, s := range rk.Scopes {
			sc := Scope(s)
			if !knownScopes[sc] {
				names := make([]string, 0, len(knownScopes))
				for k := range knownScopes {
					names = append(names, string(k))
				}
				sort.Strings(names)
				issues = append(issues, fmt.Sprintf("key %q: unknown scope %q (known: %s)", rk.ID, s, strings.Join(names, ", ")))
				continue
			}
			scopes[sc] = true
		}
		if scopes[ScopeEvents] && rk.Producer == "" {
			issues = append(issues, fmt.Sprintf("key %q: has the \"events\" scope but no producer", rk.ID))
		}
		out[rk.ID] = Key{ID: rk.ID, Secret: rk.Secret, Tenant: rk.Tenant, Producer: rk.Producer, Scopes: scopes}
	}

	if len(issues) > 0 {
		return nil, fmt.Errorf("config: %s", strings.Join(issues, "; "))
	}
	return out, nil
}
