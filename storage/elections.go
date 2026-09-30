package storage

import (
	"time"

	"github.com/vocdoni/davinci-fold/types"
)

// CreateElection persists a new election, failing if one with the same ID
// already exists.
func (s *Storage) CreateElection(e *types.Election) error {
	s.globalLock.Lock()
	defer s.globalLock.Unlock()

	key := electionKey(e.ID)
	var existing types.Election
	if err := s.getArtifact(electionPrefix, key, &existing); err == nil {
		return ErrKeyAlreadyExists
	}
	now := time.Now()
	e.CreatedAt = now
	e.UpdatedAt = now
	return s.setArtifact(electionPrefix, key, e)
}

// Election loads an election by ID.
func (s *Storage) Election(id types.ElectionID) (*types.Election, error) {
	var e types.Election
	if err := s.getArtifact(electionPrefix, electionKey(id), &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// UpdateElection applies updateFunc to the stored election and writes it
// back (bumping UpdatedAt), atomically with respect to the other election
// writes. If updateFunc fails, nothing is written and its error is returned.
func (s *Storage) UpdateElection(id types.ElectionID, updateFunc func(*types.Election) error) error {
	s.globalLock.Lock()
	defer s.globalLock.Unlock()
	var e types.Election
	if err := s.getArtifact(electionPrefix, electionKey(id), &e); err != nil {
		return err
	}
	if err := updateFunc(&e); err != nil {
		return err
	}
	e.UpdatedAt = time.Now()
	return s.setArtifact(electionPrefix, electionKey(id), &e)
}

// SetElectionStatus transitions an election to a new lifecycle status.
func (s *Storage) SetElectionStatus(id types.ElectionID, status types.Status) error {
	return s.UpdateElection(id, func(e *types.Election) error {
		e.Status = status
		return nil
	})
}

// ListElections returns every persisted election.
func (s *Storage) ListElections() ([]*types.Election, error) {
	var out []*types.Election
	if err := s.iterateArtifacts(electionPrefix, func(_, v []byte) bool {
		var e types.Election
		if err := DecodeArtifact(v, &e); err == nil {
			out = append(out, &e)
		}
		return true
	}); err != nil {
		return nil, err
	}
	return out, nil
}
