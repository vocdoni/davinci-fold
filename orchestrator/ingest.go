package orchestrator

import (
	"fmt"
	"math/big"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/vocdoni/davinci-fold/log"
	"github.com/vocdoni/davinci-fold/types"
)

// SubmitVote validates, de-duplicates and persists a self-authenticating vote,
// appending it to the election's ordered log and sealing a batch once the
// pending buffer is full. Returns the persisted vote. A rejected vote's error
// wraps one of the Err* reasons.
func (e *Engine) SubmitVote(id types.ElectionID, sub *VoteSubmission) (*types.Vote, error) {
	el, err := e.store.Election(id)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrElectionNotFound, id)
	}
	if el.Status != types.StatusActive {
		return nil, fmt.Errorf("%w: status %s", ErrNotAcceptingVotes, el.Status)
	}
	rt, ok := e.runtime(id)
	if !ok {
		return nil, fmt.Errorf("election %s not loaded", id)
	}

	voteID := types.VoteID(sub.VoteID)

	// Per-voteID in-flight + replay dedup. The lock is acquired atomically so two
	// concurrent submissions of the same voteID cannot both proceed.
	if !e.store.LockVoteID(voteID) {
		return nil, fmt.Errorf("%w: vote %s is being processed", ErrVoteAlreadySubmitted, voteID)
	}
	defer e.store.ReleaseVoteID(voteID)
	if e.store.VoteExists(id, voteID) {
		return nil, fmt.Errorf("%w: duplicate vote %s", ErrVoteAlreadySubmitted, voteID)
	}

	// Per-(election,address) exclusivity for the duration of ingest, so two
	// concurrent ballots from the same voter cannot race.
	addr := new(big.Int).SetBytes(sub.Address)
	if !e.store.LockAddress(id, addr) {
		return nil, fmt.Errorf("%w: a vote from this address is being processed", ErrVoteAlreadySubmitted)
	}
	defer e.store.ReleaseAddress(id, addr)

	bundle, err := e.validator.Validate(rt.rules, sub)
	if err != nil {
		return nil, err
	}
	// The validator checked that the census leaf binds sub.Address, so this
	// is the slot of the submitting voter.
	slot, err := bundle.Census.SlotKey()
	if err != nil {
		return nil, fmt.Errorf("%w: slot: %w", ErrInvalidCensusProof, err)
	}

	payload, err := cbor.Marshal(bundle)
	if err != nil {
		return nil, fmt.Errorf("encode payload: %w", err)
	}

	v := &types.Vote{
		ID:          voteID,
		Address:     sub.Address,
		Slot:        slot,
		VoteIDKey:   sub.VoteIDKey,
		Ballot:      sub.Ballot,
		Payload:     payload,
		SubmittedAt: time.Now(),
	}

	// The status is checked again under rt.mu, which status changes hold: a
	// vote is either in the pending buffer before the election is paused,
	// ended or canceled, or rejected.
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if el, err = e.store.Election(id); err != nil {
		return nil, fmt.Errorf("load election: %w", err)
	}
	if el.Status != types.StatusActive {
		return nil, fmt.Errorf("%w: status %s", ErrNotAcceptingVotes, el.Status)
	}
	if err := e.store.AddVote(id, v); err != nil {
		return nil, fmt.Errorf("persist vote: %w", err)
	}
	rt.pending = append(rt.pending, v)
	// The vote is accepted: a batch that fails to seal now is sealed again
	// by the monitor.
	if err := e.sealFullLocked(rt); err != nil {
		log.Warnw("failed to seal batch", "election", id.String(), "error", err.Error())
	}

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
