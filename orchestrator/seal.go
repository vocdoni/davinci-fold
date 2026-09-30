package orchestrator

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/vocdoni/davinci-fold/log"
	"github.com/vocdoni/davinci-zkvm/go-sdk/vocdoni/crypto/elgamal"

	"github.com/vocdoni/davinci-fold/types"
	davinci "github.com/vocdoni/davinci-zkvm/go-sdk"
	"github.com/vocdoni/davinci-zkvm/go-sdk/chain"
)

// nextBatch splits the pending votes into the next batch, at most size votes
// in submission order with no two on one ballot slot, and the votes left
// pending. The batch guest rejects a batch that writes a slot twice, so a
// later vote for a slot already in the batch (an overwrite by the same voter)
// waits for a following batch; per slot, votes are still applied in
// submission order and the latest one wins.
func nextBatch(pending []*types.Vote, size int) (batch, rest []*types.Vote) {
	slots := make(map[uint64]struct{}, size)
	for _, v := range pending {
		if _, dup := slots[v.Slot]; dup || len(batch) == size {
			rest = append(rest, v)
			continue
		}
		slots[v.Slot] = struct{}{}
		batch = append(batch, v)
	}
	return batch, rest
}

// sealFullLocked seals batches while the pending votes fill a whole one. The
// caller must hold rt.mu.
func (e *Engine) sealFullLocked(rt *electionRuntime) error {
	for {
		if batch, _ := nextBatch(rt.pending, rt.batchSize); len(batch) < rt.batchSize {
			return nil
		}
		if err := e.sealLocked(rt); err != nil {
			return err
		}
	}
}

// sealLocked seals the next batch of pending votes (see nextBatch), applies it
// to the state tree and persists the exact, re-drivable batch prove request
// plus the new state snapshot. A vote the state cannot apply is dropped from
// the batch and marked as error (see applyBatch). The caller must hold rt.mu.
func (e *Engine) sealLocked(rt *electionRuntime) error {
	// A seal that failed after changing the state left it ahead of the
	// persisted snapshot.
	if rt.dirty {
		if err := e.restoreState(rt); err != nil {
			return err
		}
	}
	batch, rest := nextBatch(rt.pending, rt.batchSize)
	if len(batch) == 0 {
		return nil
	}
	el, err := e.store.Election(rt.id)
	if err != nil {
		return fmt.Errorf("load election: %w", err)
	}

	var (
		sealed  []*types.Vote
		votes   []chain.Vote
		bundles []voteProofBundle
	)
	for _, v := range batch {
		ballot := elgamal.NewBallot(rt.cfg.EncKey)
		if err := ballot.Deserialize(v.Ballot); err != nil {
			e.dropVote(rt, v, fmt.Errorf("deserialize ballot: %w", err))
			continue
		}
		var bundle voteProofBundle
		if err := cbor.Unmarshal(v.Payload, &bundle); err != nil {
			e.dropVote(rt, v, fmt.Errorf("decode payload: %w", err))
			continue
		}
		sealed = append(sealed, v)
		votes = append(votes, chain.Vote{Slot: v.Slot, VoteID: v.VoteIDKey, Ballot: ballot})
		bundles = append(bundles, bundle)
	}

	stateData, reenc, refused, err := e.applyBatch(rt, votes)
	if err != nil {
		return err
	}
	if len(refused) > 0 {
		var (
			keptVotes   []*types.Vote
			keptBundles []voteProofBundle
		)
		for i, v := range sealed {
			if reason, ok := refused[i]; ok {
				e.dropVote(rt, v, reason)
				continue
			}
			keptVotes, keptBundles = append(keptVotes, v), append(keptBundles, bundles[i])
		}
		sealed, bundles = keptVotes, keptBundles
	}
	if len(sealed) == 0 {
		rt.pending = rest
		return nil
	}

	req := davinci.ProveRequest{
		VK:           json.RawMessage(el.Config.VK),
		Output:       "stark",
		State:        stateData,
		Reencryption: reenc,
	}
	voteIDs := make([]types.VoteID, len(sealed))
	for i, v := range sealed {
		voteIDs[i] = v.ID
		req.Proofs = append(req.Proofs, bundles[i].Proof)
		req.PublicInputs = append(req.PublicInputs, bundles[i].PublicInputs)
		req.Sigs = append(req.Sigs, bundles[i].Sig)
		req.CensusProofs = append(req.CensusProofs, bundles[i].Census)
	}
	reqBytes, err := json.Marshal(&req)
	if err != nil {
		return fmt.Errorf("marshal prove request: %w", err)
	}

	seq := rt.batchSeq
	if err := e.store.SetBatchInput(&types.BatchInput{
		ElectionID:   rt.id,
		Seq:          seq,
		ProveRequest: reqBytes,
		NewStateRoot: rt.state.Root(),
		VoteIDs:      voteIDs,
		SealedAt:     time.Now(),
	}); err != nil {
		return fmt.Errorf("persist batch: %w", err)
	}

	snapshot, err := rt.state.Snapshot()
	if err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	if err := e.store.SetSnapshot(rt.id, snapshot); err != nil {
		return fmt.Errorf("persist snapshot: %w", err)
	}
	rt.dirty = false

	for _, v := range sealed {
		if err := e.store.SetVoteStatus(rt.id, v.ID, types.VoteStatusBatched); err != nil {
			log.Warnw("failed to set vote batched", "vote", v.ID.String(), "error", err.Error())
		}
	}

	rt.pending = rest
	rt.batchSeq++
	e.audit("system", "system", "seal_batch", rt.id)
	log.Infow("sealed batch", "election", rt.id.String(), "seq", seq, "votes", len(sealed), "root", rt.state.Root())

	// Hand the freshly sealed batch to the scatter/gather scheduler. Notify is
	// non-blocking and coalescing, so holding rt.mu here is safe.
	if e.scheduler != nil {
		e.scheduler.Notify(rt.id)
	}
	return nil
}

