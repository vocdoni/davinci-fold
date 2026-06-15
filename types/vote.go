package types

import (
	"encoding/hex"
	"time"
)

// VoteID is the unique identifier of a submitted vote.
type VoteID []byte

// String returns the hex encoding of the vote ID.
func (v VoteID) String() string { return hex.EncodeToString(v) }

// VoteStatus tracks a vote through the ingest → batch → fold pipeline.
type VoteStatus uint8

const (
	// VoteStatusPending: accepted, not yet sealed into a batch.
	VoteStatusPending VoteStatus = iota
	// VoteStatusBatched: assigned to a sealed batch being proved.
	VoteStatusBatched
	// VoteStatusFolded: its batch STARK has been folded into the chain.
	VoteStatusFolded
	// VoteStatusSettled: included in a finalized election.
	VoteStatusSettled
	// VoteStatusError: failed validation or proving.
	VoteStatusError
)

// String returns the lowercase status name used in the API and logs.
func (s VoteStatus) String() string {
	switch s {
	case VoteStatusPending:
		return "pending"
	case VoteStatusBatched:
		return "batched"
	case VoteStatusFolded:
		return "folded"
	case VoteStatusSettled:
		return "settled"
	case VoteStatusError:
		return "error"
	default:
		return "unknown"
	}
}

// Vote is an ingested ballot in the ordered vote log. The heavy proof and
// census material needed only for batch proving travels in Payload (the raw
// ballot-proof request body); the orchestrator keeps the light key parts for
// dedup and state application.
type Vote struct {
	ID          VoteID `cbor:"id"`
	Address     []byte `cbor:"address"`     // voter Ethereum address (20 bytes)
	CensusIdx   int    `cbor:"censusIdx"`   // voter census index
	AddressLo16 uint64 `cbor:"addressLo16"` // low 16 bits of address (ballot key)
	// VoteIDKey is the numeric vote-ID state-tree key (bit 63 set), passed to
	// chain.Vote at batch application.
	VoteIDKey uint64 `cbor:"voteIDKey"`
	// Ballot is the voter-encrypted ElGamal ballot, serialized via
	// elgamal.Ballot.Serialize.
	Ballot []byte `cbor:"ballot"`
	// Payload is the raw self-authenticating submission (ballot proof + ECDSA
	// signature + census proof) retained for batch proving and audit.
	Payload     []byte    `cbor:"payload,omitempty"`
	Seq         uint64    `cbor:"seq"` // monotonic position in the vote log
	SubmittedAt time.Time `cbor:"submittedAt"`
}
