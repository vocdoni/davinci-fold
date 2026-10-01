package storage

import (
	"errors"
	"time"

	"github.com/vocdoni/davinci-node/db/prefixeddb"

	"github.com/vocdoni/davinci-fold/types"
)

// An election's fold chain lives in its fold worker's job store. To move the
// chain to another worker, the orchestrator keeps the raw proof.bin of the
// last fold and of every proved batch not folded yet.

// batchProofKey is the key of a batch's proof.bin.
func batchProofKey(id types.ElectionID, seq uint64) []byte {
	return subKey(id, append([]byte("b"), seqBytes(seq)...))
}

// foldProofKey is the key of the last fold's proof.bin.
func foldProofKey(id types.ElectionID) []byte {
	return subKey(id, []byte("f"))
}

// SetBatchProof stores the proof.bin of a proved batch until it is folded.
func (s *Storage) SetBatchProof(id types.ElectionID, seq uint64, proof []byte) error {
	return s.setArtifact(proofPrefix, batchProofKey(id, seq), &blobRecord{Blob: proof})
}

// DeleteBatchProof drops a batch's stored proof.bin.
func (s *Storage) DeleteBatchProof(id types.ElectionID, seq uint64) error {
	return s.deleteArtifact(proofPrefix, batchProofKey(id, seq))
}

// BatchProof returns a batch's stored proof.bin, ErrNotFound if the batch is
// not proved yet or already folded.
func (s *Storage) BatchProof(id types.ElectionID, seq uint64) ([]byte, error) {
	var raw blobRecord
	if err := s.getArtifact(proofPrefix, batchProofKey(id, seq), &raw); err != nil {
		return nil, err
	}
	return raw.Blob, nil
}

// ResetBatch stores b and drops its proof.bin in one write, so the batch is
// proved again.
func (s *Storage) ResetBatch(b *types.BatchInput) error {
	batch, err := EncodeArtifact(b)
	if err != nil {
		return err
	}
	tx := s.db.WriteTx()
	defer tx.Discard()
	if err := prefixeddb.NewPrefixedWriteTx(tx, batchPrefix).Set(subKey(b.ElectionID, seqBytes(b.Seq)), batch); err != nil {
		return err
	}
	if err := prefixeddb.NewPrefixedWriteTx(tx, proofPrefix).Delete(batchProofKey(b.ElectionID, b.Seq)); err != nil {
		return err
	}
	return tx.Commit()
}

// FoldProof returns the proof.bin of an election's last fold, ErrNotFound
// before the first one.
func (s *Storage) FoldProof(id types.ElectionID) ([]byte, error) {
	var raw blobRecord
	if err := s.getArtifact(proofPrefix, foldProofKey(id), &raw); err != nil {
		return nil, err
	}
	return raw.Blob, nil
}

// CommitFold persists a completed fold in one write: its checkpoint, its
// proof.bin in place of the previous fold's, and the removal of the proofs
// of the batches it folded, which the chain no longer needs.
func (s *Storage) CommitFold(c *types.FoldCheckpoint, proof []byte, folded []uint64) error {
	if len(proof) == 0 {
		return errors.New("empty fold proof")
	}
	c.UpdatedAt = time.Now()
	cp, err := EncodeArtifact(c)
	if err != nil {
		return err
	}
	blob, err := EncodeArtifact(&blobRecord{Blob: proof})
	if err != nil {
		return err
	}
	tx := s.db.WriteTx()
	defer tx.Discard()
	if err := prefixeddb.NewPrefixedWriteTx(tx, foldPrefix).Set(electionKey(c.ElectionID), cp); err != nil {
		return err
	}
	proofs := prefixeddb.NewPrefixedWriteTx(tx, proofPrefix)
	if err := proofs.Set(foldProofKey(c.ElectionID), blob); err != nil {
		return err
	}
	for _, seq := range folded {
		if err := proofs.Delete(batchProofKey(c.ElectionID, seq)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteProofs drops every stored proof of an election, once its chain is
// finished.
func (s *Storage) DeleteProofs(id types.ElectionID) error {
	prefix := append(append([]byte{}, proofPrefix...), electionScanPrefix(id)...)
	tx := prefixeddb.NewPrefixedDatabase(s.db, prefix).WriteTx()
	defer tx.Discard()
	var keys [][]byte
	if err := tx.Iterate(nil, func(k, _ []byte) bool {
		keys = append(keys, append([]byte(nil), k...))
		return true
	}); err != nil {
		return err
	}
	for _, k := range keys {
		if err := tx.Delete(k); err != nil {
			return err
		}
	}
	return tx.Commit()
}
