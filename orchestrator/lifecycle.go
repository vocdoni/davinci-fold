package orchestrator

import (
	"fmt"
	"math/big"
	"slices"

	"github.com/vocdoni/davinci-fold/log"
	bjjgnark "github.com/vocdoni/davinci-zkvm/go-sdk/vocdoni/crypto/ecc/bjj_gnark"

	"github.com/vocdoni/davinci-fold/types"
)

// statusChanges are the status changes an organizer may request, by current
// status. The end time ends an election through the same path.
var statusChanges = map[types.Status][]types.Status{
	types.StatusActive: {types.StatusPaused, types.StatusEnded, types.StatusCanceled},
	types.StatusPaused: {types.StatusActive, types.StatusEnded, types.StatusCanceled},
}

// statusActions names each status change in the audit log.
var statusActions = map[types.Status]string{
	types.StatusActive:   "resume_election",
	types.StatusPaused:   "pause_election",
	types.StatusEnded:    "end_election",
	types.StatusCanceled: "cancel_election",
}

// transitionError is the ErrInvalidTransition for moving from to to.
func transitionError(from, to types.Status) error {
	return fmt.Errorf("%w: election is %s, cannot become %s", ErrInvalidTransition, from, to)
}

// SetStatus changes an election's status on its organizer's request: active
// to paused and back; active or paused to ended, which seals the pending votes
// and then waits for the decryption key as at the end time; active or paused
// to canceled, after which nothing is sealed, proved or folded for it (it
// stays readable and its votes are marked as error). Any other change fails
// with ErrInvalidTransition.
func (e *Engine) SetStatus(subject string, id types.ElectionID, to types.Status) error {
	el, err := e.store.Election(id)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrElectionNotFound, id)
	}
	rt, ok := e.runtime(id)
	if !ok {
		if _, open := statusChanges[el.Status]; open {
			return fmt.Errorf("election %s not loaded", id)
		}
		// Only open elections are loaded.
		return transitionError(el.Status, to)
	}
	return e.changeStatus(rt, subject, "admin", to)
}

// changeStatus moves an active or paused election to status to. It holds
// rt.mu throughout, so no vote enters or leaves the pending buffer while the
// status changes.
func (e *Engine) changeStatus(rt *electionRuntime, subject, role string, to types.Status) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	el, err := e.store.Election(rt.id)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrElectionNotFound, rt.id)
	}
	from := el.Status
	if !slices.Contains(statusChanges[from], to) {
		return transitionError(from, to)
	}
	switch to {
	case types.StatusEnded:
		// Seal every accepted vote, in as many batches as the slots need.
		for len(rt.pending) > 0 {
			if err := e.sealLocked(rt); err != nil {
				return fmt.Errorf("seal pending votes: %w", err)
			}
		}
	case types.StatusCanceled:
		rt.pending = nil
	}
	if err := e.store.UpdateElection(rt.id, func(stored *types.Election) error {
		if stored.Status != from {
			return transitionError(stored.Status, to)
		}
		stored.Status = to
		return nil
	}); err != nil {
		return err
	}
	if to == types.StatusCanceled {
		e.mu.Lock()
		delete(e.runtimes, rt.id.String())
		e.mu.Unlock()
		e.setVotesStatus(rt.id, types.VoteStatusError)
	}
	e.audit(subject, role, statusActions[to], rt.id)
	log.Infow("election status changed", "election", rt.id.String(),
		"from", from.String(), "to", to.String(), "root", rt.state.Root())
	return nil
}

// setVotesStatus moves every vote of an election to status st, as far as
// storage.SetVoteStatus allows.
func (e *Engine) setVotesStatus(id types.ElectionID, st types.VoteStatus) {
	votes, err := e.store.ListVotes(id)
	if err != nil {
		log.Warnw("failed to list votes", "election", id.String(), "error", err.Error())
		return
	}
	for _, v := range votes {
		if err := e.store.SetVoteStatus(id, v.ID, st); err != nil {
			log.Warnw("failed to set vote status", "vote", v.ID.String(), "status", st.String(), "error", err.Error())
		}
	}
}

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
		"election", id.String(), "ciphertext", len(rt.current().EncryptedResults()))
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
	switch el.Status {
	case types.StatusDecrypting, types.StatusFinalizing, types.StatusResults:
	default:
		return nil, fmt.Errorf("election %s not decrypting yet (status %s)", id.String(), el.Status)
	}
	rt, ok := e.runtime(id)
	if !ok {
		return nil, fmt.Errorf("election %s not loaded", id.String())
	}
	return rt.current().EncryptedResults(), nil
}

