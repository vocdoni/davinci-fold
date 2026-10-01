package orchestrator

import (
	"errors"
	"slices"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/vocdoni/davinci-node/db"
	"github.com/vocdoni/davinci-node/db/metadb"

	"github.com/vocdoni/davinci-fold/storage"
	"github.com/vocdoni/davinci-fold/types"
	"github.com/vocdoni/davinci-fold/workers"
)

// TestRestoreReloadsPendingVotes stops an engine with accepted votes no
// batch holds: the next engine has them pending in submission order, still
// refuses them as duplicates, and seals them.
func TestRestoreReloadsPendingVotes(t *testing.T) {
	c := qt.New(t)
	database, err := metadb.New(db.TypeInMem, "")
	c.Assert(err, qt.IsNil)
	s := storage.New(database)
	opts := Options{BatchSize: 3, BatchTimeWindow: time.Hour, Validator: structuralValidator{}}
	e1, err := NewEngine(s, opts)
	c.Assert(err, qt.IsNil)
	el, encKey := testElection(t, 0x90, time.Now().Add(time.Hour))
	el.BatchSize = 3
	c.Assert(e1.CreateElection("admin", el), qt.IsNil)
	// Votes 0 to 2 fill a batch; 4 then 3 stay pending.
	for _, i := range []int{0, 1, 2, 4, 3} {
		_, err := e1.SubmitVote(el.ID, makeSub(t, encKey, i))
		c.Assert(err, qt.IsNil)
	}
	e1.Stop()

	e2, err := NewEngine(s, opts)
	c.Assert(err, qt.IsNil)
	defer e2.Stop()
	rt, ok := e2.runtime(el.ID)
	c.Assert(ok, qt.IsTrue)
	rt.mu.Lock()
	pending := slices.Clone(rt.pending)
	rt.mu.Unlock()
	c.Assert(len(pending), qt.Equals, 2)
	c.Assert(pending[0].ID, qt.DeepEquals, types.VoteID(testVoteIDBytes(0xa4)))
	c.Assert(pending[1].ID, qt.DeepEquals, types.VoteID(testVoteIDBytes(0xa3)))

	_, err = e2.SubmitVote(el.ID, makeSub(t, encKey, 3))
	c.Assert(err, qt.ErrorIs, ErrVoteAlreadySubmitted)
	c.Assert(e2.SetStatus("admin", el.ID, types.StatusEnded), qt.IsNil)
	bi, err := s.BatchInput(el.ID, 1)
	c.Assert(err, qt.IsNil)
	c.Assert(bi.VoteIDs, qt.DeepEquals, []types.VoteID{pending[0].ID, pending[1].ID})
	c.Assert(voteStatus(t, s, el.ID, 0xa3), qt.Equals, types.VoteStatusBatched)
}

// TestWorkerRegistrationRestored checks a registered worker is stored and
// added to the pool of the next engine, and a removed one is deleted.
func TestWorkerRegistrationRestored(t *testing.T) {
	c := qt.New(t)
	database, err := metadb.New(db.TypeInMem, "")
	c.Assert(err, qt.IsNil)
	s := storage.New(database)
	e1, err := NewEngine(s, Options{Validator: structuralValidator{}, Pool: workers.NewWorkerManager(nil)})
	c.Assert(err, qt.IsNil)
	w, err := e1.RegisterWorker("ops", "http://10.0.0.5:8080", "gpu-0")
	c.Assert(err, qt.IsNil)
	_, err = e1.RegisterWorker("ops", "http://10.0.0.6:8080", "")
	c.Assert(err, qt.IsNil)
	c.Assert(e1.RemoveWorker("ops", "nope"), qt.ErrorIs, ErrWorkerNotFound)
	e1.Stop()

	pool := workers.NewWorkerManager(nil)
	e2, err := NewEngine(s, Options{Validator: structuralValidator{}, Pool: pool})
	c.Assert(err, qt.IsNil)
	defer e2.Stop()
	got, ok := pool.GetWorker("http://10.0.0.5:8080")
	c.Assert(ok, qt.IsTrue)
	c.Assert(got.Name, qt.Equals, "gpu-0")
	_, ok = pool.GetWorker("http://10.0.0.6:8080")
	c.Assert(ok, qt.IsTrue)

	c.Assert(e2.RemoveWorker("ops", w.ID), qt.IsNil)
	_, ok = pool.GetWorker("http://10.0.0.5:8080")
	c.Assert(ok, qt.IsFalse)
	regs, err := s.ListWorkers()
	c.Assert(err, qt.IsNil)
	c.Assert(len(regs), qt.Equals, 1)
	c.Assert(regs[0].Address, qt.Equals, "http://10.0.0.6:8080")
	audit, err := s.ListAudit()
	c.Assert(err, qt.IsNil)
	c.Assert(audit[len(audit)-1].Action, qt.Equals, "remove_worker:http://10.0.0.5:8080")
}

// TestRetries checks a failed task waits a doubling delay, a scheduled task
// runs once at the next take, and success clears both.
func TestRetries(t *testing.T) {
	c := qt.New(t)
	r := newRetries()
	id := types.ElectionID{0x91}

	c.Assert(r.due(id, taskDrain), qt.IsTrue)
	c.Assert(r.take(id, taskDispatch), qt.IsFalse)
	r.schedule(id, taskDispatch)
	c.Assert(r.take(id, taskDispatch), qt.IsTrue)
	c.Assert(r.take(id, taskDispatch), qt.IsFalse) // held off while it runs
	r.succeeded(id, taskDispatch)
	c.Assert(r.take(id, taskDispatch), qt.IsFalse)

	r.failed(id, taskDrain, errors.New("prover down"))
	c.Assert(r.due(id, taskDrain), qt.IsFalse)
	c.Assert(r.due(id, taskEnd), qt.IsTrue)
	r.succeeded(id, taskDrain)
	c.Assert(r.due(id, taskDrain), qt.IsTrue)

	// A finished election's tasks are dropped.
	r.failed(id, taskDispatch, errors.New("prover down"))
	r.forget(id)
	c.Assert(r.due(id, taskDispatch), qt.IsTrue)
	c.Assert(r.take(id, taskDispatch), qt.IsFalse)

	for failures, want := range map[int]time.Duration{
		1: time.Second, 2: 2 * time.Second, 9: 256 * time.Second, 10: retryMaxDelay, 100: retryMaxDelay,
	} {
		c.Assert(retryDelay(failures), qt.Equals, want, qt.Commentf("%d failures", failures))
	}
}
