package types

import (
	"encoding/hex"
	"time"
)

// ElectionID identifies an election; it is the protocol ProcessID bytes.
type ElectionID []byte

// String returns the hex encoding of the election ID.
func (e ElectionID) String() string { return hex.EncodeToString(e) }

// Status is the lifecycle state of an election. The values are persisted:
// never renumber them.
type Status uint8

const (
	// StatusCreated is the zero value. Elections are created active, so no
	// election is ever in it.
	StatusCreated Status = iota
	// StatusActive: accepting votes, batches sealing and proving.
	StatusActive
	// StatusEnded: votes closed (end time or organizer), draining the fold
	// chain.
	StatusEnded
	// StatusDecrypting: encrypted results published, awaiting the keywarden's
	// decryption key.
	StatusDecrypting
	// StatusFinalizing: decryption key received, producing the final PLONK.
	StatusFinalizing
	// StatusResults: final tally + PLONK available.
	StatusResults
	// StatusPaused: not accepting votes (organizer); accepted votes keep
	// being sealed and proved.
	StatusPaused
	// StatusCanceled: stopped without results (organizer); nothing is sealed,
	// proved or folded any more.
	StatusCanceled
)

// String returns the lowercase status name used in the API and logs.
func (s Status) String() string {
	switch s {
	case StatusCreated:
		return "created"
	case StatusActive:
		return "active"
	case StatusEnded:
		return "ended"
	case StatusDecrypting:
		return "decrypting"
	case StatusFinalizing:
		return "finalizing"
	case StatusResults:
		return "results"
	case StatusPaused:
		return "paused"
	case StatusCanceled:
		return "canceled"
	default:
		return "unknown"
	}
}

// ElectionConfig is the immutable, CBOR-serializable election configuration.
// ProcessID, BallotMode and CensusRoot are big-endian hex (0x optional)
// big-integer values; EncX/EncY are the ElGamal public key's canonical RTE
// coordinates as big-endian hex. The orchestrator parses these into a
// chain.Config and derives the arbo-LE wire ChainConfig from the live State.
type ElectionConfig struct {
	ProcessID    string `cbor:"processID"`
	BallotMode   string `cbor:"ballotMode"`
	EncX         string `cbor:"encX"`
	EncY         string `cbor:"encY"`
	CensusOrigin uint64 `cbor:"censusOrigin"`
	CensusRoot   string `cbor:"censusRoot"`
	// VK is the snarkjs ballot-proof verification key (JSON), shipped in every
	// batch ProveRequest. Stored once at election creation.
	VK []byte `cbor:"vk,omitempty"`
}

// Election is the persisted election record and lifecycle state.
type Election struct {
	ID         ElectionID     `cbor:"id"`
	Status     Status         `cbor:"status"`
	Config     ElectionConfig `cbor:"config"`
	BatchSize  int            `cbor:"batchSize"`
	FoldEvery  int            `cbor:"foldEvery"`
	EndTime    time.Time      `cbor:"endTime"`
	CreatedAt  time.Time      `cbor:"createdAt"`
	UpdatedAt  time.Time      `cbor:"updatedAt"`
	FoldWorker string         `cbor:"foldWorker,omitempty"` // pinned fold worker URL
	// FinalizeError is a short, fixed reason the last finalize failed, empty
	// otherwise; the detail is in the log. It is cleared when a decryption
	// key is submitted again.
	FinalizeError string `cbor:"finalizeError,omitempty"`
}
