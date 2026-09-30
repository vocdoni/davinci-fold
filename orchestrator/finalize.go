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

// Finalize closes an election's chain: it drains any pending batches into the
// fold head, decrypts the accumulators with privKey, runs the finalize +
// final PLONK on the pinned fold worker, verifies the returned digest against
// the local state (the same external checks chain.Sequencer.Finalize performs),
// and persists the resulting Results. The privKey is the keywarden-provided
// decryption scalar; it is used only here and never stored. An error wraps
// the reason of the step that failed (see finalizeSummary).
func (sc *Scheduler) Finalize(id types.ElectionID, privKey *big.Int) (*types.Results, error) {
	rt, ok := sc.engine.runtime(id)
	if !ok {
		return nil, fmt.Errorf("unknown election %s", id.String())
	}

	// Drain any remaining imported batches into the fold head.
	if err := sc.Fold(id); err != nil {
		return nil, fmt.Errorf("%w: %w", errFinalFoldFailed, err)
	}

	fc, err := sc.chain(id)
	if err != nil {
		return nil, err
	}
	sc.mu.Lock()
	lastFold, aggVK, batchVK, foldCount := fc.lastFold, fc.aggVK, fc.batchVK, fc.foldCount
	w := fc.foldWorker
	sc.mu.Unlock()
	if lastFold == "" {
		return nil, fmt.Errorf("%w: no fold in the chain", errNothingToFinalize)
	}

	// The election has ended: its state no longer changes.
	state := rt.current()
	chainCfg := *state.ChainConfig()
	payload, results, err := state.ResultsPayload(privKey)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errDecryptionFailed, err)
	}

	finID, err := sc.runFinalize(w, &davinci.FinalizeRequest{
		Config:  chainCfg,
		FoldJob: lastFold,
		FoldVK:  aggVK,
		Results: *payload,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errFinalProofFailed, err)
	}

	publics, err := w.Client().FetchPublics(finID)
	if err != nil {
		return nil, fmt.Errorf("%w: FetchPublics %s: %w", errFinalProofFailed, finID, err)
	}
	digest, err := chain.ParseDigest(publics)
	if err != nil {
		return nil, fmt.Errorf("%w: parse digest: %w", errFinalProofFailed, err)
	}
	snark, err := w.Client().FetchSnark(finID)
	if err != nil {
		return nil, fmt.Errorf("%w: FetchSnark %s: %w", errFinalProofFailed, finID, err)
	}

	if err := verifyFinalDigest(digest, snark, state, foldCount, batchVK, aggVK, results); err != nil {
		return nil, fmt.Errorf("%w: %w", errVerificationFailed, err)
	}

	res := &types.Results{
		ElectionID:       id,
		Tally:            results,
		ProgramVK:        "0x" + hex.EncodeToString(snark.ProgramVK[:]),
		RootCVadcopFinal: "0x" + hex.EncodeToString(snark.RootCVadcopFinal[:]),
		PublicValues:     "0x" + hex.EncodeToString(snark.PublicValues),
		ProofBytes:       "0x" + hex.EncodeToString(snark.ProofBytes),
		FinalizedAt:      time.Now(),
	}
	if err := sc.store.SetResults(res); err != nil {
		return nil, fmt.Errorf("persist results: %w", err)
	}
	log.Infow("finalized election", "election", id.String(), "finalizeJob", finID, "tally", results,
		"aggVK", aggVK, "batchVK", batchVK, "programVK", res.ProgramVK,
		"configCommitment", "0x"+hex.EncodeToString(digest.ConfigCommitment))
	return res, nil
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

// runFinalize submits a finalize job to the fold worker and waits for it,
// resubmitting the identical request on failure up to maxJobAttempts times.
func (sc *Scheduler) runFinalize(w *workers.Worker, req *davinci.FinalizeRequest) (string, error) {
	var lastErr error
	for attempt := 1; attempt <= maxJobAttempts; attempt++ {
		id, err := w.Client().SubmitFinalize(req)
		if err != nil {
			return "", fmt.Errorf("submit finalize: %w", err)
		}
		err = sc.waitJob(w, id)
		if err == nil {
			sc.pool.WorkerResult(w.Address, true)
			return id, nil
		}
		lastErr = fmt.Errorf("finalize job %s (attempt %d/%d): %w", id, attempt, maxJobAttempts, err)
		sc.pool.WorkerResult(w.Address, false)
		if errors.Is(err, errWorkerRemoved) || sc.ctx.Err() != nil {
			break
		}
	}
	return "", lastErr
}
