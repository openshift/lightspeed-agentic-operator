//go:build mc_product_e2e

package mcproducte2e

import "testing"

// TestCrossClusterRun needs a prepared hub and registered spoke (including self-spoke) and a real provider
func TestCrossClusterRun(t *testing.T) {
	c := checkPrerequisites(t)
	observer := newObserver(t, c)

	f := createFixture(t, c, observer)
	// keep one provider-backed run, named subtests report its independent checks
	if !t.Run("completed_run_and_spoke_proof", f.waitForCompletion) {
		t.FailNow()
	}
	for _, tc := range []struct{ name, step string }{
		{"analysis_spoke_identity_and_token", "anl"},
		{"execution_scoped_RBAC_and_token", "exe"},
		{"verification_spoke_identity_and_token", "ver"},
	} {
		if !t.Run(tc.name, func(t *testing.T) { observer.require(t, tc.step) }) {
			t.FailNow()
		}
	}
	if !t.Run("released_step_artifacts", observer.waitForCleanup) {
		t.FailNow()
	}
	// fixture cleanup deletes only this run (waiting for its finalizer), then its namespace
}
