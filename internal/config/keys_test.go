package config_test

import (
	"strings"
	"testing"

	"github.com/tokencanopy/abusekit/internal/config"
)

func TestLoadKeys_Valid(t *testing.T) {
	data := []byte(`
keys:
  - id: prod_e2a_server
    secret: dev-secret-events-not-real
    tenant: e2a
    producer: e2a-server
    scopes: [events]
  - id: prod_e2a_operator
    secret: dev-secret-labels-not-real
    tenant: e2a
    scopes: [labels, read, erase]
  - id: prod_e2a_backfill
    secret: dev-secret-backfill-not-real
    tenant: e2a
    producer: e2a-backfill
    scopes: [events, backfill]
`)
	keys, err := config.LoadKeys(data)
	if err != nil {
		t.Fatalf("LoadKeys: %v", err)
	}
	if len(keys) != 3 {
		t.Fatalf("expected 3 keys, got %d", len(keys))
	}
	k, ok := keys["prod_e2a_server"]
	if !ok {
		t.Fatalf("missing prod_e2a_server")
	}
	if k.Tenant != "e2a" || k.Producer != "e2a-server" {
		t.Fatalf("unexpected key contents: %+v", k)
	}
	if !k.HasScope(config.ScopeEvents) || k.HasScope(config.ScopeLabels) {
		t.Fatalf("unexpected scopes: %+v", k.Scopes)
	}

	backfill := keys["prod_e2a_backfill"]
	if !backfill.HasScope(config.ScopeBackfill) || !backfill.HasScope(config.ScopeEvents) {
		t.Fatalf("expected backfill key to have events+backfill scopes, got %+v", backfill.Scopes)
	}
}

func TestLoadKeys_RejectsDuplicateID(t *testing.T) {
	data := []byte(`
keys:
  - id: dup
    secret: s1
    tenant: e2a
    scopes: [read]
  - id: dup
    secret: s2
    tenant: e2a
    scopes: [read]
`)
	_, err := config.LoadKeys(data)
	if err == nil || !strings.Contains(err.Error(), "duplicate id") {
		t.Fatalf("expected duplicate id error, got %v", err)
	}
}

func TestLoadKeys_RejectsEventsScopeWithoutProducer(t *testing.T) {
	data := []byte(`
keys:
  - id: no_producer
    secret: s1
    tenant: e2a
    scopes: [events]
`)
	_, err := config.LoadKeys(data)
	if err == nil || !strings.Contains(err.Error(), "no producer") {
		t.Fatalf("expected missing-producer error, got %v", err)
	}
}

func TestLoadKeys_RejectsUnknownScope(t *testing.T) {
	data := []byte(`
keys:
  - id: bad_scope
    secret: s1
    tenant: e2a
    scopes: [flying]
`)
	_, err := config.LoadKeys(data)
	if err == nil || !strings.Contains(err.Error(), "unknown scope") {
		t.Fatalf("expected unknown-scope error, got %v", err)
	}
}

func TestLoadKeys_RejectsMissingFields(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{"missing id", "keys:\n  - secret: s\n    tenant: e2a\n    scopes: [read]\n", "missing id"},
		{"missing secret", "keys:\n  - id: k\n    tenant: e2a\n    scopes: [read]\n", "missing secret"},
		{"missing tenant", "keys:\n  - id: k\n    secret: s\n    scopes: [read]\n", "missing tenant"},
		{"missing scopes", "keys:\n  - id: k\n    secret: s\n    tenant: e2a\n", "must declare at least one scope"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.LoadKeys([]byte(tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestLoadKeys_RejectsUnknownField(t *testing.T) {
	data := []byte(`
keys:
  - id: k
    secret: s
    tenant: e2a
    scopes: [read]
    typo_field: oops
`)
	_, err := config.LoadKeys(data)
	if err == nil {
		t.Fatalf("expected a strict-decode error for an unknown field")
	}
}
