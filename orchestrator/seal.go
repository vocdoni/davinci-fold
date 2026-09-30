package orchestrator

import (
	"encoding/json"
	"fmt"
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
// plus the new state snapshot. The caller must hold rt.mu.
func (e *Engine) sealLocked(rt *electionRuntime) error {
	batch, rest := nextBatch(rt.pending, rt.batchSize)
	n := len(batch)
	if n == 0 {
		return nil
	}
	el, err := e.store.Election(rt.id)
	if err != nil {
		return fmt.Errorf("load election: %w", err)
	}

	votes := make([]chain.Vote, n)
	bundles := make([]voteProofBundle, n)
	for i, v := range batch {
		ballot := elgamal.NewBallot(rt.cfg.EncKey)
		if err := ballot.Deserialize(v.Ballot); err != nil {
			return fmt.Errorf("deserialize ballot %s: %w", v.ID.String(), err)
		}
		votes[i] = chain.Vote{
			Slot:   v.Slot,
			VoteID: v.VoteIDKey,
			Ballot: ballot,
		}
		if err := cbor.Unmarshal(v.Payload, &bundles[i]); err != nil {
			return fmt.Errorf("decode payload %s: %w", v.ID.String(), err)
		}
	}

	stateData, reenc, err := rt.state.ApplyBatch(votes)
	if err != nil {
		return fmt.Errorf("apply batch: %w", err)
	}

	req := davinci.ProveRequest{
		VK:           json.RawMessage(el.Config.VK),
		Output:       "stark",
		State:        stateData,
		Reencryption: reenc,
	}
	for i := range bundles {
		req.Proofs = append(req.Proofs, bundles[i].Proof)
		req.PublicInputs = append(req.PublicInputs, bundles[i].PublicInputs)
		req.Sigs = append(req.Sigs, bundles[i].Sig)
		req.CensusProofs = append(req.CensusProofs, bundles[i].Census)
	}
	reqBytes, err := json.Marshal(&req)
	if err != nil {
		return fmt.Errorf("marshal prove request: %w", err)
	}

	voteIDs := make([]types.VoteID, n)
	for i, v := range batch {
		voteIDs[i] = v.ID
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

	for _, v := range batch {
		if err := e.store.SetVoteStatus(rt.id, v.ID, types.VoteStatusBatched); err != nil {
			log.Warnw("failed to set vote batched", "vote", v.ID.String(), "error", err.Error())
		}
	}

	rt.pending = rest
	rt.batchSeq++
	e.audit("system", "system", "seal_batch", rt.id)
	log.Infow("sealed batch", "election", rt.id.String(), "seq", seq, "votes", n, "root", rt.state.Root())

	// Hand the freshly sealed batch to the scatter/gather scheduler. Notify is
	// non-blocking and coalescing, so holding rt.mu here is safe.
	if e.scheduler != nil {
		e.scheduler.Notify(rt.id)
	}
	return nil
}
