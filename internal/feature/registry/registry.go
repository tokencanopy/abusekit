// Package registry owns feature names, immutable arithmetic order, and hash buckets.
// It has no extraction or storage dependencies.
package registry

// KeySpace is the sole accepted serialized feature-key contract.
const KeySpace = "ns-v1"

// FeatureDef carries metadata required by the P1 scoring and hashing consumers.
// Pack extraction metadata is introduced with the later pack engine.
type FeatureDef struct {
	Name        string
	Order       int
	HashQuantum float64
}

var definitions = []FeatureDef{
	{"core.burst_ratio_24h_vs_lifetime", 0, 0},
	{"core.declines_before_first_success", 1, 0},
	{"email.distinct_recipients_1h", 2, 0},
	{"core.fingerprint_seen_on_other_subjects", 3, 0},
	{"email.first_day_distinct_domains", 4, 0},
	{"core.first_funding_prepaid", 5, 0},
	{"core.credential_total", 6, 0},
	{"core.credential_velocity_1h", 7, 0},
	{"core.linked_deleted_n", 8, 0},
	{"core.linked_labelled_abusive_n", 9, 0},
	{"brand.name_match", 10, 0},
	{"brand.name_has_at", 11, 0},
	{"core.neighbors_truncated", 12, 0},
	{"core.resource_total", 13, 0},
	{"core.resource_velocity_1h", 14, 0},
	{"email.self_send_before_external", 15, 0},
	{"email.sends_10m_max", 16, 0},
	{"email.sends_1h", 17, 0},
	{"email.sends_first_day", 18, 0},
	{"core.subject_age_h", 19, 1},
	{"email.subject_brand_match", 20, 0},
	{"core.upgrade_delay_min", 21, 60},
	{"core.upgraded", 22, 0},
	{"email.webmail_recipient_share", 23, 0},
	{"email.webmail_sends_1h", 24, 0},
}

// Definitions returns a copy so callers cannot change registered arithmetic order.
func Definitions() []FeatureDef { return append([]FeatureDef(nil), definitions...) }

// Names returns registered names in immutable arithmetic order.
func Names() []string {
	out := make([]string, len(definitions))
	for i, d := range definitions {
		out[i] = d.Name
	}
	return out
}

// HashQuanta returns an independent map of positive hash-bucket widths.
func HashQuanta() map[string]float64 {
	out := map[string]float64{}
	for _, d := range definitions {
		if d.HashQuantum > 0 {
			out[d.Name] = d.HashQuantum
		}
	}
	return out
}

// Less preserves legacy arithmetic order. Unregistered custom features follow by name.
func Less(a, b string) bool {
	ai, bi := 1000000, 1000000
	for _, d := range definitions {
		if d.Name == a {
			ai = d.Order
		}
		if d.Name == b {
			bi = d.Order
		}
	}
	if ai != bi {
		return ai < bi
	}
	return a < b
}
