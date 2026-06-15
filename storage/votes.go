package storage

import (
	"math/big"
	"time"

	"github.com/vocdoni/davinci-fold/types"
)

// nextVoteSeq atomically increments and returns the per-election vote
// sequence counter. Caller must hold globalLock.
func (s *Storage) nextVoteSeq(id types.ElectionID) (uint64, error) {
	key := electionKey(id)
	var seq uint64
	var raw struct {
		Seq uint64 `cbor:"seq"`
	}
	if err := s.getArtifact(voteSeqPrefix, key, &raw); err == nil {
		seq = raw.Seq
	}
	seq++
	raw.Seq = seq
	if err := s.setArtifact(voteSeqPrefix, key, &raw); err != nil {
		return 0, err
	}
	return seq, nil
}

// AddVote appends a vote to the election's ordered log with pending status.
func (s *Storage) AddVote(id types.ElectionID, v *types.Vote) error {
	s.globalLock.Lock()
	defer s.globalLock.Unlock()

	seq, err := s.nextVoteSeq(id)
	if err != nil {
		return err
	}
	v.Seq = seq
	v.SubmittedAt = time.Now()

	if err := s.setArtifact(votePrefix, subKey(id, v.ID), v); err != nil {
		return err
	}
	return s.setVoteStatusLocked(id, v.ID, types.VoteStatusPending)
}

// Vote loads one vote of an election.
func (s *Storage) Vote(id types.ElectionID, voteID types.VoteID) (*types.Vote, error) {
	var v types.Vote
	if err := s.getArtifact(votePrefix, subKey(id, voteID), &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// VoteExists reports whether a vote with voteID was already submitted.
func (s *Storage) VoteExists(id types.ElectionID, voteID types.VoteID) bool {
	_, err := s.Vote(id, voteID)
	return err == nil
}

// ListVotes returns every vote of an election ordered by submission sequence.
func (s *Storage) ListVotes(id types.ElectionID) ([]*types.Vote, error) {
	var out []*types.Vote
	prefix := append(append([]byte{}, votePrefix...), electionScanPrefix(id)...)
	if err := s.iterateArtifacts(prefix, func(_, v []byte) bool {
		var vote types.Vote
		if err := DecodeArtifact(v, &vote); err == nil {
			out = append(out, &vote)
		}
		return true
	}); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Storage) setVoteStatusLocked(id types.ElectionID, voteID types.VoteID, st types.VoteStatus) error {
	return s.setArtifact(voteStatusPrefix, subKey(id, voteID), &struct {
		Status types.VoteStatus `cbor:"status"`
	}{Status: st})
}

// SetVoteStatus updates a vote's pipeline status.
func (s *Storage) SetVoteStatus(id types.ElectionID, voteID types.VoteID, st types.VoteStatus) error {
	s.globalLock.Lock()
	defer s.globalLock.Unlock()
	return s.setVoteStatusLocked(id, voteID, st)
}

// VoteStatus returns a vote's current status.
func (s *Storage) VoteStatus(id types.ElectionID, voteID types.VoteID) (types.VoteStatus, error) {
	var raw struct {
		Status types.VoteStatus `cbor:"status"`
	}
	if err := s.getArtifact(voteStatusPrefix, subKey(id, voteID), &raw); err != nil {
		return 0, err
	}
	return raw.Status, nil
}

// --- in-memory dedup locks (per-(election,address) and per-voteID) ---

// LockAddress tries to acquire the in-flight lock for a voter address scoped
// to an election. Returns true if acquired, false if already held.
func (s *Storage) LockAddress(id types.ElectionID, address *big.Int) bool {
	key := id.String() + ":" + address.String()
	_, loaded := s.processingAddresses.LoadOrStore(key, struct{}{})
	return !loaded
}

// ReleaseAddress releases a voter address lock.
func (s *Storage) ReleaseAddress(id types.ElectionID, address *big.Int) {
	s.processingAddresses.Delete(id.String() + ":" + address.String())
}

// LockVoteID atomically marks a voteID as in-flight. It returns true if the
// lock was acquired, false if the voteID was already being processed. The
// atomic acquire-or-fail closes the check-then-lock race between two concurrent
// submissions of the same voteID.
func (s *Storage) LockVoteID(voteID types.VoteID) bool {
	_, loaded := s.processingVoteIDs.LoadOrStore(string(voteID), struct{}{})
	return !loaded
}

// IsVoteIDProcessing reports whether a voteID is currently in-flight.
func (s *Storage) IsVoteIDProcessing(voteID types.VoteID) bool {
	_, ok := s.processingVoteIDs.Load(string(voteID))
	return ok
}

// ReleaseVoteID clears a voteID's in-flight mark.
func (s *Storage) ReleaseVoteID(voteID types.VoteID) {
	s.processingVoteIDs.Delete(string(voteID))
}
