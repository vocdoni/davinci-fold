package storage

import (
	"time"

	"github.com/vocdoni/davinci-fold/types"
)

// SetBatchInput persists (or overwrites) a sealed batch's re-drivable input.
func (s *Storage) SetBatchInput(b *types.BatchInput) error {
	if b.SealedAt.IsZero() {
		b.SealedAt = time.Now()
	}
	return s.setArtifact(batchPrefix, subKey(b.ElectionID, seqBytes(b.Seq)), b)
}

// BatchInput loads a batch input by election and sequence.
func (s *Storage) BatchInput(id types.ElectionID, seq uint64) (*types.BatchInput, error) {
	var b types.BatchInput
	if err := s.getArtifact(batchPrefix, subKey(id, seqBytes(seq)), &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// ListBatchInputs returns an election's batch inputs ordered by sequence.
func (s *Storage) ListBatchInputs(id types.ElectionID) ([]*types.BatchInput, error) {
	var out []*types.BatchInput
	prefix := append(append([]byte{}, batchPrefix...), electionScanPrefix(id)...)
	if err := s.iterateArtifacts(prefix, func(_, v []byte) bool {
		var b types.BatchInput
		if err := DecodeArtifact(v, &b); err == nil {
			out = append(out, &b)
		}
		return true
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// ReserveBatch reserves a batch for dispatch, returning ErrKeyAlreadyExists if
// another worker already holds it. Pair with ReleaseBatch on completion.
func (s *Storage) ReserveBatch(id types.ElectionID, seq uint64) error {
	return s.setReservation(batchPrefix, subKey(id, seqBytes(seq)))
}

// ReleaseBatch frees a batch reservation.
func (s *Storage) ReleaseBatch(id types.ElectionID, seq uint64) error {
	return s.deleteReservation(batchPrefix, subKey(id, seqBytes(seq)))
}

// IsBatchReserved reports whether a batch is currently reserved.
func (s *Storage) IsBatchReserved(id types.ElectionID, seq uint64) bool {
	return s.isReserved(batchPrefix, subKey(id, seqBytes(seq)))
}

// SetFoldCheckpoint persists the head of an election's fold chain.
func (s *Storage) SetFoldCheckpoint(c *types.FoldCheckpoint) error {
	c.UpdatedAt = time.Now()
	return s.setArtifact(foldPrefix, electionKey(c.ElectionID), c)
}

// FoldCheckpoint loads an election's fold checkpoint.
func (s *Storage) FoldCheckpoint(id types.ElectionID) (*types.FoldCheckpoint, error) {
	var c types.FoldCheckpoint
	if err := s.getArtifact(foldPrefix, electionKey(id), &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// SetSnapshot persists the latest chain.State snapshot blob for an election.
func (s *Storage) SetSnapshot(id types.ElectionID, blob []byte) error {
	return s.setArtifact(snapshotPrefix, electionKey(id), &struct {
		Blob []byte `cbor:"blob"`
	}{Blob: blob})
}

// Snapshot loads the latest state snapshot blob for an election.
func (s *Storage) Snapshot(id types.ElectionID) ([]byte, error) {
	var raw struct {
		Blob []byte `cbor:"blob"`
	}
	if err := s.getArtifact(snapshotPrefix, electionKey(id), &raw); err != nil {
		return nil, err
	}
	return raw.Blob, nil
}
