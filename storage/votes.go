package storage

import (
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/vocdoni/davinci-node/db/prefixeddb"

	"github.com/vocdoni/davinci-fold/types"
)

// voteSeqRecord is the stored per-election vote sequence counter.
type voteSeqRecord struct {
	Seq uint64 `cbor:"seq"`
}

// AddVote appends a vote to the election's ordered log with pending status,
// in one write with the vote sequence counter.
func (s *Storage) AddVote(id types.ElectionID, v *types.Vote) error {
	s.globalLock.Lock()
	defer s.globalLock.Unlock()

	var counter voteSeqRecord
	if err := s.getArtifact(voteSeqPrefix, electionKey(id), &counter); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	counter.Seq++
	v.Seq = counter.Seq
	v.SubmittedAt = time.Now()

	seq, err := EncodeArtifact(&counter)
	if err != nil {
		return err
	}
	vote, err := EncodeArtifact(v)
	if err != nil {
		return err
	}
	status, err := EncodeArtifact(&voteStatusRecord{Status: types.VoteStatusPending})
	if err != nil {
		return err
	}
	tx := s.db.WriteTx()
	defer tx.Discard()
	if err := prefixeddb.NewPrefixedWriteTx(tx, voteSeqPrefix).Set(electionKey(id), seq); err != nil {
		return err
	}
	if err := prefixeddb.NewPrefixedWriteTx(tx, votePrefix).Set(subKey(id, v.ID), vote); err != nil {
		return err
	}
	if err := prefixeddb.NewPrefixedWriteTx(tx, voteStatusPrefix).Set(subKey(id, v.ID), status); err != nil {
		return err
	}
	return tx.Commit()
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

// ListVotes returns every vote of an election, ordered by vote ID (Seq is
// the submission order).
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

// voteStatusRecord is the stored status of a vote.
type voteStatusRecord struct {
	Status types.VoteStatus `cbor:"status"`
}

func (s *Storage) setVoteStatusLocked(id types.ElectionID, voteID types.VoteID, st types.VoteStatus) error {
	return s.setArtifact(voteStatusPrefix, subKey(id, voteID), &voteStatusRecord{Status: st})
}

// VotesWithStatus returns the votes of an election in status st, ordered by
// vote ID. It reads only the status records and the votes it returns.
func (s *Storage) VotesWithStatus(id types.ElectionID, st types.VoteStatus) ([]*types.Vote, error) {
	var ids []types.VoteID
	prefix := append(append([]byte{}, voteStatusPrefix...), electionScanPrefix(id)...)
	if err := s.iterateArtifacts(prefix, func(k, v []byte) bool {
		var raw voteStatusRecord
		if err := DecodeArtifact(v, &raw); err == nil && raw.Status == st {
			ids = append(ids, types.VoteID(append([]byte(nil), k...)))
		}
		return true
	}); err != nil {
		return nil, err
	}
	votes := make([]*types.Vote, 0, len(ids))
	for _, voteID := range ids {
		v, err := s.Vote(id, voteID)
		if err != nil {
			return nil, fmt.Errorf("vote %s: %w", voteID, err)
		}
		votes = append(votes, v)
	}
	return votes, nil
}

// SetVoteStatus moves a vote to status st, unless it is already there or
// past it (see types.VoteStatus), which leaves it unchanged.
func (s *Storage) SetVoteStatus(id types.ElectionID, voteID types.VoteID, st types.VoteStatus) error {
	s.globalLock.Lock()
	defer s.globalLock.Unlock()
	if cur, err := s.VoteStatus(id, voteID); err == nil && !cur.CanMoveTo(st) {
		return nil
	}
	return s.setVoteStatusLocked(id, voteID, st)
}

// VoteStatus returns a vote's current status.
func (s *Storage) VoteStatus(id types.ElectionID, voteID types.VoteID) (types.VoteStatus, error) {
	var raw voteStatusRecord
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
