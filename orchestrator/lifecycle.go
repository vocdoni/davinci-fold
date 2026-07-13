package orchestrator

import (
	"fmt"
	"math/big"

	"github.com/vocdoni/davinci-fold/log"

	"github.com/vocdoni/davinci-fold/types"
)

// drainAndPublish drives a just-Ended election to Decrypting: it drains any
// remaining batches and pending folds onto the fold chain, then publishes the
// encrypted results by advancing the status. The ciphertext itself is recomputed
// from the live State on demand (EncryptedResults), so "publishing" is the status
// move. Guarded by e.draining so only one drive runs per election at a time.
func (e *Engine) drainAndPublish(rt *electionRuntime) {
	id := rt.id
	if _, busy := e.draining.LoadOrStore(id.String(), struct{}{}); busy {
		return
	}
	defer e.draining.Delete(id.String())

	if e.scheduler == nil {
		return // ingest-only mode cannot drain or finalize
	}
	if err := e.scheduler.Dispatch(id); err != nil {
		log.Warnw("drain dispatch failed", "election", id.String(), "error", err.Error())
		return
	}
	if err := e.scheduler.Fold(id); err != nil {
		log.Warnw("drain fold failed", "election", id.String(), "error", err.Error())
		return
	}
	if err := e.store.SetElectionStatus(id, types.StatusDecrypting); err != nil {
		log.Warnw("set decrypting failed", "election", id.String(), "error", err.Error())
		return
	}
	e.audit("system", "system", "publish_encrypted_results", id)
	log.Infow("encrypted results published",
		"election", id.String(), "ciphertext", len(rt.state.EncryptedResults()))
}

// EncryptedResults returns the published results ciphertext (NumFields ElGamal
// ciphertexts as 4 Twisted-Edwards little-endian coords each), available once
// the election has reached Decrypting. This is what the keywarden fetches
// before returning a key.
func (e *Engine) EncryptedResults(id types.ElectionID) ([]string, error) {
	el, err := e.store.Election(id)
	if err != nil {
		return nil, err
	}
	if el.Status < types.StatusDecrypting {
		return nil, fmt.Errorf("election %s not decrypting yet (status %s)", id.String(), el.Status)
	}
	rt, ok := e.runtime(id)
	if !ok {
		return nil, fmt.Errorf("election %s not loaded", id.String())
	}
	return rt.state.EncryptedResults(), nil
}

// SubmitDecryptionKey accepts the keywarden's decryption key (v1: the raw ElGamal
// private scalar) for a Decrypting election, runs the finalize (decrypt + final
// PLONK + external digest verification), persists the Results, and advances the
// election to Results. On finalize failure the status rolls back to Decrypting so
// the keywarden can retry with the same key.
func (e *Engine) SubmitDecryptionKey(subject string, id types.ElectionID, key *big.Int) (*types.Results, error) {
	el, err := e.store.Election(id)
	if err != nil {
		return nil, err
	}
	if el.Status != types.StatusDecrypting {
		return nil, fmt.Errorf("election %s not awaiting a decryption key (status %s)", id.String(), el.Status)
	}
	if e.scheduler == nil {
		return nil, fmt.Errorf("no scheduler configured: cannot finalize")
	}
	// Single-flight the finalize: even if two callers both observed Decrypting
	// above, only one acquires the guard. Without it a second finalize would
	// re-run the PLONK and, on a transient error, roll the status back from
	// Results to Decrypting.
	if _, busy := e.finalizing.LoadOrStore(id.String(), struct{}{}); busy {
		return nil, fmt.Errorf("election %s finalize already in progress", id.String())
	}
	defer e.finalizing.Delete(id.String())
	if err := e.store.SetElectionStatus(id, types.StatusFinalizing); err != nil {
		return nil, fmt.Errorf("set finalizing: %w", err)
	}
	res, err := e.scheduler.Finalize(id, key)
	if err != nil {
		if rbErr := e.store.SetElectionStatus(id, types.StatusDecrypting); rbErr != nil {
			log.Warnw("rollback to decrypting failed", "election", id.String(), "error", rbErr.Error())
		}
		return nil, fmt.Errorf("finalize: %w", err)
	}
	if err := e.store.SetElectionStatus(id, types.StatusResults); err != nil {
		return nil, fmt.Errorf("set results: %w", err)
	}
	e.audit(subject, "keywarden", "submit_decryption_key", id)
	log.Infow("election results finalized", "election", id.String(), "tally", res.Tally)
	return res, nil
}

// Results returns the finalized tally + PLONK for an election, if available.
func (e *Engine) Results(id types.ElectionID) (*types.Results, error) {
	return e.store.Results(id)
}
