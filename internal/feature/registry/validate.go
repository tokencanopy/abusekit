package registry

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9]*\.[a-z][a-z0-9_]{0,55}$`)

// ValidateKeys rejects legacy inputs; it never translates or accepts aliases.
func ValidateKeys(values map[string]float64) error {
	names := make([]string, 0, len(values))
	for k := range values {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		if !strings.Contains(k, ".") {
			suffix := k
			switch k {
			case "key_total":
				suffix = "credential_total"
			case "key_velocity_1h":
				suffix = "credential_velocity_1h"
			case "name_brand_match":
				suffix = "name_match"
			}
			for _, d := range definitions {
				if strings.HasSuffix(d.Name, "."+suffix) {
					return fmt.Errorf("feature_renamed: %s is now %s", k, d.Name)
				}
			}
		}
		if !namePattern.MatchString(k) || strings.HasSuffix(k, "__absent") {
			return fmt.Errorf("invalid_feature_name: %q", k)
		}
	}
	return nil
}
