package storage

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/vocdoni/davinci-node/db"
	"github.com/vocdoni/davinci-node/db/metadb"

	"github.com/vocdoni/davinci-fold/types"
)

// onBackends runs test against an in-memory and a Pebble store: they
// iterate prefixes differently.
func onBackends(t *testing.T, test func(c *qt.C, s *Storage)) {
	for _, typ := range []string{db.TypeInMem, db.TypePebble} {
		t.Run(typ, func(t *testing.T) {
			database, err := metadb.New(typ, t.TempDir())
			qt.Assert(t, err, qt.IsNil)
			s := New(database)
			defer s.Close()
			test(qt.New(t), s)
		})
	}
}

// TestSealBatch checks a sealed batch, the snapshot and the votes' batched
// status are stored together, and a vote already past batched keeps its
// status.
func TestSealBatch(t *testing.T) {
	onBackends(t, func(c *qt.C, s *Storage) {
		e := sampleElection()
		c.Assert(s.CreateElection(e), qt.IsNil)
		v1 := &types.Vote{ID: types.VoteID("vote-1")}
		v2 := &types.Vote{ID: types.VoteID("vote-2")}
		c.Assert(s.AddVote(e.ID, v1), qt.IsNil)
		c.Assert(s.AddVote(e.ID, v2), qt.IsNil)
		c.Assert(s.SetVoteStatus(e.ID, v2.ID, types.VoteStatusError), qt.IsNil)

		c.Assert(s.SealBatch(&types.BatchInput{
			ElectionID: e.ID, Seq: 0, NewStateRoot: "0x01", VoteIDs: []types.VoteID{v1.ID, v2.ID},
		}, []byte("state")), qt.IsNil)
		bi, err := s.BatchInput(e.ID, 0)
		c.Assert(err, qt.IsNil)
		c.Assert(bi.NewStateRoot, qt.Equals, "0x01")
		snap, err := s.Snapshot(e.ID)
		c.Assert(err, qt.IsNil)
		c.Assert(string(snap), qt.Equals, "state")
		st, _ := s.VoteStatus(e.ID, v1.ID)
		c.Assert(st, qt.Equals, types.VoteStatusBatched)
		st, _ = s.VoteStatus(e.ID, v2.ID)
		c.Assert(st, qt.Equals, types.VoteStatusError)
	})
}

// TestVotesWithStatus checks votes are found by status, and that AddVote
// stores each with pending status.
func TestVotesWithStatus(t *testing.T) {
	onBackends(t, func(c *qt.C, s *Storage) {
		e := sampleElection()
		other := sampleElection()
		other.ID = types.ElectionID{0x01, 0x02, 0x03, 0x04}
		c.Assert(s.CreateElection(e), qt.IsNil)
		c.Assert(s.CreateElection(other), qt.IsNil)
		for _, id := range []string{"a", "b", "c"} {
			c.Assert(s.AddVote(e.ID, &types.Vote{ID: types.VoteID(id)}), qt.IsNil)
		}
		c.Assert(s.AddVote(other.ID, &types.Vote{ID: types.VoteID("d")}), qt.IsNil)
		c.Assert(s.SetVoteStatus(e.ID, types.VoteID("b"), types.VoteStatusBatched), qt.IsNil)

		pending, err := s.VotesWithStatus(e.ID, types.VoteStatusPending)
		c.Assert(err, qt.IsNil)
		c.Assert(len(pending), qt.Equals, 2)
		c.Assert(string(pending[0].ID), qt.Equals, "a")
		c.Assert(pending[0].Seq, qt.Equals, uint64(1))
		c.Assert(string(pending[1].ID), qt.Equals, "c")
		c.Assert(pending[1].Seq, qt.Equals, uint64(3))
	})
}