// SubmitDecryptionKey takes the keywarden's decryption key (the raw ElGamal
// private scalar) for a decrypting election. It checks the key against the
// election's encryption key, moves the election to finalizing and returns,
// running the finalize (decryption, final PLONK, digest checks) in the
// background. The election then reaches results, or goes back to decrypting
// with a short reason in FinalizeError (see finalizeSummary) so the key can be
// submitted again.
func (e *Engine) SubmitDecryptionKey(subject string, id types.ElectionID, key *big.Int) error {
	el, err := e.store.Election(id)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrElectionNotFound, id)
	}
	if el.Status != types.StatusDecrypting {
		return transitionError(el.Status, types.StatusFinalizing)
	}
	rt, ok := e.runtime(id)
	if !ok {
		return fmt.Errorf("election %s not loaded", id)
	}
	if err := checkDecryptionKey(rt.cfg.EncKey, key); err != nil {
		return err
	}
	if e.finalize == nil {
		return fmt.Errorf("no worker pool configured: cannot finalize")
	}
	// Two callers may both have seen Decrypting above; only one gets the guard
	// (see Engine.finalizing).
	if _, busy := e.finalizing.LoadOrStore(id.String(), struct{}{}); busy {
		return fmt.Errorf("%w: a finalize is already running", ErrInvalidTransition)
	}
	if err := e.store.UpdateElection(id, func(stored *types.Election) error {
		if stored.Status != types.StatusDecrypting {
			return transitionError(stored.Status, types.StatusFinalizing)
		}
		stored.Status = types.StatusFinalizing
		stored.FinalizeError = ""
		return nil
	}); err != nil {
		e.finalizing.Delete(id.String())
		return err
	}
	e.audit(subject, "keywarden", "submit_decryption_key", id)
	go e.finalizeElection(id, key)
	return nil
}

// finalizeElection runs the finalize of an election in Finalizing and moves
// it to Results, marking its votes settled, or back to Decrypting with the
// error.
func (e *Engine) finalizeElection(id types.ElectionID, key *big.Int) {
	res, err := e.finalize(id, key)
	// Released before the status moves on: a key submitted once the election
	// is decrypting again is taken.
	e.finalizing.Delete(id.String())
	if err != nil {
		// The election shows only the reason; the detail (prover addresses,
		// job IDs, proof values) stays in the log and the audit trail.
		reason := finalizeSummary(err)
		log.Warnw("finalize failed", "election", id.String(), "reason", reason, "error", err.Error())
		e.audit("system", "system", "finalize_failed: "+err.Error(), id)
		if uerr := e.store.UpdateElection(id, func(el *types.Election) error {
			el.Status = types.StatusDecrypting
			el.FinalizeError = reason
			return nil
		}); uerr != nil {
			log.Warnw("rollback to decrypting failed", "election", id.String(), "error", uerr.Error())
		}
		return
	}
	if err := e.store.SetElectionStatus(id, types.StatusResults); err != nil {
		log.Warnw("set results failed", "election", id.String(), "error", err.Error())
		return
	}
	e.setVotesStatus(id, types.VoteStatusSettled)
	log.Infow("election results finalized", "election", id.String(), "tally", res.Tally)
}

// checkDecryptionKey checks key is the private key of encKey: in [1, order)
// and key·G = encKey.
func checkDecryptionKey(encKey *bjjgnark.BJJ, key *big.Int) error {
	if key.Sign() <= 0 || key.Cmp(encKey.Order()) >= 0 {
		return fmt.Errorf("%w: out of range", ErrInvalidDecryptionKey)
	}
	pub := encKey.New()
	pub.ScalarBaseMult(key)
	if !pub.Equal(encKey) {
		return fmt.Errorf("%w: it does not match the election's encryption key", ErrInvalidDecryptionKey)
	}
	return nil
}

// Results returns the finalized tally + PLONK for an election, if available.
func (e *Engine) Results(id types.ElectionID) (*types.Results, error) {
	return e.store.Results(id)
}
