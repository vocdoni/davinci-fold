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

// sealLocked drains up to batchSize pending votes, applies them to the state
// tree and persists the exact, re-drivable batch prove request plus the new
// state snapshot. The caller must hold rt.mu.
func (e *Engine) sealLocked(rt *electionRuntime) error {
	n := len(rt.pending)
	if n == 0 {
		return nil
	}
	if n > e.batchSize {
		n = e.batchSize
	}
	batch := rt.pending[:n]

	votes := make([]chain.Vote, n)
	bundles := make([]voteProofBundle, n)
	for i, v := range batch {
		ballot := elgamal.NewBallot(rt.cfg.EncKey)
		if err := ballot.Deserialize(v.Ballot); err != nil {
			return fmt.Errorf("deserialize ballot %s: %w", v.ID.String(), err)
		}
		votes[i] = chain.Vote{
			CensusIdx:   v.CensusIdx,
			VoteID:      v.VoteIDKey,
			AddressLo16: v.AddressLo16,
			Ballot:      ballot,
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
		Output:       "stark",
		State:        stateData,
		Reencryption: reenc,
	}
	if el, err := e.store.Election(rt.id); err == nil {
		req.VK = json.RawMessage(el.Config.VK)
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

	rt.pending = rt.pending[n:]
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