// TestCommitFoldAndProofs checks a fold commit stores the checkpoint and the
// fold proof and drops the proofs of the folded batches, that a batch reset
// drops its proof, and that the proofs of an election are dropped together.
func TestCommitFoldAndProofs(t *testing.T) {
	onBackends(t, func(c *qt.C, s *Storage) {
		e := sampleElection()
		other := types.ElectionID{0x01, 0x02, 0x03, 0x04}
		c.Assert(s.CreateElection(e), qt.IsNil)
		_, err := s.FoldProof(e.ID)
		c.Assert(err, qt.ErrorIs, ErrNotFound)
		for seq := range uint64(3) {
			c.Assert(s.SetBatchProof(e.ID, seq, []byte{byte(seq)}), qt.IsNil)
		}
		c.Assert(s.SetBatchProof(other, 0, []byte("other")), qt.IsNil)

		c.Assert(s.CommitFold(&types.FoldCheckpoint{
			ElectionID: e.ID, FoldCount: 1, LastFoldJob: "fold-1", FoldWorker: "http://w", BatchesFolded: 2,
		}, []byte("fold"), []uint64{0, 1}), qt.IsNil)
		c.Assert(s.CommitFold(&types.FoldCheckpoint{ElectionID: e.ID}, nil, nil), qt.ErrorMatches, "empty fold proof")
		cp, err := s.FoldCheckpoint(e.ID)
		c.Assert(err, qt.IsNil)
		c.Assert(cp.LastFoldJob, qt.Equals, "fold-1")
		c.Assert(cp.FoldWorker, qt.Equals, "http://w")
		proof, err := s.FoldProof(e.ID)
		c.Assert(err, qt.IsNil)
		c.Assert(string(proof), qt.Equals, "fold")
		for seq := range uint64(2) {
			_, err := s.BatchProof(e.ID, seq)
			c.Assert(err, qt.ErrorIs, ErrNotFound)
		}
		proof, err = s.BatchProof(e.ID, 2)
		c.Assert(err, qt.IsNil)
		c.Assert(proof, qt.DeepEquals, []byte{2})

		c.Assert(s.ResetBatch(&types.BatchInput{ElectionID: e.ID, Seq: 2, NewStateRoot: "0x02"}), qt.IsNil)
		_, err = s.BatchProof(e.ID, 2)
		c.Assert(err, qt.ErrorIs, ErrNotFound)
		bi, err := s.BatchInput(e.ID, 2)
		c.Assert(err, qt.IsNil)
		c.Assert(bi.NewStateRoot, qt.Equals, "0x02")
		c.Assert(s.SetBatchProof(e.ID, 2, []byte{2}), qt.IsNil)

		c.Assert(s.DeleteProofs(e.ID), qt.IsNil)
		_, err = s.FoldProof(e.ID)
		c.Assert(err, qt.ErrorIs, ErrNotFound)
		_, err = s.BatchProof(e.ID, 2)
		c.Assert(err, qt.ErrorIs, ErrNotFound)
		proof, err = s.BatchProof(other, 0)
		c.Assert(err, qt.IsNil)
		c.Assert(string(proof), qt.Equals, "other")
	})
}

// TestWorkerRegistrations checks registrations are stored, replaced by
// address and deleted.
func TestWorkerRegistrations(t *testing.T) {
	onBackends(t, func(c *qt.C, s *Storage) {
		c.Assert(s.SetWorker(&types.WorkerRegistration{Address: "http://a", Name: "a"}), qt.IsNil)
		c.Assert(s.SetWorker(&types.WorkerRegistration{Address: "http://b"}), qt.IsNil)
		c.Assert(s.SetWorker(&types.WorkerRegistration{Address: "http://b", Name: "b"}), qt.IsNil)
		regs, err := s.ListWorkers()
		c.Assert(err, qt.IsNil)
		c.Assert(len(regs), qt.Equals, 2)
		c.Assert(regs[1].Name, qt.Equals, "b")
		c.Assert(regs[1].RegisteredAt.IsZero(), qt.IsFalse)

		c.Assert(s.DeleteWorker("http://a"), qt.IsNil)
		regs, err = s.ListWorkers()
		c.Assert(err, qt.IsNil)
		c.Assert(len(regs), qt.Equals, 1)
		c.Assert(regs[0].Address, qt.Equals, "http://b")
	})
}
