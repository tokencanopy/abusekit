package worker

import (
	"fmt"

	"github.com/tokencanopy/abusekit/internal/feature"
)

// renderReason is a v0 placeholder for design §4.6's "reason ... generated
// from a deterministic template over feature values": a real
// per-rule-configurable template/Explainer system is out of S2's scope
// (this slice is feature/worker/replay; a template engine belongs with S4's
// harness or a later slice — see the S2 PR description's open
// interpretations). This produces a compact, deterministic summary of the
// feature values that most directly drive the local scorer's weights
// (config/local_weights.yaml), sufficient for a human skimming a verdict
// row and for the replay fixtures' own assertions. It never touches raw
// event text, matching design's constraint that a reason is built from
// feature values only.
func renderReason(f feature.Features) string {
	return fmt.Sprintf(
		"core.resource_velocity_1h=%.0f core.credential_velocity_1h=%.0f core.declines_before_first_success=%.0f "+
			"core.first_funding_prepaid=%.0f brand.name_match=%.0f brand.name_has_at=%.0f "+
			"email.self_send_before_external=%.0f core.linked_deleted_n=%.0f core.linked_labelled_abusive_n=%.0f "+
			"core.fingerprint_seen_on_other_subjects=%.0f core.burst_ratio_24h_vs_lifetime=%.2f",
		f.ResourceVelocity1h, f.KeyVelocity1h, f.DeclinesBeforeFirstSuccess,
		f.FirstFundingPrepaid, f.NameBrandMatch, f.NameHasAt,
		f.SelfSendBeforeExternal, f.LinkedDeletedN, f.LinkedLabelledAbusiveN,
		f.FingerprintSeenOnOtherSubjects, f.BurstRatio24hVsLifetime,
	)
}
