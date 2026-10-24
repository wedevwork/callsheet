package reale2e

import (
	"testing"
)

// probeLightRig is a scoped qualification probe on the offline rig: three
// wake tasks, the full reviewer probe, the triage probe on the light seed
// and the light reviewer probe on the harness-authored candidate.
func probeLightRig(t testing.TB, scope string) *devRig {
	r := newDevRig(t, []devAct{{}, {}, {}, {marker: markerReviewApproved}, {marker: markerTriageConf, designRequired: designRequiredNo},
		{marker: markerReviewApproved}})
	r.command = "probe-dev-full"
	r.extraArgs = []string{"--scenario", scope}
	r.owner = []string{"continue 1", "continue 2"}
	return r
}
