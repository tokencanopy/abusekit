// webmail.go — config/webmail.yaml's loaded domain set: a public list of
// major consumer webmail providers, used only to compute
// email.webmail_recipient_share and email.webmail_sends_1h (§4.5). Deliberately a
// flat, public list of well-known provider domain names (gmail.com,
// outlook.com, ...) — see config/webmail.yaml's own header for why this
// stays public-repo-safe (AGENTS.md's data-boundary rule).
package feature

import (
	"bytes"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// WebmailSet is a loaded, ready-to-query set of webmail domains
// (config/webmail.yaml). The zero value matches nothing — a caller that
// hasn't loaded a webmail list simply gets email.webmail_recipient_share/
// email.webmail_sends_1h held at 0, never a panic or an error.
type WebmailSet struct {
	domains map[string]struct{}
}

// NewWebmailSet builds a WebmailSet from a list of domains, normalising
// each one the same way Contains normalises its argument (case/whitespace-
// insensitive, normalizeToken) so construction and lookup can never
// silently diverge.
func NewWebmailSet(domains []string) WebmailSet {
	m := make(map[string]struct{}, len(domains))
	for _, d := range domains {
		if n := normalizeToken(d); n != "" {
			m[n] = struct{}{}
		}
	}
	return WebmailSet{domains: m}
}

// Contains reports whether domain (case/whitespace-insensitively, the
// same convention resourceCount's kind matching and
// firstDayDistinctDomains' domain matching already use) is on the loaded
// webmail list.
func (w WebmailSet) Contains(domain string) bool {
	if len(w.domains) == 0 {
		return false
	}
	_, ok := w.domains[normalizeToken(domain)]
	return ok
}

// rawWebmailFile mirrors config/webmail.yaml's shape.
type rawWebmailFile struct {
	Domains []string `yaml:"domains"`
}

// LoadWebmailFile reads and parses a config/webmail.yaml-shaped file.
func LoadWebmailFile(path string) (WebmailSet, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return WebmailSet{}, fmt.Errorf("feature: read webmail file %s: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	var raw rawWebmailFile
	if err := dec.Decode(&raw); err != nil {
		return WebmailSet{}, fmt.Errorf("feature: parse webmail file %s: %w", path, err)
	}
	if len(raw.Domains) == 0 {
		return WebmailSet{}, fmt.Errorf("feature: %s: domains list is empty", path)
	}
	return NewWebmailSet(raw.Domains), nil
}
