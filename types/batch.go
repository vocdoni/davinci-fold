package types

import "time"

// BatchInput is the persisted, re-drivable record of one sealed batch: the
// exact prove request body shipped to a worker plus the bookkeeping needed to
// re-dispatch it if the worker dies, so a batch can always be proved again.
type BatchInput struct {
	ElectionID ElectionID `cbor:"electionID"`
	Seq        uint64     `cbor:"seq"` // batch sequence within the election
	// ProveRequest is the marshaled davinci.ProveRequest (output=stark).
	ProveRequest []byte `cbor:"proveRequest"`
	// NewStateRoot is the resulting state root after this batch (0x arbo LE).
	NewStateRoot string `cbor:"newStateRoot"`
	// VoteIDs included in this batch, in order.
	VoteIDs []VoteID `cbor:"voteIDs"`
	// Worker is the URL of the worker the batch's prove job was sent to.
	Worker string `cbor:"worker,omitempty"`
	// JobID is the prove job on Worker, stored when the job is submitted so a
	// restart can reuse it (empty until dispatched).
	JobID string `cbor:"jobID,omitempty"`
	// ImportedID is the job ID of the batch STARK imported onto FoldWorker
	// (empty until imported).
	ImportedID string `cbor:"importedID,omitempty"`
	// FoldWorker is the URL of the fold worker ImportedID lives on.
	FoldWorker string    `cbor:"foldWorker,omitempty"`
	SealedAt   time.Time `cbor:"sealedAt"`
}

// FoldCheckpointVersion is the FoldCheckpoint format the orchestrator writes.
// Version 0 checkpoints stored the fold count in BatchesFolded.
const FoldCheckpointVersion = 1

// FoldCheckpoint is the persisted head of an election's fold chain. After a
// crash the orchestrator resumes folding from here.
type FoldCheckpoint struct {
	ElectionID ElectionID `cbor:"electionID"`
	// FoldCount is the number of fold steps applied so far (matches the
	// aggregator digest step_count).
	FoldCount uint64 `cbor:"foldCount"`
	// LastFoldJob is the fold job ID on FoldWorker, chained as prev_fold_job
	// into the next fold.
	LastFoldJob string `cbor:"lastFoldJob,omitempty"`
	// FoldWorker is the URL of the worker LastFoldJob lives on.
	FoldWorker string `cbor:"foldWorker,omitempty"`
	// StateRoot is the state root the fold chain attests up to: the new root
	// of the last batch folded.
	StateRoot string `cbor:"stateRoot"`
	// BatchesFolded is the number of batch STARKs folded so far (the fold
	// count in version 0 checkpoints).
	BatchesFolded uint64 `cbor:"batchesFolded"`
	// AggVK is the aggregator program_vk bound by the genesis fold (0x BE hex).
	AggVK string `cbor:"aggVK,omitempty"`
	// BatchVK is the batch circuit program_vk learned at runtime (0x BE hex).
	BatchVK   string    `cbor:"batchVK,omitempty"`
	UpdatedAt time.Time `cbor:"updatedAt"`
	// Version is the checkpoint format, FoldCheckpointVersion when written.
	Version uint8 `cbor:"version,omitempty"`
}

// Results is the final tally and on-chain PLONK for a finalized election.
type Results struct {
	ElectionID ElectionID `cbor:"electionID"`
	// Tally is the per-field decrypted result.
	Tally []uint64 `cbor:"tally"`
	// ProgramVK, RootCVadcopFinal, PublicValues and ProofBytes are the four
	// Solidity-ready PLONK arguments as 0x hex.
	ProgramVK        string    `cbor:"programVK"`
	RootCVadcopFinal string    `cbor:"rootCVadcopFinal"`
	PublicValues     string    `cbor:"publicValues"`
	ProofBytes       string    `cbor:"proofBytes"`
	FinalizedAt      time.Time `cbor:"finalizedAt"`
}
