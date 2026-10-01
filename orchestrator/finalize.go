package orchestrator

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/vocdoni/davinci-fold/log"

	"github.com/vocdoni/davinci-fold/types"
	"github.com/vocdoni/davinci-fold/workers"
	davinci "github.com/vocdoni/davinci-zkvm/go-sdk"
	"github.com/vocdoni/davinci-zkvm/go-sdk/chain"
)

// Finalize failure reasons. Their messages are the public summary of a failed
// finalize (Election.FinalizeError, see finalizeSummary), so they never carry
// prover addresses, job IDs or proof values; the full error goes to the log
// and the audit trail.
var (
	errProverUnavailable   = errors.New("prover unavailable")
	errFinalizeInterrupted = errors.New("finalize interrupted")
	errNothingToFinalize   = errors.New("nothing to finalize")
	errFinalFoldFailed     = errors.New("fold failed")
	errDecryptionFailed    = errors.New("decryption failed")
	errFinalProofFailed    = errors.New("final proof failed")
	errVerificationFailed  = errors.New("final proof verification failed")
	errFinalizeInternal    = errors.New("internal error")
)

// finalizeSummary returns the reason a finalize failed with err, one of the
// errors above: no prover to run it and an interruption first, then the step
// that failed.
func finalizeSummary(err error) string {
	switch {
	case errors.Is(err, errNoWorker), errors.Is(err, errWorkerRemoved), errors.Is(err, errProverUnavailable):
		return errProverUnavailable.Error()
	case errors.Is(err, context.Canceled), errors.Is(err, errFinalizeInterrupted):
		return errFinalizeInterrupted.Error()
	}
	for _, reason := range []error{
		errNothingToFinalize, errFinalFoldFailed, errDecryptionFailed, errFinalProofFailed, errVerificationFailed,
	} {
		if errors.Is(err, reason) {
			return reason.Error()
		}
	}
	return errFinalizeInternal.Error()
}

// Finalize closes an election's chain: it folds whatever the drain left,
// decrypts the accumulators with privKey, runs the finalize + final PLONK on
// the fold worker (moving the chain to another worker if it fails, as the
// folds do), verifies the returned digest against the local state (the same
// external checks chain.Sequencer.Finalize performs), and persists the
// resulting Results. The privKey is the keywarden-provided decryption scalar;
// it is used only here and never stored. An error wraps the reason of the
// step that failed (see finalizeSummary).
func (sc *Scheduler) Finalize(id types.ElectionID, privKey *big.Int) (*types.Results, error) {
	rt, ok := sc.engine.runtime(id)
	if !ok {
		return nil, fmt.Errorf("unknown election %s", id.String())
	}
	m := sc.driveLock(id)
	m.Lock()
	defer m.Unlock()
	fc, err := sc.chain(id)
	if err != nil {
		return nil, err
	}
	if err := sc.advance(id, fc, true); err != nil {
		return nil, fmt.Errorf("%w: %w", errFinalFoldFailed, err)
	}
	if fc.lastFold == "" {
		return nil, fmt.Errorf("%w: no fold in the chain", errNothingToFinalize)
	}

	// The election has ended: its state no longer changes.
	state := rt.current()
	payload, results, err := state.ResultsPayload(privKey)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errDecryptionFailed, err)
	}

	var res *types.Results
	if err := sc.withFoldWorker(id, fc, func(w *workers.Worker) error {
		var err error
		res, err = sc.finalizeOn(id, fc, w, state, payload, results)
		return err
	}); err != nil {
		return nil, err
	}
	if err := sc.store.SetResults(res); err != nil {
		return nil, fmt.Errorf("persist results: %w", err)
	}
	return res, nil
}

