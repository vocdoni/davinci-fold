package orchestrator

import "errors"

// Errors the engine returns to its callers, wrapped with the detail. Match
// them with errors.Is.
var (
	// ErrElectionNotFound: no election has the given ID.
	ErrElectionNotFound = errors.New("election not found")
	// ErrNotAcceptingVotes: the election is not active.
	ErrNotAcceptingVotes = errors.New("election is not accepting votes")
	// ErrVoteAlreadySubmitted: the vote ID was already accepted, or a vote
	// with the same vote ID or address is being ingested.
	ErrVoteAlreadySubmitted = errors.New("vote already submitted")
	// ErrInvalidBallotProof: the ballot proof or its public inputs do not
	// verify, or do not bind the submitted vote.
	ErrInvalidBallotProof = errors.New("invalid ballot proof")
	// ErrInvalidSignature: the signature is malformed or not the voter's.
	ErrInvalidSignature = errors.New("invalid signature")
	// ErrInvalidCensusProof: the census proof is malformed, does not reach
	// the election's census root, or its leaf is not the voter's.
	ErrInvalidCensusProof = errors.New("invalid census proof")
	// ErrMalformedVote: any other check on the vote failed (address, vote ID
	// or ballot encoding).
	ErrMalformedVote = errors.New("malformed vote")
	// ErrInvalidTransition: the election's status does not allow the
	// requested change.
	ErrInvalidTransition = errors.New("invalid election status transition")
	// ErrInvalidDecryptionKey: the key is not the private key of the
	// election's encryption key.
	ErrInvalidDecryptionKey = errors.New("invalid decryption key")
	// ErrWorkerNotFound: no registered worker has the given ID.
	ErrWorkerNotFound = errors.New("worker not found")
)