// applyBatch applies votes to the state, leaving out the ones it refuses (a
// vote-ID key that collides with a key already in the tree, for example),
// which it returns with the reason, by position in votes. ApplyBatch fails as
// a whole and may leave the tree half written, so after a failure the state
// is restored from the last snapshot and the refused vote is found as the
// last vote of the shortest failing prefix: ApplyBatch applies the votes in
// order, so once a prefix fails every longer one does. The batch is then
// applied again without it. It returns nil blocks when every vote is
// refused. The caller must hold rt.mu.
func (e *Engine) applyBatch(rt *electionRuntime, votes []chain.Vote) (
	*davinci.StateTransitionData, *davinci.ReencryptionData, map[int]error, error,
) {
	idx := make([]int, len(votes)) // positions of the votes still in the batch
	for i := range idx {
		idx[i] = i
	}
	refused := map[int]error{}
	for len(idx) > 0 {
		batch := make([]chain.Vote, len(idx))
		for i, j := range idx {
			batch[i] = votes[j]
		}
		rt.dirty = true
		stateData, reenc, err := rt.state.ApplyBatch(batch)
		if err == nil {
			return stateData, reenc, refused, nil
		}
		if rerr := e.restoreState(rt); rerr != nil {
			return nil, nil, nil, rerr
		}
		lo, hi := 1, len(batch) // batch[:hi] fails
		for lo < hi {
			mid := (lo + hi) / 2
			rt.dirty = true
			_, _, perr := rt.state.ApplyBatch(batch[:mid])
			if rerr := e.restoreState(rt); rerr != nil {
				return nil, nil, nil, rerr
			}
			if perr != nil {
				hi, err = mid, perr
			} else {
				lo = mid + 1
			}
		}
		refused[idx[hi-1]] = err
		idx = slices.Delete(idx, hi-1, hi)
	}
	return nil, nil, refused, nil
}

// restoreState replaces the runtime's state with the persisted snapshot,
// taken after the last sealed batch. The caller must hold rt.mu.
func (e *Engine) restoreState(rt *electionRuntime) error {
	blob, err := e.store.Snapshot(rt.id)
	if err != nil {
		return fmt.Errorf("load snapshot: %w", err)
	}
	state, err := chain.RestoreState(rt.cfg, blob)
	if err != nil {
		return fmt.Errorf("restore state: %w", err)
	}
	rt.state = state
	rt.dirty = false
	return nil
}

// dropVote takes a vote that cannot be sealed out of the pending buffer and
// marks it as error. The caller must hold rt.mu.
func (e *Engine) dropVote(rt *electionRuntime, v *types.Vote, reason error) {
	log.Warnw("dropping a vote the state refuses",
		"election", rt.id.String(), "vote", v.ID.String(), "error", reason.Error())
	rt.pending = slices.DeleteFunc(rt.pending, func(p *types.Vote) bool { return p == v })
	if err := e.store.SetVoteStatus(rt.id, v.ID, types.VoteStatusError); err != nil {
		log.Warnw("failed to set vote error", "vote", v.ID.String(), "error", err.Error())
	}
}
