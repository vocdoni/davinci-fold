package api

import (
	"encoding/json"
	"time"

	"github.com/vocdoni/davinci-fold/workers"
)

// InfoResponse is returned by GET /info.
type InfoResponse struct {
	Version   string `json:"version"`
	BatchSize int    `json:"batchSize"`
	FoldEvery int    `json:"foldEvery"`
	Workers   int    `json:"workers"`
	Elections int    `json:"elections"`
}

// ElectionCreateRequest is the admin body for POST /elections. The election ID
// is derived from ProcessID. All field-element values are big-endian hex (0x
// optional); EncX/EncY are the ElGamal public key's canonical RTE coordinates.
type ElectionCreateRequest struct {
	ProcessID    string          `json:"processID"`
	BallotMode   string          `json:"ballotMode"`
	EncX         string          `json:"encX"`
	EncY         string          `json:"encY"`
	CensusOrigin uint64          `json:"censusOrigin"`
	CensusRoot   string          `json:"censusRoot"`
	VK           json.RawMessage `json:"vk"`
	BatchSize    int             `json:"batchSize,omitempty"`
	FoldEvery    int             `json:"foldEvery,omitempty"`
	EndTime      time.Time       `json:"endTime,omitempty"`
}

// ElectionResponse is the public view of an election record.
type ElectionResponse struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	BatchSize int       `json:"batchSize"`
	FoldEvery int       `json:"foldEvery"`
	EndTime   time.Time `json:"endTime"`
	CreatedAt time.Time `json:"createdAt"`
}

// ElectionsResponse lists elections.
type ElectionsResponse struct {
	Elections []*ElectionResponse `json:"elections"`
}

// VoteReceiptResponse acknowledges an accepted vote.
type VoteReceiptResponse struct {
	VoteID string `json:"voteID"`
	Status string `json:"status"`
}

// VoteStatusResponse reports a vote's pipeline status.
type VoteStatusResponse struct {
	VoteID string `json:"voteID"`
	Status string `json:"status"`
	Seq    uint64 `json:"seq"`
}

// EncryptedResultsResponse is the published results ciphertext served to the
// keywarden. The JSON shape matches keywarden.EncryptedResultsResponse.
type EncryptedResultsResponse struct {
	ElectionID string   `json:"election_id"`
	Ciphertext []string `json:"ciphertext"`
}

// DecryptionKeyRequest carries the keywarden's decryption key (v1: the raw
// ElGamal private scalar as 0x big-endian hex). Matches keywarden's request.
type DecryptionKeyRequest struct {
	Key string `json:"key"`
}

// ResultsResponse is the final tally plus the four Solidity-ready PLONK fields.
type ResultsResponse struct {
	ElectionID       string   `json:"electionID"`
	Tally            []uint64 `json:"tally"`
	ProgramVK        string   `json:"programVK"`
	RootCVadcopFinal string   `json:"rootCVadcopFinal"`
	PublicValues     string   `json:"publicValues"`
	ProofBytes       string   `json:"proofBytes"`
}

// WorkerRegisterRequest registers a prover worker into the pool. POST /workers/register
type WorkerRegisterRequest struct {
	Address string `json:"address"` // worker base URL
	Name    string `json:"name,omitempty"`
}

// WorkersResponse lists the prover-worker pool.
type WorkersResponse struct {
	Workers []*workers.WorkerInfo `json:"workers"`
}
