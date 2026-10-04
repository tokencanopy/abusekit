package feature_test

import (
	"github.com/tokencanopy/abusekit/internal/feature"
	"testing"
)

func TestNamespacedFeatureOutput(t *testing.T) {
	values := (feature.Features{SubjectAgeH: 2, KeyTotal: 3, NameBrandMatch: 1, Sends1h: 4}).Map()
	for name, want := range map[string]float64{"core.subject_age_h": 2, "core.credential_total": 3, "brand.name_match": 1, "email.sends_1h": 4} {
		if got, ok := values[name]; !ok || got != want {
			t.Errorf("%s = %v (present=%v), want %v", name, got, ok, want)
		}
	}
}
