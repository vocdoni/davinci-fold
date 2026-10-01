package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/vocdoni/davinci-node/db"
	"github.com/vocdoni/davinci-node/db/metadb"

	"github.com/vocdoni/davinci-fold/storage"
	"github.com/vocdoni/davinci-fold/types"
	"github.com/vocdoni/davinci-fold/workers"
	davinci "github.com/vocdoni/davinci-zkvm/go-sdk"
	"github.com/vocdoni/davinci-zkvm/go-sdk/chain"
)

// fakeProver stands in for a davinci-zkvm prover: jobs are done as soon as
// they are submitted, except prove jobs while hold is set, which run until
// it is cleared.
type fakeProver struct {
	srv      *httptest.Server
	queueLen int

	mu        sync.Mutex
	next      int
	running   map[string]bool // prove jobs held running
	failed    map[string]bool // fold jobs that fail
	failFolds bool            // fold jobs submitted now fail
	hold      bool
	proves    int
	imports   int
	folds     [][]string // batch jobs of each fold request
	prevs     []string   // previous fold job of each fold request
}

// newFakeProver starts a fake prover reporting queueLen jobs queued; with
// hold, its prove jobs run until release.
func newFakeProver(t *testing.T, queueLen int, hold bool) *fakeProver {
	t.Helper()
	fp := &fakeProver{queueLen: queueLen, hold: hold, running: map[string]bool{}, failed: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, davinci.HealthResponse{Status: "ok", QueueLen: fp.queueLen})
	})
	mux.HandleFunc("POST /prove", func(w http.ResponseWriter, _ *http.Request) {
		fp.mu.Lock()
		defer fp.mu.Unlock()
		fp.proves++
		id := fp.newJob("prove")
		fp.running[id] = fp.hold
		writeJSON(w, http.StatusAccepted, davinci.ProveResponse{JobID: id})
	})
	mux.HandleFunc("POST /jobs/import", func(w http.ResponseWriter, _ *http.Request) {
		fp.mu.Lock()
		defer fp.mu.Unlock()
		fp.imports++
		writeJSON(w, http.StatusOK, davinci.ProveResponse{JobID: fp.newJob("import")})
	})
	mux.HandleFunc("POST /fold", func(w http.ResponseWriter, r *http.Request) {
		var req davinci.FoldRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		fp.mu.Lock()
		defer fp.mu.Unlock()
		fp.folds = append(fp.folds, req.BatchJobs)
		fp.prevs = append(fp.prevs, req.PrevFoldJob)
		id := fp.newJob("fold")
		fp.failed[id] = fp.failFolds
		writeJSON(w, http.StatusAccepted, davinci.ProveResponse{JobID: id})
	})
	mux.HandleFunc("GET /jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		fp.mu.Lock()
		defer fp.mu.Unlock()
		resp := davinci.JobResponse{JobID: r.PathValue("id"), Status: davinci.JobStatusDone}
		switch {
		case fp.running[resp.JobID]:
			resp.Status = davinci.JobStatusRunning
		case fp.failed[resp.JobID]:
			msg := "rejected"
			resp.Status, resp.Error = davinci.JobStatusFailed, &msg
		}
		writeJSON(w, http.StatusOK, resp)
	})
	mux.HandleFunc("GET /jobs/{id}/stark", func(w http.ResponseWriter, r *http.Request) {
		vk := chain.CircuitRelease.BatchVK
		if strings.HasPrefix(r.PathValue("id"), "fold") {
			vk = chain.CircuitRelease.AggVK
		}
		writeJSON(w, http.StatusOK, davinci.StarkInfo{ProgramVK: vk})
	})
	mux.HandleFunc("GET /jobs/{id}/snark/raw", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("proof"))
	})
	fp.srv = httptest.NewServer(mux)
	t.Cleanup(fp.srv.Close)
	return fp
}

// newJob returns a fresh job ID. The caller holds fp.mu.
func (fp *fakeProver) newJob(kind string) string {
	fp.next++
	return fmt.Sprintf("%s-%d", kind, fp.next)
}

