package orchestrator

import (
	"bytes"
	"encoding/hex"
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

// Finalize closes an election's chain: it drains any pending batches into the
// fold head, decrypts the accumulators with privKey, runs the finalize +
// final PLONK on the pinned fold worker, verifies the returned digest against
// the local state (the same external checks chain.Sequencer.Finalize performs),
// and persists the resulting Results. The privKey is the keywarden-provided
// decryption scalar; it is used only here and never stored.
func (sc *Scheduler) Finalize(id types.ElectionID, privKey *big.Int) (*types.Results, error) {
	rt, ok := sc.engine.runtime(id)
	if !ok {
		return nil, fmt.Errorf("unknown election %s", id.String())
	}

	// Drain any remaining imported batches into the fold head.
	if err := sc.Fold(id); err != nil {
		return nil, fmt.Errorf("final fold: %w", err)
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
		return nil, fmt.Errorf("nothing to finalize: no fold in the chain")
	}

	chainCfg := *rt.state.ChainConfig()
	payload, results, err := rt.state.ResultsPayload(privKey)
	if err != nil {
		return nil, fmt.Errorf("ResultsPayload: %w", err)
	}

	finID, err := sc.runFinalize(w, &davinci.FinalizeRequest{
		Config:  chainCfg,
		FoldJob: lastFold,
		FoldVK:  aggVK,
		Results: *payload,
	})
	if err != nil {
		return nil, err
	}

	publics, err := w.Client().FetchPublics(finID)
	if err != nil {
		return nil, fmt.Errorf("FetchPublics %s: %w", finID, err)
	}
	digest, err := chain.ParseDigest(publics)
	if err != nil {
		return nil, fmt.Errorf("parse digest: %w", err)
	}
	snark, err := w.Client().FetchSnark(finID)
	if err != nil {
		return nil, fmt.Errorf("FetchSnark %s: %w", finID, err)
	}

	if err := verifyFinalDigest(digest, snark, rt.state, foldCount, batchVK, aggVK, results); err != nil {
		return nil, err
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

// verifyFinalDigest runs the external consistency + vk-binding checks against
// the local state, mirroring chain.Sequencer.Finalize. On top of those it
// proves e2e verifiability: it recomputes the election-identity commitment from
// the declared initial parameters (config frame ‖ batch_vk ‖ fold_vk) and
// checks it against the digest, and — when a circuit release is pinned —
// anchors the proof's program_vk to that release so the fold_vk==program_vk
// knot binds to a known circuit rather than a self-chosen one.
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

	// Parameter binding: recompute the election-identity commitment from the
	// declared initial parameters plus the runtime-learned vks and check it
	// against the digest. This proves the proof attests exactly these params
	// and this circuit pair, and verifies host/guest commitment parity.
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

	// Absolute anchor: when a canonical circuit release is pinned, the proof's
	// program_vk must equal the release aggregator vk, and the runtime-learned
	// vks must match the release. This grounds the fold_vk==program_vk knot in a
	// known-good circuit instead of trusting whatever the worker reports.
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
		if _, err := w.Client().WaitForJob(id, sc.timeout); err == nil {
			sc.pool.WorkerResult(w.Address, true)
			return id, nil
		} else {
			lastErr = fmt.Errorf("finalize job %s (attempt %d/%d): %w", id, attempt, maxJobAttempts, err)
			sc.pool.WorkerResult(w.Address, false)
		}
	}
	return "", lastErr
}
