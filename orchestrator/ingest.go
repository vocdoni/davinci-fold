package orchestrator

import (
	"fmt"
	"math/big"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/vocdoni/davinci-fold/types"
)

// SubmitVote validates, de-duplicates and persists a self-authenticating vote,
// appending it to the election's ordered log and sealing a batch once the
// pending buffer is full. Returns the persisted vote.
func (e *Engine) SubmitVote(id types.ElectionID, sub *VoteSubmission) (*types.Vote, error) {
	rt, ok := e.runtime(id)
	if !ok {
		return nil, fmt.Errorf("unknown election %s", id.String())
	}
	el, err := e.store.Election(id)
	if err != nil {
		return nil, err
	}
	if el.Status != types.StatusActive {
		return nil, fmt.Errorf("election %s not accepting votes (status %s)", id.String(), el.Status)
	}

	voteID := types.VoteID(sub.VoteID)

	// Per-voteID in-flight + replay dedup. The lock is acquired atomically so two
	// concurrent submissions of the same voteID cannot both proceed.
	if !e.store.LockVoteID(voteID) {
		return nil, fmt.Errorf("vote already being processed")
	}
	defer e.store.ReleaseVoteID(voteID)
	if e.store.VoteExists(id, voteID) {
		return nil, fmt.Errorf("duplicate vote")
	}

	// Per-(election,address) exclusivity for the duration of ingest, so two
	// concurrent ballots from the same voter cannot race.
	addr := new(big.Int).SetBytes(sub.Address)
	if !e.store.LockAddress(id, addr) {
		return nil, fmt.Errorf("a vote from this address is already in progress")
	}
	defer e.store.ReleaseAddress(id, addr)

	if err := e.validator.Validate(rt.cfg, sub); err != nil {
		return nil, fmt.Errorf("invalid vote: %w", err)
	}

	payload, err := cbor.Marshal(&voteProofBundle{
		Proof:        sub.Proof,
		PublicInputs: sub.PublicInputs,
		Sig:          sub.Sig,
		Census:       sub.Census,
	})
	if err != nil {
		return nil, fmt.Errorf("encode payload: %w", err)
	}

	v := &types.Vote{
		ID:          voteID,
		Address:     sub.Address,
		CensusIdx:   sub.CensusIdx,
		AddressLo16: sub.AddressLo16,
		VoteIDKey:   sub.VoteIDKey,
		Ballot:      sub.Ballot,
		Payload:     payload,
		SubmittedAt: time.Now(),
	}
	if err := e.store.AddVote(id, v); err != nil {
		return nil, fmt.Errorf("persist vote: %w", err)
	}

	rt.mu.Lock()
	rt.pending = append(rt.pending, v)
	full := len(rt.pending) >= el.BatchSize
	if full {
		if err := e.sealLocked(rt); err != nil {
			rt.mu.Unlock()
			return nil, fmt.Errorf("seal batch: %w", err)
		}
	}
	rt.mu.Unlock()

	e.audit("voter", "voter", "submit_vote", id)
	return v, nil
}

// VoteRecord returns a persisted vote and its current pipeline status.
func (e *Engine) VoteRecord(id types.ElectionID, voteID types.VoteID) (*types.Vote, types.VoteStatus, error) {
	v, err := e.store.Vote(id, voteID)
	if err != nil {
		return nil, 0, err
	}
	st, err := e.store.VoteStatus(id, voteID)
	if err != nil {
		return nil, 0, err
	}
	return v, st, nil
}
