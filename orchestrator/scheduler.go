package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/vocdoni/davinci-fold/log"

	"github.com/vocdoni/davinci-fold/storage"
	"github.com/vocdoni/davinci-fold/types"
	"github.com/vocdoni/davinci-fold/workers"
	davinci "github.com/vocdoni/davinci-zkvm/go-sdk"
)

// maxJobAttempts bounds how many times a failed prove/fold job is resubmitted
// with the same (deterministic) input before giving up on a worker. ZisK
// occasionally flakes at the recursion stage; the input is identical, so
// resubmitting elsewhere is always safe. Mirrors chain.Sequencer.
const maxJobAttempts = 3

// errNoWorker is returned when no healthy, non-banned worker is available.
var errNoWorker = fmt.Errorf("no healthy worker available")

// Scheduler drives the scatter/gather proving for sealed batches: it scatters
// batch STARK proves across the whole pool, gathers each resulting proof blob
// onto the election's single pinned fold worker (import), and folds them there
// on the configured cadence. It adapts chain.Sequencer's fold orchestration to
// a multi-worker pool with persisted, re-drivable inputs.
type Scheduler struct {
	engine    *Engine
	store     *storage.Storage
	pool      *workers.WorkerManager
	timeout   time.Duration
	foldEvery int

	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	chains map[string]*foldChain // electionID -> fold-chain state

	wg          sync.WaitGroup
	dispatchers sync.Map // electionID -> *dispatcher
	driveMu     sync.Map // electionID -> *sync.Mutex, serializes Dispatch/Fold
}

// driveLock returns the per-election mutex that serializes a whole
// Dispatch/Fold drive. Both the per-election dispatchLoop and the end-of-election
// drain path acquire it, so a sealed-batch dispatch and an end-of-election drain
// can never scatter+import the same batch concurrently or fold out of order.
func (sc *Scheduler) driveLock(id types.ElectionID) *sync.Mutex {
	m, _ := sc.driveMu.LoadOrStore(id.String(), &sync.Mutex{})
	return m.(*sync.Mutex)
}

// foldChain is the per-election fold state, the multi-worker analog of the
// fields chain.Sequencer keeps for a single client.
type foldChain struct {
	foldWorker *workers.Worker // pinned worker running the serial fold chain
	aggVK      string          // aggregator program_vk, learned on first fold
	batchVK    string          // vote-batch program_vk, learned on first batch
	lastFold   string          // last completed fold job on foldWorker, "" before genesis
	foldCount  uint64          // completed fold steps (digest step_count)
	pending    []string        // imported batch job IDs not yet folded
}

// NewScheduler builds a scheduler over the engine's storage and a worker pool.
func NewScheduler(engine *Engine, pool *workers.WorkerManager, foldEvery int, timeout time.Duration) *Scheduler {
	if foldEvery <= 0 {
		foldEvery = 1
	}
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Scheduler{
		engine:    engine,
		store:     engine.store,
		pool:      pool,
		timeout:   timeout,
		foldEvery: foldEvery,
		ctx:       ctx,
		cancel:    cancel,
		chains:    make(map[string]*foldChain),
	}
}

// chain returns the election's fold state, pinning a fold worker on first use.
// It restores aggVK/batchVK/lastFold/foldCount from a persisted checkpoint so a
// restart resumes the chain instead of re-folding from genesis.
func (sc *Scheduler) chain(id types.ElectionID) (*foldChain, error) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if fc, ok := sc.chains[id.String()]; ok {
		if fc.foldWorker == nil || !fc.foldWorker.Healthy() || fc.foldWorker.IsBanned(workers.DefaultWorkerBanRules) {
			w := sc.pool.LeastLoaded()
			if w == nil {
				return nil, errNoWorker
			}
			fc.foldWorker = w
		}
		return fc, nil
	}

	w := sc.pool.LeastLoaded()
	if w == nil {
		return nil, errNoWorker
	}
	fc := &foldChain{foldWorker: w}
	if cp, err := sc.store.FoldCheckpoint(id); err == nil && cp != nil {
		fc.aggVK = cp.AggVK
		fc.batchVK = cp.BatchVK
		fc.lastFold = cp.LastFoldJob
		fc.foldCount = cp.FoldCount
	}
	sc.chains[id.String()] = fc
	return fc, nil
}