// finalizeOn runs the finalize of the chain's last fold on the fold worker w
// and checks its proof against the local state.
func (sc *Scheduler) finalizeOn(id types.ElectionID, fc *foldChain, w *workers.Worker,
	state *chain.State, payload *davinci.ResultsPayload, results []uint64,
) (*types.Results, error) {
	req := &davinci.FinalizeRequest{
		Config:  *state.ChainConfig(),
		FoldJob: fc.lastFold,
		FoldVK:  fc.aggVK,
		Results: *payload,
	}
	if err := sc.claim(w); err != nil {
		return nil, fmt.Errorf("%w: %w", errFinalProofFailed, err)
	}
	defer sc.pool.Release(w)
	finID, err := sc.runJob(w, "finalize", func(c *davinci.Client) (string, error) { return c.SubmitFinalize(req) })
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errFinalProofFailed, err)
	}
	var (
		publics []byte
		snark   *davinci.PlonkSnark
	)
	if err := sc.call(w, "final proof", func(c *davinci.Client) error {
		if publics, err = c.FetchPublics(finID); err != nil {
			return err
		}
		snark, err = c.FetchSnark(finID)
		return err
	}); err != nil {
		return nil, fmt.Errorf("%w: job %s: %w", errFinalProofFailed, finID, err)
	}
	digest, err := chain.ParseDigest(publics)
	if err != nil {
		return nil, fmt.Errorf("%w: parse digest: %w", errFinalProofFailed, err)
	}
	if err := verifyFinalDigest(digest, snark, state, fc.foldCount, fc.batchVK, fc.aggVK, results); err != nil {
		return nil, fmt.Errorf("%w: %w", errVerificationFailed, err)
	}
	log.Infow("finalized election", "election", id.String(), "finalizeJob", finID, "worker", w.Address,
		"tally", results, "aggVK", fc.aggVK, "batchVK", fc.batchVK,
		"programVK", "0x"+hex.EncodeToString(snark.ProgramVK[:]),
		"configCommitment", "0x"+hex.EncodeToString(digest.ConfigCommitment))
	return &types.Results{
		ElectionID:       id,
		Tally:            results,
		ProgramVK:        "0x" + hex.EncodeToString(snark.ProgramVK[:]),
		RootCVadcopFinal: "0x" + hex.EncodeToString(snark.RootCVadcopFinal[:]),
		PublicValues:     "0x" + hex.EncodeToString(snark.PublicValues),
		ProofBytes:       "0x" + hex.EncodeToString(snark.ProofBytes),
		FinalizedAt:      time.Now(),
	}, nil
}

// verifyFinalDigest runs the external consistency and vk-binding checks against
// the local state, mirroring chain.Sequencer.Finalize. It also recomputes the
// config commitment (config frame ‖ batch_vk ‖ fold_vk) from the creation
// parameters and, when a circuit release is pinned, requires the proof's
// program_vk to be the release's aggregator vk, so the fold_vk == program_vk
// binding points at a known circuit rather than one the worker chose.
func verifyFinalDigest(d *chain.Digest, snark *davinci.PlonkSnark, state *chain.State, foldCount uint64, batchVK, aggVK string, results []uint64) error {
	if d.Mode != chain.ModeFinalize {
		return fmt.Errorf("finalize digest mode = %d, want %d", d.Mode, chain.ModeFinalize)
	}
	if d.StateRootHex() != state.Root() {
		return fmt.Errorf("digest state root %s != local root %s", d.StateRootHex(), state.Root())
	}
	if uint64(d.StepCount) != foldCount {
		return fmt.Errorf("digest step_count = %d, want %d folds", d.StepCount, foldCount)
	}
	voters, overwrites := state.Voters()
	if uint64(d.TotalVoters) != voters || uint64(d.TotalOverwrites) != overwrites {
		return fmt.Errorf("digest counts (%d, %d) != local (%d, %d)",
			d.TotalVoters, d.TotalOverwrites, voters, overwrites)
	}
	proofVK := "0x" + hex.EncodeToString(snark.ProgramVK[:])
	if err := d.VerifyBinding(proofVK, batchVK); err != nil {
		return err
	}
	for i, r := range results {
		if uint64(d.Results[i]) != r {
			return fmt.Errorf("digest result[%d] = %d, want %d", i, d.Results[i], r)
		}
	}

	// The commitment ties the proof to these election parameters and to this
	// batch/fold circuit pair.
	batchWords, err := chain.VKWords(batchVK)
	if err != nil {
		return fmt.Errorf("batch vk words: %w", err)
	}
	aggWords, err := chain.VKWords(aggVK)
	if err != nil {
		return fmt.Errorf("agg vk words: %w", err)
	}
	wantCommit, err := state.ConfigCommitment(batchWords, aggWords)
	if err != nil {
		return fmt.Errorf("recompute config commitment: %w", err)
	}
	if !bytes.Equal(d.ConfigCommitment, wantCommit[:]) {
		return fmt.Errorf("config commitment mismatch: digest %x != recomputed %x",
			d.ConfigCommitment, wantCommit[:])
	}

	// Anchor both vks to the pinned release instead of trusting what the
	// workers report.
	if chain.CircuitRelease.IsSet() {
		if !strings.EqualFold(strings.TrimPrefix(proofVK, "0x"), strings.TrimPrefix(chain.CircuitRelease.AggVK, "0x")) {
			return fmt.Errorf("proof program_vk %s != release agg vk %s", proofVK, chain.CircuitRelease.AggVK)
		}
		if err := chain.CircuitRelease.Verify(aggVK, batchVK); err != nil {
			return err
		}
	}
	return nil
}
