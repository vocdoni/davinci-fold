package tests

import (
	"testing"
)

// TestChaosBatchWorkerDeath kills a batch worker mid-election and asserts the
// scheduler re-dispatches the persisted ProveRequest elsewhere and the election
// still finalizes to the correct tally.
//
// Requires >=2 workers (one to kill, one to absorb the re-dispatch).
func TestChaosBatchWorkerDeath(t *testing.T) {
	requireWorkers(t, 2)
	// TODO(integration): start an election, let one batch dispatch to worker A,
	// stop A's process, and assert the batch re-proves on B and the fold chain
	// completes. Needs the ballot-generation harness and process control over
	// the external Rust workers.
	t.Skip("chaos: batch-worker death pending ballot-generation + worker process control")
}

// TestChaosFoldWorkerDeath kills the pinned fold worker and asserts the
// orchestrator re-pins a new fold worker, re-imports every batch blob
// (re-proving any missing ones), and re-folds from genesis to the same tally.
func TestChaosFoldWorkerDeath(t *testing.T) {
	requireWorkers(t, 2)
	// TODO(integration): drive folds onto worker A, stop A, assert re-pin to B
	// with re-import/re-fold from genesis and a correct finalize.
	t.Skip("chaos: fold-worker death pending ballot-generation + worker process control")
}

// TestChaosOrchestratorRestart stops and rebuilds the engine mid-election and
// asserts it restores chain.State from the latest snapshot, reloads the job
// maps, clears stale reservations, and resumes to the correct tally.
func TestChaosOrchestratorRestart(t *testing.T) {
	requireWorkers(t, 1)
	// TODO(integration): submit votes, seal/persist batches, tear down and
	// rebuild the Engine over the same Pebble dir, and assert State.Root and the
	// next StateTransitionData match, then finalize to the correct tally.
	t.Skip("chaos: orchestrator restart pending ballot-generation harness")
}
