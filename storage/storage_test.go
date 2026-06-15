package storage

import (
	"math/big"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/vocdoni/davinci-node/db"
	"github.com/vocdoni/davinci-node/db/metadb"

	"github.com/vocdoni/davinci-fold/types"
)

func newTestStorage(t *testing.T) *Storage {
	t.Helper()
	database, err := metadb.New(db.TypeInMem, "")
	qt.Assert(t, err, qt.IsNil)
	return New(database)
}

func sampleElection() *types.Election {
	return &types.Election{
		ID:        types.ElectionID{0x01, 0x02, 0x03},
		Status:    types.StatusCreated,
		BatchSize: 64,
		FoldEvery: 4,
		EndTime:   time.Now().Add(time.Hour),
		Config: types.ElectionConfig{
			ProcessID:    "0a0b",
			BallotMode:   "01",
			CensusOrigin: 1,
			CensusRoot:   "1234",
		},
	}
}

func TestElectionCRUD(t *testing.T) {
	c := qt.New(t)
	s := newTestStorage(t)
	defer s.Close()

	e := sampleElection()
	c.Assert(s.CreateElection(e), qt.IsNil)
	// Duplicate create rejected.
	c.Assert(s.CreateElection(e), qt.Equals, ErrKeyAlreadyExists)

	got, err := s.Election(e.ID)
	c.Assert(err, qt.IsNil)
	c.Assert(got.Status, qt.Equals, types.StatusCreated)
	c.Assert(got.BatchSize, qt.Equals, 64)

	c.Assert(s.SetElectionStatus(e.ID, types.StatusActive), qt.IsNil)
	got, err = s.Election(e.ID)
	c.Assert(err, qt.IsNil)
	c.Assert(got.Status, qt.Equals, types.StatusActive)

	all, err := s.ListElections()
	c.Assert(err, qt.IsNil)
	c.Assert(len(all), qt.Equals, 1)

	_, err = s.Election(types.ElectionID{0xff})
	c.Assert(err, qt.Equals, ErrNotFound)
}

func TestVoteLogAndDedup(t *testing.T) {
	c := qt.New(t)
	s := newTestStorage(t)
	defer s.Close()

	e := sampleElection()
	c.Assert(s.CreateElection(e), qt.IsNil)

	v1 := &types.Vote{ID: types.VoteID("vote-1"), CensusIdx: 0}
	v2 := &types.Vote{ID: types.VoteID("vote-2"), CensusIdx: 1}
	c.Assert(s.AddVote(e.ID, v1), qt.IsNil)
	c.Assert(s.AddVote(e.ID, v2), qt.IsNil)
	c.Assert(v1.Seq, qt.Equals, uint64(1))
	c.Assert(v2.Seq, qt.Equals, uint64(2))

	c.Assert(s.VoteExists(e.ID, types.VoteID("vote-1")), qt.IsTrue)
	c.Assert(s.VoteExists(e.ID, types.VoteID("nope")), qt.IsFalse)

	votes, err := s.ListVotes(e.ID)
	c.Assert(err, qt.IsNil)
	c.Assert(len(votes), qt.Equals, 2)

	st, err := s.VoteStatus(e.ID, v1.ID)
	c.Assert(err, qt.IsNil)
	c.Assert(st, qt.Equals, types.VoteStatusPending)
	c.Assert(s.SetVoteStatus(e.ID, v1.ID, types.VoteStatusBatched), qt.IsNil)
	st, _ = s.VoteStatus(e.ID, v1.ID)
	c.Assert(st, qt.Equals, types.VoteStatusBatched)

	// Address lock is exclusive until released.
	addr := big.NewInt(0xdead)
	c.Assert(s.LockAddress(e.ID, addr), qt.IsTrue)
	c.Assert(s.LockAddress(e.ID, addr), qt.IsFalse)
	s.ReleaseAddress(e.ID, addr)
	c.Assert(s.LockAddress(e.ID, addr), qt.IsTrue)

	// voteID in-flight tracking.
	c.Assert(s.IsVoteIDProcessing(v1.ID), qt.IsFalse)
	s.LockVoteID(v1.ID)
	c.Assert(s.IsVoteIDProcessing(v1.ID), qt.IsTrue)
	s.ReleaseVoteID(v1.ID)
	c.Assert(s.IsVoteIDProcessing(v1.ID), qt.IsFalse)
}

