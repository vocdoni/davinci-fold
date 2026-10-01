package orchestrator

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/vocdoni/davinci-fold/storage"
	"github.com/vocdoni/davinci-fold/types"
)

// scatter proves the batches of one dispatch pass in parallel, each on its
// own goroutine and its own worker (see batchProof and acquire), while the
// pass gathers their proofs in seq order (see proof). A batch's prove input
// is fixed at seal time, so batches are proved independently; only the
// imports and the folds are ordered. It is used from the pass's goroutine
// only; a goroutine proving a batch gets what it reads of the fold chain when
// it starts.
type scatter struct {
	sc      *Scheduler
	id      types.ElectionID
	fc      *foldChain
	batches []*types.BatchInput // sealed batches in seq order, topped up as more are sealed
	tasks   map[uint64]*proveTask
	wg      sync.WaitGroup
}

// proveTask is the proof of a batch being made.
type proveTask struct {
	done  chan struct{} // closed once proof or err is set
	proof []byte
	err   error
}

func (sc *Scheduler) newScatter(id types.ElectionID, fc *foldChain, batches []*types.BatchInput) *scatter {
	return &scatter{
		sc:      sc,
		id:      id,
		fc:      fc,
		batches: slices.Clone(batches),
		tasks:   make(map[uint64]*proveTask),
	}
}

// proof returns the proof of the batch at position i, the next one to
// gather, starting the proves of the batches after it while it waits.
func (s *scatter) proof(i int) ([]byte, error) {
	seq := s.batches[i].Seq
	s.topUp()
	for {
		s.start(i)
		t := s.tasks[seq]
		select {
		case <-t.done:
			delete(s.tasks, seq)
			return t.proof, t.err
		case <-time.After(s.sc.poll):
			s.topUp()
		}
	}
}

// start starts getting the proofs of the batches from position i on, as many
// as the pool has workers that can take a job. If a batch past those has a
// prove job a previous run left, the window extends to the last such batch:
// every batch up to it is started, those without a job too. The left jobs
// are claimed before any new prove starts, so none is sent to a worker that
// still runs one.
func (s *scatter) start(i int) {
	// ponytail: new proves run at most one pool's worth of batches ahead of
	// the gather, so a slow batch idles the rest of the pool for this
	// election once the batches behind it are proved.
	end := min(i+max(s.sc.pool.Available(), 1), len(s.batches))
	for j := end; j < len(s.batches); j++ {
		if _, ok := s.tasks[s.batches[j].Seq]; !ok && s.batches[j].JobID != "" {
			end = j + 1
		}
	}
	var proves []func()
	for _, bi := range s.batches[i:end] {
		if _, ok := s.tasks[bi.Seq]; !ok {
			t, prove := s.newTask(bi)
			s.tasks[bi.Seq] = t
			if prove != nil {
				proves = append(proves, prove)
			}
		}
	}
	for _, prove := range proves {
		s.wg.Go(prove)
	}
}

// newTask returns the task getting the proof of a batch: done with its stored
// proof, or else with the function that proves it (see batchProof), for its
// own goroutine. A prove job a previous run left on a worker is claimed here.
func (s *scatter) newTask(bi *types.BatchInput) (*proveTask, func()) {
	t := &proveTask{done: make(chan struct{})}
	proof, err := s.sc.store.BatchProof(s.id, bi.Seq)
	if !errors.Is(err, storage.ErrNotFound) {
		if err != nil {
			err = fmt.Errorf("load proof: %w", err)
		}
		t.proof, t.err = proof, err
		close(t.done)
		return t, nil
	}
	avoid, wantVK := slices.Clone(s.fc.avoid[bi.Seq]), s.fc.wantBatchVK()
	held := s.sc.reusable(bi, avoid)
	return t, func() {
		defer close(t.done)
		t.proof, t.err = s.sc.batchProof(s.id, bi, held, avoid, wantVK)
	}
}

// topUp adds the batches sealed since the pass started.
func (s *scatter) topUp() {
	for {
		bi, err := s.sc.store.BatchInput(s.id, s.batches[len(s.batches)-1].Seq+1)
		if err != nil {
			return
		}
		s.batches = append(s.batches, bi)
	}
}

// wait waits for the proves still running when the pass ends; their proofs
// are stored for the next pass.
func (s *scatter) wait() { s.wg.Wait() }
