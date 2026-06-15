package types

import "time"

// BatchInput is the persisted, re-drivable record of one sealed batch: the
// exact prove request body shipped to a worker plus the bookkeeping needed to
// re-dispatch it if the worker dies. The orchestrator never loses this, so a
// batch is always re-provable.
type BatchInput struct {
	ElectionID ElectionID `cbor:"electionID"`
	Seq        uint64     `cbor:"seq"` // batch sequence within the election
	// ProveRequest is the marshaled davinci.ProveRequest (output=stark).
	ProveRequest []byte `cbor:"proveRequest"`
	// NewStateRoot is the resulting state root after this batch (0x arbo LE).
	NewStateRoot string `cbor:"newStateRoot"`
	// VoteIDs included in this batch, in order.
	VoteIDs []VoteID `cbor:"voteIDs"`
	// Worker is the URL of the worker currently assigned to prove this batch.
	Worker string `cbor:"worker,omitempty"`
	// JobID is the prove job on Worker (empty until dispatched).
	JobID string `cbor:"jobID,omitempty"`
	// ImportedID is the local job ID after importing the STARK onto the fold
	// worker (empty until imported).
	ImportedID string    `cbor:"importedID,omitempty"`
	SealedAt   time.Time `cbor:"sealedAt"`
}

// FoldCheckpoint is the persisted head of an election's fold chain. After a
// crash the orchestrator resumes folding from here.
type FoldCheckpoint struct {
	ElectionID ElectionID `cbor:"electionID"`
	// FoldCount is the number of fold steps applied so far (matches the
	// aggregator digest step_count).
	FoldCount uint64 `cbor:"foldCount"`
	// LastFoldJob is the fold job ID on the fold worker, chained as
	// prev_fold_job into the next fold.
	LastFoldJob string `cbor:"lastFoldJob,omitempty"`
	// StateRoot is the state root the fold chain attests up to.
	StateRoot string `cbor:"stateRoot"`
	// BatchesFolded is the count of batch STARKs folded so far.
	BatchesFolded uint64 `cbor:"batchesFolded"`
	// AggVK is the aggregator program_vk bound by the genesis fold (0x BE hex).
	AggVK string `cbor:"aggVK,omitempty"`
	// BatchVK is the batch circuit program_vk learned at runtime (0x BE hex).
	BatchVK   string    `cbor:"batchVK,omitempty"`
	UpdatedAt time.Time `cbor:"updatedAt"`
}

// Results is the final tally and on-chain PLONK for a finalized election.
type Results struct {
	ElectionID ElectionID `cbor:"electionID"`
	// Tally is the per-field decrypted result.
	Tally []uint64 `cbor:"tally"`
	// PlonkSnark holds the four Solidity-ready hex strings.
	ProgramVK        string    `cbor:"programVK"`
	RootCVadcopFinal string    `cbor:"rootCVadcopFinal"`
	PublicValues     string    `cbor:"publicValues"`
	ProofBytes       string    `cbor:"proofBytes"`
	FinalizedAt      time.Time `cbor:"finalizedAt"`
}