// Dispatch drives every persisted, not-yet-proved batch of an election through
// scatter (prove on a pool worker) and gather (import onto the fold worker), in
// seq order, folding on cadence. It is idempotent: already-imported batches are
// skipped, so it is safe to call after a crash to resume. The per-election drive
// lock serializes concurrent callers (dispatchLoop vs. end-of-election drain) so
// a batch is never scattered+imported twice.
func (sc *Scheduler) Dispatch(id types.ElectionID) error {
	m := sc.driveLock(id)
	m.Lock()
	defer m.Unlock()
	return sc.dispatch(id)
}

func (sc *Scheduler) dispatch(id types.ElectionID) error {
	batches, err := sc.store.ListBatchInputs(id)
	if err != nil {
		return fmt.Errorf("list batches: %w", err)
	}
	for _, bi := range batches {
		if bi.ImportedID != "" {
			continue // already scattered+gathered
		}
		if err := sc.proveAndImport(id, bi); err != nil {
			return fmt.Errorf("batch %d: %w", bi.Seq, err)
		}
		if err := sc.maybeFold(id); err != nil {
			return fmt.Errorf("fold after batch %d: %w", bi.Seq, err)
		}
	}
	return nil
}

// proveAndImport scatters one batch to the least-loaded worker, waits for the
// STARK, imports its proof blob onto the election's fold worker, and persists
// the worker/job/imported IDs so the batch is re-drivable.
func (sc *Scheduler) proveAndImport(id types.ElectionID, bi *types.BatchInput) error {
	if !sc.store.IsBatchReserved(id, bi.Seq) {
		if err := sc.store.ReserveBatch(id, bi.Seq); err != nil {
			return fmt.Errorf("reserve: %w", err)
		}
	}
	defer func() { _ = sc.store.ReleaseBatch(id, bi.Seq) }()

	var req davinci.ProveRequest
	if err := json.Unmarshal(bi.ProveRequest, &req); err != nil {
		return fmt.Errorf("decode prove request: %w", err)
	}
	req.Output = "stark"

	jobID, worker, err := sc.scatterProve(&req)
	if err != nil {
		return err
	}

	fc, err := sc.chain(id)
	if err != nil {
		return err
	}
	if fc.batchVK == "" {
		info, err := worker.Client().FetchStarkInfo(jobID)
		if err != nil {
			return fmt.Errorf("FetchStarkInfo %s: %w", jobID, err)
		}
		fc.batchVK = info.ProgramVK
	}

	raw, err := worker.Client().FetchStarkRaw(jobID)
	if err != nil {
		return fmt.Errorf("FetchStarkRaw %s: %w", jobID, err)
	}
	importedID, err := fc.foldWorker.Client().ImportStark(raw)
	if err != nil {
		return fmt.Errorf("import onto fold worker %s: %w", fc.foldWorker.Address, err)
	}

	bi.Worker = worker.Address
	bi.JobID = jobID
	bi.ImportedID = importedID
	if err := sc.store.SetBatchInput(bi); err != nil {
		return fmt.Errorf("persist batch: %w", err)
	}

	sc.mu.Lock()
	fc.pending = append(fc.pending, importedID)
	sc.mu.Unlock()

	log.Infow("batch scattered+imported",
		"election", id.String(), "seq", bi.Seq,
		"worker", worker.Address, "job", jobID, "imported", importedID)
	return nil
}

// scatterProve submits a batch prove to the least-loaded healthy worker and
// waits for it, retrying on a fresh worker if the job fails. Returns the job ID
// and the worker that proved it.
func (sc *Scheduler) scatterProve(req *davinci.ProveRequest) (string, *workers.Worker, error) {
	var lastErr error
	for attempt := 1; attempt <= maxJobAttempts; attempt++ {
		w := sc.pool.LeastLoaded()
		if w == nil {
			return "", nil, errNoWorker
		}
		jobID, err := w.Client().SubmitProve(req)
		if err != nil {
			lastErr = fmt.Errorf("submit prove (attempt %d/%d): %w", attempt, maxJobAttempts, err)
			sc.pool.WorkerResult(w.Address, false)
			continue
		}
		if _, err := w.Client().WaitForJob(jobID, sc.timeout); err != nil {
			lastErr = fmt.Errorf("prove job %s (attempt %d/%d): %w", jobID, attempt, maxJobAttempts, err)
			sc.pool.WorkerResult(w.Address, false)
			continue
		}
		sc.pool.WorkerResult(w.Address, true)
		return jobID, w, nil
	}
	return "", nil, lastErr
}