// release lets the held prove jobs finish.
func (fp *fakeProver) release() {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	fp.hold = false
	clear(fp.running)
}

// counts returns the prove, import and fold requests received.
func (fp *fakeProver) counts() (proves, imports, folds int) {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	return fp.proves, fp.imports, len(fp.folds)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// newProvingEngine builds an engine that proves on the given fake provers.
func newProvingEngine(t *testing.T, provers ...*fakeProver) (*Engine, *storage.Storage, *workers.WorkerManager) {
	t.Helper()
	database, err := metadb.New(db.TypeInMem, "")
	qt.Assert(t, err, qt.IsNil)
	s := storage.New(database)
	pool := workers.NewWorkerManager(nil, 10*time.Millisecond)
	for _, fp := range provers {
		pool.AddWorker(fp.srv.URL, "")
	}
	ctx, cancel := context.WithCancel(context.Background())
	pool.Start(ctx)
	e, err := NewEngine(s, Options{
		BatchSize:       2,
		BatchTimeWindow: time.Hour,
		Validator:       structuralValidator{},
		Pool:            pool,
		FoldEvery:       2,
	})
	qt.Assert(t, err, qt.IsNil)
	e.scheduler.poll = 5 * time.Millisecond
	t.Cleanup(func() {
		e.Stop()
		pool.Stop()
		cancel()
	})
	return e, s, pool
}

// waitFor polls cond until it holds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestFoldCheckpointCountsBatches drives four batches through the scheduler
// with a fold every two: the checkpoint counts four batches in two folds, and
// every vote is folded.
func TestFoldCheckpointCountsBatches(t *testing.T) {
	c := qt.New(t)
	fp := newFakeProver(t, 0, false)
	e, s, _ := newProvingEngine(t, fp)

	el, encKey := testElection(t, 0x70, time.Now().Add(time.Hour))
	el.FoldEvery = 0 // the engine default, 2
	c.Assert(e.CreateElection("admin", el), qt.IsNil)
	for i := range testCensusSize {
		_, err := e.SubmitVote(el.ID, makeSub(t, encKey, i))
		c.Assert(err, qt.IsNil)
	}

	var cp *types.FoldCheckpoint
	waitFor(t, "two folds", func() bool {
		var err error
		cp, err = s.FoldCheckpoint(el.ID)
		return err == nil && cp.FoldCount == 2
	})
	c.Assert(cp.BatchesFolded, qt.Equals, uint64(4))
	c.Assert(cp.Version, qt.Equals, uint8(types.FoldCheckpointVersion))
	last, err := s.BatchInput(el.ID, 3)
	c.Assert(err, qt.IsNil)
	c.Assert(cp.StateRoot, qt.Equals, last.NewStateRoot)
	for i := range testCensusSize {
		c.Assert(voteStatus(t, s, el.ID, byte(0xa0+i)), qt.Equals, types.VoteStatusFolded)
	}
	// A bootstrap fold of the first batch, then two folds of two batches.
	fp.mu.Lock()
	c.Assert(len(fp.folds), qt.Equals, 3)
	c.Assert(len(fp.folds[0]), qt.Equals, 1)
	c.Assert(len(fp.folds[1]), qt.Equals, 2)
	c.Assert(len(fp.folds[2]), qt.Equals, 2)
	fp.mu.Unlock()
}

// TestLegacyCheckpointBatchesFolded checks a version 0 checkpoint, which
// stored the fold count as BatchesFolded, is read as the batches it folded.
func TestLegacyCheckpointBatchesFolded(t *testing.T) {
	c := qt.New(t)
	e, s, _ := newProvingEngine(t, newFakeProver(t, 0, false))

	el, _ := testElection(t, 0x71, time.Now().Add(time.Hour))
	el.FoldEvery = 2
	c.Assert(e.CreateElection("admin", el), qt.IsNil)
	// Five imported batches, two folds of two (the fifth not folded yet).
	for seq := range uint64(5) {
		c.Assert(s.SetBatchInput(&types.BatchInput{ElectionID: el.ID, Seq: seq, ImportedID: fmt.Sprint(seq)}), qt.IsNil)
	}
	c.Assert(s.SetFoldCheckpoint(&types.FoldCheckpoint{
		ElectionID: el.ID, FoldCount: 2, BatchesFolded: 2, LastFoldJob: "fold-2",
	}), qt.IsNil)
	fc, err := e.scheduler.chain(el.ID)
	c.Assert(err, qt.IsNil)
	c.Assert(fc.foldCount, qt.Equals, uint64(2))
	c.Assert(fc.batchesFolded, qt.Equals, uint64(4))

	// A drained election folded all of its imported batches.
	el2, _ := testElection(t, 0x72, time.Now().Add(time.Hour))
	el2.FoldEvery = 2
	c.Assert(e.CreateElection("admin", el2), qt.IsNil)
	for seq := range uint64(3) {
		c.Assert(s.SetBatchInput(&types.BatchInput{ElectionID: el2.ID, Seq: seq, ImportedID: fmt.Sprint(seq)}), qt.IsNil)
	}
	c.Assert(s.SetFoldCheckpoint(&types.FoldCheckpoint{ElectionID: el2.ID, FoldCount: 2, BatchesFolded: 2}), qt.IsNil)
	fc, err = e.scheduler.chain(el2.ID)
	c.Assert(err, qt.IsNil)
	c.Assert(fc.batchesFolded, qt.Equals, uint64(3))
}

// TestCanceledElectionIsNotProved checks a batch whose prove job is running
// when its election is canceled is not imported or folded.
func TestCanceledElectionIsNotProved(t *testing.T) {
	c := qt.New(t)
	fp := newFakeProver(t, 0, true)
	e, s, _ := newProvingEngine(t, fp)

	el, encKey := testElection(t, 0x73, time.Now().Add(time.Hour))
	c.Assert(e.CreateElection("admin", el), qt.IsNil)
	for i := range 2 {
		_, err := e.SubmitVote(el.ID, makeSub(t, encKey, i))
		c.Assert(err, qt.IsNil)
	}
	waitFor(t, "the prove job", func() bool { p, _, _ := fp.counts(); return p == 1 })

	c.Assert(e.SetStatus("admin", el.ID, types.StatusCanceled), qt.IsNil)
	fp.release()
	time.Sleep(100 * time.Millisecond)
	proves, imports, folds := fp.counts()
	c.Assert(proves, qt.Equals, 1)
	c.Assert(imports, qt.Equals, 0)
	c.Assert(folds, qt.Equals, 0)
	bi, err := s.BatchInput(el.ID, 0)
	c.Assert(err, qt.IsNil)
	c.Assert(bi.ImportedID, qt.Equals, "")
}

// TestRemovedWorkerJobMoves checks a prove job running on a worker that is
// removed from the pool is proved again on another one.
func TestRemovedWorkerJobMoves(t *testing.T) {
	c := qt.New(t)
	busy := newFakeProver(t, 0, true) // picked first: shortest queue
	spare := newFakeProver(t, 5, false)
	e, s, pool := newProvingEngine(t, busy, spare)

	el, encKey := testElection(t, 0x74, time.Now().Add(time.Hour))
	el.FoldEvery = 1
	c.Assert(e.CreateElection("admin", el), qt.IsNil)
	for i := range 2 {
		_, err := e.SubmitVote(el.ID, makeSub(t, encKey, i))
		c.Assert(err, qt.IsNil)
	}
	waitFor(t, "the prove job", func() bool { p, _, _ := busy.counts(); return p == 1 })

	pool.RemoveWorker(busy.srv.URL)
	waitFor(t, "the batch import", func() bool {
		bi, err := s.BatchInput(el.ID, 0)
		return err == nil && bi.ImportedID != ""
	})
	bi, err := s.BatchInput(el.ID, 0)
	c.Assert(err, qt.IsNil)
	c.Assert(bi.Worker, qt.Equals, spare.srv.URL)
	proves, imports, _ := spare.counts()
	c.Assert(proves, qt.Equals, 1)
	c.Assert(imports, qt.Equals, 1)
}

// TestMoveWithoutFoldProofFoldsFromGenesis moves a fold chain whose last
// fold's proof is not stored: the new fold worker folds again from genesis,
// proving the folded batches again.
func TestMoveWithoutFoldProofFoldsFromGenesis(t *testing.T) {
	c := qt.New(t)
	a := newFakeProver(t, 0, false)
	b := newFakeProver(t, 5, false)
	e, s, pool := newProvingEngine(t, a, b)

	el, encKey := testElection(t, 0x75, time.Now().Add(time.Hour))
	el.FoldEvery = 2
	c.Assert(e.CreateElection("admin", el), qt.IsNil)
	for i := range 4 {
		_, err := e.SubmitVote(el.ID, makeSub(t, encKey, i))
		c.Assert(err, qt.IsNil)
	}
	waitFor(t, "the first fold", func() bool {
		cp, err := s.FoldCheckpoint(el.ID)
		return err == nil && cp.FoldCount == 1
	})
	c.Assert(s.DeleteProofs(el.ID), qt.IsNil)
	pool.RemoveWorker(a.srv.URL)

	for i := 4; i < 6; i++ {
		_, err := e.SubmitVote(el.ID, makeSub(t, encKey, i))
		c.Assert(err, qt.IsNil)
	}
	waitFor(t, "the third batch on b", func() bool {
		bi, err := s.BatchInput(el.ID, 2)
		return err == nil && bi.FoldWorker == b.srv.URL
	})
	cp, err := s.FoldCheckpoint(el.ID)
	c.Assert(err, qt.IsNil)
	c.Assert(cp.FoldCount, qt.Equals, uint64(1))
	c.Assert(cp.BatchesFolded, qt.Equals, uint64(2))
	c.Assert(cp.FoldWorker, qt.Equals, b.srv.URL)
	got, err := e.Election(el.ID)
	c.Assert(err, qt.IsNil)
	c.Assert(got.FoldWorker, qt.Equals, b.srv.URL)
	b.mu.Lock()
	defer b.mu.Unlock()
	c.Assert(b.proves, qt.Equals, 3)
	// One genesis fold of the two batches proved again; the aggregator vk was
	// already known, so no bootstrap pass.
	c.Assert(len(b.folds), qt.Equals, 1)
	c.Assert(len(b.folds[0]), qt.Equals, 2)
	c.Assert(b.prevs, qt.DeepEquals, []string{""})
}

// TestFoldFailuresEscalate makes every fold job fail after a first fold:
// after maxFoldFailures failed folds the pending batch is proved again, once;
// after as many more the last fold's proof is reported as suspect, and the
// chain head is kept.
func TestFoldFailuresEscalate(t *testing.T) {
	c := qt.New(t)
	fp := newFakeProver(t, 0, false)
	database, err := metadb.New(db.TypeInMem, "")
	c.Assert(err, qt.IsNil)
	s := storage.New(database)
	// Lenient bans: the only worker stays usable through the failures.
	pool := workers.NewWorkerManager(&workers.WorkerBanRules{BanTimeout: time.Minute, FailuresToGetBanned: 1000}, 10*time.Millisecond)
	pool.AddWorker(fp.srv.URL, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)
	defer pool.Stop()
	e, err := NewEngine(s, Options{
		BatchSize: 2, BatchTimeWindow: time.Hour, Validator: structuralValidator{}, Pool: pool, FoldEvery: 1,
	})
	c.Assert(err, qt.IsNil)
	defer e.Stop()
	e.scheduler.poll = time.Millisecond

	el, encKey := testElection(t, 0x76, time.Now().Add(time.Hour))
	el.FoldEvery = 1
	c.Assert(e.CreateElection("admin", el), qt.IsNil)
	for i := range 2 {
		_, err := e.SubmitVote(el.ID, makeSub(t, encKey, i))
		c.Assert(err, qt.IsNil)
	}
	waitFor(t, "the first fold", func() bool {
		cp, err := s.FoldCheckpoint(el.ID)
		return err == nil && cp.FoldCount == 1
	})
	fp.mu.Lock()
	fp.failFolds = true
	fp.mu.Unlock()
	for i := 2; i < 4; i++ {
		_, err := e.SubmitVote(el.ID, makeSub(t, encKey, i))
		c.Assert(err, qt.IsNil)
	}

	fc, err := e.scheduler.chain(el.ID)
	c.Assert(err, qt.IsNil)
	suspect := func() bool {
		m := e.scheduler.driveLock(el.ID)
		m.Lock()
		defer m.Unlock()
		return fc.headSuspect
	}
	deadline := time.Now().Add(10 * time.Second)
	for !suspect() {
		c.Assert(time.Now().Before(deadline), qt.IsTrue, qt.Commentf("the head never became suspect"))
		c.Assert(e.scheduler.Dispatch(el.ID), qt.ErrorIs, errFoldWorker)
	}
	fp.mu.Lock()
	c.Assert(fp.proves, qt.Equals, 3) // batch 0, batch 1, batch 1 again
	fp.mu.Unlock()
	cp, err := s.FoldCheckpoint(el.ID)
	c.Assert(err, qt.IsNil)
	c.Assert(cp.FoldCount, qt.Equals, uint64(1))
	_, err = s.FoldProof(el.ID)
	c.Assert(err, qt.IsNil)
}

// TestProveAvoidsDroppedWorker checks a batch whose proof from a worker was
// dropped is proved on another worker.
func TestProveAvoidsDroppedWorker(t *testing.T) {
	c := qt.New(t)
	a := newFakeProver(t, 0, false)
	b := newFakeProver(t, 5, false)
	e, _, pool := newProvingEngine(t, a, b)
	waitFor(t, "both healthy", func() bool {
		wa, _ := pool.GetWorker(a.srv.URL)
		wb, _ := pool.GetWorker(b.srv.URL)
		return wa.Healthy() && wb.Healthy()
	})

	bi := &types.BatchInput{ElectionID: types.ElectionID{0x77}, ProveRequest: []byte("{}")}
	fc := &foldChain{avoid: map[uint64][]string{0: {a.srv.URL}}}
	proof, err := e.scheduler.prove(bi.ElectionID, fc, bi)
	c.Assert(err, qt.IsNil)
	c.Assert(string(proof), qt.Equals, "proof")
	c.Assert(bi.Worker, qt.Equals, b.srv.URL)
	proves, _, _ := a.counts()
	c.Assert(proves, qt.Equals, 0)
}

// TestReusedProofChecks checks a prove job of a previous run is reused only
// if its worker is in the pool and not avoided for the batch, the job did
// not fail and it is a STARK of the batch circuit.
func TestReusedProofChecks(t *testing.T) {
	c := qt.New(t)
	fp := newFakeProver(t, 0, false)
	e, _, _ := newProvingEngine(t, fp)
	fp.mu.Lock()
	fp.failed["prove-8"] = true
	fp.mu.Unlock()

	id := types.ElectionID{0x78}
	for _, tc := range []struct {
		name   string
		worker string
		job    string
		avoid  bool
		reused bool
	}{
		{"batch stark", fp.srv.URL, "prove-9", false, true},
		{"no job", fp.srv.URL, "", false, false},
		{"failed job", fp.srv.URL, "prove-8", false, false},
		{"fold job", fp.srv.URL, "fold-9", false, false},
		{"worker gone", "http://127.0.0.1:1", "prove-9", false, false},
		{"worker avoided", fp.srv.URL, "prove-9", true, false},
	} {
		fc := &foldChain{}
		if tc.avoid {
			fc.avoid = map[uint64][]string{0: {tc.worker}}
		}
		bi := &types.BatchInput{ElectionID: id, Worker: tc.worker, JobID: tc.job}
		c.Assert(e.scheduler.reusedProof(id, fc, bi) != nil, qt.Equals, tc.reused, qt.Commentf("%s", tc.name))
	}
}