func TestBatchReservationAndRecovery(t *testing.T) {
	c := qt.New(t)
	database, err := metadb.New(db.TypeInMem, "")
	c.Assert(err, qt.IsNil)

	s := New(database)
	e := sampleElection()
	c.Assert(s.CreateElection(e), qt.IsNil)

	b := &types.BatchInput{ElectionID: e.ID, Seq: 0, NewStateRoot: "0xabcd"}
	c.Assert(s.SetBatchInput(b), qt.IsNil)
	got, err := s.BatchInput(e.ID, 0)
	c.Assert(err, qt.IsNil)
	c.Assert(got.NewStateRoot, qt.Equals, "0xabcd")

	// Reserve, double-reserve fails.
	c.Assert(s.ReserveBatch(e.ID, 0), qt.IsNil)
	c.Assert(s.IsBatchReserved(e.ID, 0), qt.IsTrue)
	c.Assert(s.ReserveBatch(e.ID, 0), qt.Equals, ErrKeyAlreadyExists)

	// Simulate a crash: a new Storage over the same db must clear the stale
	// reservation on startup so the batch is re-dispatchable.
	s.cancel() // stop the monitor without closing the db
	s2 := New(database)
	defer s2.Close()
	c.Assert(s2.IsBatchReserved(e.ID, 0), qt.IsFalse)
	// Batch input survived the restart.
	got, err = s2.BatchInput(e.ID, 0)
	c.Assert(err, qt.IsNil)
	c.Assert(got.NewStateRoot, qt.Equals, "0xabcd")
}

func TestFoldCheckpointSnapshotResultsAudit(t *testing.T) {
	c := qt.New(t)
	s := newTestStorage(t)
	defer s.Close()
	e := sampleElection()
	c.Assert(s.CreateElection(e), qt.IsNil)

	cp := &types.FoldCheckpoint{ElectionID: e.ID, FoldCount: 3, StateRoot: "0x99", LastFoldJob: "job-9"}
	c.Assert(s.SetFoldCheckpoint(cp), qt.IsNil)
	gotCP, err := s.FoldCheckpoint(e.ID)
	c.Assert(err, qt.IsNil)
	c.Assert(gotCP.FoldCount, qt.Equals, uint64(3))
	c.Assert(gotCP.LastFoldJob, qt.Equals, "job-9")

	blob := []byte("snapshot-bytes")
	c.Assert(s.SetSnapshot(e.ID, blob), qt.IsNil)
	gotBlob, err := s.Snapshot(e.ID)
	c.Assert(err, qt.IsNil)
	c.Assert(gotBlob, qt.DeepEquals, blob)

	res := &types.Results{ElectionID: e.ID, Tally: []uint64{10, 20}, ProgramVK: "0xvk"}
	c.Assert(s.SetResults(res), qt.IsNil)
	gotRes, err := s.Results(e.ID)
	c.Assert(err, qt.IsNil)
	c.Assert(gotRes.Tally, qt.DeepEquals, []uint64{10, 20})

	c.Assert(s.AppendAudit(&types.AuditRecord{Subject: "admin", Action: "create_election", ElectionID: e.ID}), qt.IsNil)
	c.Assert(s.AppendAudit(&types.AuditRecord{Subject: "kw", Action: "decryption_key", ElectionID: e.ID}), qt.IsNil)
	audit, err := s.ListAudit()
	c.Assert(err, qt.IsNil)
	c.Assert(len(audit), qt.Equals, 2)
	c.Assert(audit[0].Action, qt.Equals, "create_election")
}
