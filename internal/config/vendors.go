package config

import (
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/tokencanopy/abusekit/internal/model"
)

// rawVendors mirrors config/vendors.yaml's shape.
type rawVendors struct {
	Vendors []rawVendor `yaml:"vendors"`
}

type rawVendor struct {
	Name         string `yaml:"name"`
	TermsVersion string `yaml:"terms_version"`
	DPARef       string `yaml:"dpa_ref"`
	Policy       struct {
		AllowsText     bool `yaml:"allows_text"`
		RetainsInputs  bool `yaml:"retains_inputs"`
		TrainsOnInputs bool `yaml:"trains_on_inputs"`
	} `yaml:"policy"`
}

// LoadVendors parses config/vendors.yaml's adapter allowlist (design
// §4.6: "config/vendors.yaml lists each adapter with terms_version,
// dpa_ref and the DataPolicy; the loader refuses an adapter absent from
// the list"). The returned map is keyed by vendor name for direct use as
// Dependencies.Vendors.
//
// Duplicate vendor names are a load error: two conflicting policy records
// for the same adapter is a data-governance bug, not a config choice.
func LoadVendors(data []byte) (map[string]VendorEntry, error) {
	var raw rawVendors
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("config: parse vendors yaml: %w", err)
	}
	out := make(map[string]VendorEntry, len(raw.Vendors))
	for _, v := range raw.Vendors {
		if v.Name == "" {
			return nil, fmt.Errorf("config: a vendors.yaml entry is missing `name`")
		}
		if _, exists := out[v.Name]; exists {
			return nil, fmt.Errorf("config: duplicate vendors.yaml entry for %q", v.Name)
		}
		// S9: an entry with no terms_version or dpa_ref is a data-
		// governance gap, not a valid placeholder — an adapter with
		// genuinely no vendor terms (e.g. local) still records the
		// literal string "n/a", so an empty string always means "nobody
		// filled this in".
		if v.TermsVersion == "" {
			return nil, fmt.Errorf("config: vendors.yaml entry %q is missing terms_version", v.Name)
		}
		if v.DPARef == "" {
			return nil, fmt.Errorf("config: vendors.yaml entry %q is missing dpa_ref", v.Name)
		}
		out[v.Name] = VendorEntry{
			Name:         v.Name,
			TermsVersion: v.TermsVersion,
			DPARef:       v.DPARef,
			Policy: model.DataPolicy{
				TermsVersion:   v.TermsVersion,
				AllowsText:     v.Policy.AllowsText,
				RetainsInputs:  v.Policy.RetainsInputs,
				TrainsOnInputs: v.Policy.TrainsOnInputs,
			},
		}
	}
	return out, nil
}
