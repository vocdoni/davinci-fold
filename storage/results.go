package storage

import (
	"time"

	"github.com/vocdoni/davinci-fold/types"
)

// SetResults persists an election's final tally and PLONK.
func (s *Storage) SetResults(r *types.Results) error {
	if r.FinalizedAt.IsZero() {
		r.FinalizedAt = time.Now()
	}
	return s.setArtifact(resultsPrefix, electionKey(r.ElectionID), r)
}

// Results loads an election's final results, ErrNotFound if not finalized.
func (s *Storage) Results(id types.ElectionID) (*types.Results, error) {
	var r types.Results
	if err := s.getArtifact(resultsPrefix, electionKey(id), &r); err != nil {
		return nil, err
	}
	return &r, nil
}