// maybeFold folds the election's pending imported batches once the fold cadence
// is reached, persisting the resulting checkpoint.
func (sc *Scheduler) maybeFold(id types.ElectionID) error {
	sc.mu.Lock()
	fc := sc.chains[id.String()]
	ready := fc != nil && len(fc.pending) >= sc.foldEvery
	sc.mu.Unlock()
	if !ready {
		return nil
	}
	return sc.fold(id)
}

// Fold folds all of an election's pending imported batches into its chain on
// the pinned fold worker. The first fold runs a bootstrap pass to learn the
// aggregator program_vk, then the genesis fold binds it; later folds chain via
// PrevFoldJob. Adapts chain.Sequencer.Fold to the pool. No-op if nothing is
// pending. The per-election drive lock serializes it against Dispatch so folds
// never run out of order.
func (sc *Scheduler) Fold(id types.ElectionID) error {
	m := sc.driveLock(id)
	m.Lock()
	defer m.Unlock()
	return sc.fold(id)
}

func (sc *Scheduler) fold(id types.ElectionID) error {
	rt, ok := sc.engine.runtime(id)
	if !ok {
		return fmt.Errorf("unknown election %s", id.String())
	}
	chainCfg := *rt.state.ChainConfig()

	fc, err := sc.chain(id)
	if err != nil {
		return err
	}

	sc.mu.Lock()
	pending := append([]string(nil), fc.pending...)
	sc.mu.Unlock()
	if len(pending) == 0 {
		return nil
	}

	w := fc.foldWorker

	// Bootstrap pass to learn the aggregator program_vk (the guest cannot know
	// its own vk).
	if fc.aggVK == "" {
		bootID, err := sc.runFold(w, &davinci.FoldRequest{
			Config:    chainCfg,
			BatchJobs: pending[:1],
		})
		if err != nil {
			return fmt.Errorf("bootstrap fold: %w", err)
		}
		info, err := w.Client().FetchStarkInfo(bootID)
		if err != nil {
			return fmt.Errorf("bootstrap fold info: %w", err)
		}
		fc.aggVK = info.ProgramVK
	}

	req := &davinci.FoldRequest{
		Config:    chainCfg,
		BatchJobs: pending,
	}
	if fc.lastFold == "" {
		req.FoldVK = fc.aggVK // genesis fold binds the learned vk
	} else {
		req.PrevFoldJob = fc.lastFold
	}
	foldID, err := sc.runFold(w, req)
	if err != nil {
		return err
	}

	sc.mu.Lock()
	fc.lastFold = foldID
	fc.foldCount++
	fc.pending = fc.pending[len(pending):]
	foldCount := fc.foldCount
	batchVK := fc.batchVK
	aggVK := fc.aggVK
	sc.mu.Unlock()

	if err := sc.store.SetFoldCheckpoint(&types.FoldCheckpoint{
		ElectionID:    id,
		FoldCount:     foldCount,
		LastFoldJob:   foldID,
		StateRoot:     rt.state.Root(),
		BatchesFolded: foldCount, // batches-per-fold cadence tracked separately if needed
		AggVK:         aggVK,
		BatchVK:       batchVK,
		UpdatedAt:     time.Now(),
	}); err != nil {
		return fmt.Errorf("persist checkpoint: %w", err)
	}

	log.Infow("folded batches",
		"election", id.String(), "foldJob", foldID,
		"foldCount", foldCount, "batches", len(pending))
	return nil
}

// runFold submits a fold to the fold worker and waits for it, resubmitting the
// identical request on failure up to maxJobAttempts times.
func (sc *Scheduler) runFold(w *workers.Worker, req *davinci.FoldRequest) (string, error) {
	var lastErr error
	for attempt := 1; attempt <= maxJobAttempts; attempt++ {
		id, err := w.Client().SubmitFold(req)
		if err != nil {
			return "", fmt.Errorf("submit fold: %w", err)
		}
		if _, err := w.Client().WaitForJob(id, sc.timeout); err == nil {
			sc.pool.WorkerResult(w.Address, true)
			return id, nil
		} else {
			lastErr = fmt.Errorf("fold job %s (attempt %d/%d): %w", id, attempt, maxJobAttempts, err)
			sc.pool.WorkerResult(w.Address, false)
		}
	}
	return "", lastErr
}
