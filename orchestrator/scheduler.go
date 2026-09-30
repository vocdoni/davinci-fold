package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
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
// with the same (deterministic) input before giving up. ZisK occasionally
// flakes at the recursion stage; the input is identical, so resubmitting
// elsewhere is always safe. Mirrors chain.Sequencer.
const maxJobAttempts = 3

const (
	// defaultJobTimeout bounds each prove, fold and finalize job.
	defaultJobTimeout = 30 * time.Minute
	// jobPollInterval is how often a running job is polled.
	jobPollInterval = 5 * time.Second
	// maxPollErrs is how many consecutive failed polls end the wait for a
	// job, as in davinci.Client.WaitForJob.
	maxPollErrs = 5
	// reservationMargin is added to the longest a batch is legitimately
	// reserved (maxJobAttempts prove jobs) to get the age at which the storage
	// monitor frees a reservation.
	reservationMargin = 5 * time.Minute
)

var (
	// errNoWorker is returned when no healthy, non-banned worker is available.
	errNoWorker = errors.New("no healthy worker available")
	// errWorkerRemoved is returned when the worker running a job is removed
	// from the pool.
	errWorkerRemoved = errors.New("worker removed from the pool")
)

// Scheduler drives the scatter/gather proving for sealed batches: it scatters
// batch STARK proves across the whole pool, gathers each resulting proof blob
// onto the election's single pinned fold worker (import), and folds them there
// on the configured cadence. It adapts chain.Sequencer's fold orchestration to
// a multi-worker pool with persisted, re-drivable inputs.
type Scheduler struct {
	engine    *Engine
	store     *storage.Storage
	pool      *workers.WorkerManager
	timeout   time.Duration // per job
	poll      time.Duration // job poll interval
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
	foldWorker    *workers.Worker     // pinned worker running the serial fold chain
	aggVK         string              // aggregator program_vk, learned on first fold
	batchVK       string              // vote-batch program_vk, learned on first batch
	lastFold      string              // last completed fold job on foldWorker, "" before genesis
	foldCount     uint64              // completed fold steps (digest step_count)
	batchesFolded uint64              // batch proofs folded so far
	pending       []*types.BatchInput // imported batches not yet folded, in order
	foldEvery     int                 // per-election fold cadence (falls back to the scheduler default)
}

// NewScheduler builds a scheduler over the engine's storage and a worker pool.
// timeout bounds each job; a batch reservation older than every attempt at
// proving it is freed by the storage monitor.
func NewScheduler(engine *Engine, pool *workers.WorkerManager, foldEvery int, timeout time.Duration) *Scheduler {
	if foldEvery <= 0 {
		foldEvery = defaultFoldEvery
	}
	if timeout <= 0 {
		timeout = defaultJobTimeout
	}
	engine.store.SetReservationTimeout(maxJobAttempts*timeout + reservationMargin)
	ctx, cancel := context.WithCancel(context.Background())
	return &Scheduler{
		engine:    engine,
		store:     engine.store,
		pool:      pool,
		timeout:   timeout,
		poll:      jobPollInterval,
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
		if fc.foldWorker == nil || !sc.pool.Has(fc.foldWorker) || !fc.foldWorker.Healthy() ||
			fc.foldWorker.IsBanned(workers.DefaultWorkerBanRules) {
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
	fc := &foldChain{foldWorker: w, foldEvery: sc.foldEvery}
	if el, err := sc.store.Election(id); err == nil && el.FoldEvery > 0 {
		fc.foldEvery = el.FoldEvery
	}
	if cp, err := sc.store.FoldCheckpoint(id); err == nil && cp != nil {
		fc.aggVK = cp.AggVK
		fc.batchVK = cp.BatchVK
		fc.lastFold = cp.LastFoldJob
		fc.foldCount = cp.FoldCount
		fc.batchesFolded = cp.BatchesFolded
		if cp.Version == 0 {
			fc.batchesFolded = sc.legacyBatchesFolded(id, cp.FoldCount, fc.foldEvery)
		}
	}
	sc.chains[id.String()] = fc
	return fc, nil
}

// legacyBatchesFolded derives the batches folded up to a version 0
// checkpoint, which stored the fold count instead: every fold folds foldEvery
// batches except the one that drains an ended election, which folds every
// imported batch.
func (sc *Scheduler) legacyBatchesFolded(id types.ElectionID, foldCount uint64, foldEvery int) uint64 {
	batches, err := sc.store.ListBatchInputs(id)
	if err != nil {
		return foldCount
	}
	var imported uint64
	for _, bi := range batches {
		if bi.ImportedID != "" {
			imported++
		}
	}
	return min(foldCount*uint64(foldEvery), imported)
}

// stopped reports whether no more work may be done for an election: it was
// canceled or can no longer be read.
func (sc *Scheduler) stopped(id types.ElectionID) bool {
	el, err := sc.store.Election(id)
	return err != nil || el.Status == types.StatusCanceled
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
		if sc.stopped(id) {
			return nil
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
	if sc.stopped(id) {
		return nil
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
	fc.pending = append(fc.pending, bi)
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
		if err := sc.waitJob(w, jobID); err != nil {
			lastErr = fmt.Errorf("prove job %s (attempt %d/%d): %w", jobID, attempt, maxJobAttempts, err)
			sc.pool.WorkerResult(w.Address, false)
			if sc.ctx.Err() != nil {
				return "", nil, lastErr
			}
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
	ready := fc != nil && len(fc.pending) >= fc.foldEvery
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
	if !ok || sc.stopped(id) {
		return nil
	}
	// The chain config comes from the election's immutable parameters.
	chainCfg := *rt.current().ChainConfig()

	fc, err := sc.chain(id)
	if err != nil {
		return err
	}

	sc.mu.Lock()
	pending := append([]*types.BatchInput(nil), fc.pending...)
	sc.mu.Unlock()
	if len(pending) == 0 {
		return nil
	}
	batchJobs := make([]string, len(pending))
	for i, bi := range pending {
		batchJobs[i] = bi.ImportedID
	}

	w := fc.foldWorker

	// Bootstrap pass to learn the aggregator program_vk (the guest cannot know
	// its own vk).
	if fc.aggVK == "" {
		bootID, err := sc.runFold(w, &davinci.FoldRequest{
			Config:    chainCfg,
			BatchJobs: batchJobs[:1],
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
		BatchJobs: batchJobs,
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
	fc.batchesFolded += uint64(len(pending))
	fc.pending = fc.pending[len(pending):]
	foldCount := fc.foldCount
	batchesFolded := fc.batchesFolded
	batchVK := fc.batchVK
	aggVK := fc.aggVK
	sc.mu.Unlock()

	if err := sc.store.SetFoldCheckpoint(&types.FoldCheckpoint{
		ElectionID:    id,
		FoldCount:     foldCount,
		LastFoldJob:   foldID,
		StateRoot:     pending[len(pending)-1].NewStateRoot,
		BatchesFolded: batchesFolded,
		AggVK:         aggVK,
		BatchVK:       batchVK,
		UpdatedAt:     time.Now(),
		Version:       types.FoldCheckpointVersion,
	}); err != nil {
		return fmt.Errorf("persist checkpoint: %w", err)
	}
	for _, bi := range pending {
		for _, voteID := range bi.VoteIDs {
			if err := sc.store.SetVoteStatus(id, voteID, types.VoteStatusFolded); err != nil {
				log.Warnw("failed to set vote folded", "vote", voteID.String(), "error", err.Error())
			}
		}
	}

	log.Infow("folded batches",
		"election", id.String(), "foldJob", foldID,
		"foldCount", foldCount, "batches", len(pending), "batchesFolded", batchesFolded)
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
		err = sc.waitJob(w, id)
		if err == nil {
			sc.pool.WorkerResult(w.Address, true)
			return id, nil
		}
		lastErr = fmt.Errorf("fold job %s (attempt %d/%d): %w", id, attempt, maxJobAttempts, err)
		sc.pool.WorkerResult(w.Address, false)
		if errors.Is(err, errWorkerRemoved) || sc.ctx.Err() != nil {
			break
		}
	}
	return "", lastErr
}

// waitJob waits for a job on w as davinci.Client.WaitForJob does, and also
// stops when w is removed from the pool, so the job counts as failed like a
// dead worker's, or when the scheduler stops.
func (sc *Scheduler) waitJob(w *workers.Worker, jobID string) error {
	deadline := time.Now().Add(sc.timeout)
	pollErrs := 0
	for {
		if !sc.pool.Has(w) {
			return errWorkerRemoved
		}
		job, err := w.Client().GetJob(jobID)
		switch {
		case err != nil:
			if pollErrs++; pollErrs >= maxPollErrs {
				return fmt.Errorf("poll job: %w", err)
			}
		case job.Status == davinci.JobStatusDone:
			return nil
		case job.Status == davinci.JobStatusFailed:
			msg := ""
			if job.Error != nil {
				msg = *job.Error
			}
			return fmt.Errorf("job failed: %s", msg)
		default:
			pollErrs = 0
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("job did not complete within %v", sc.timeout)
		}
		select {
		case <-sc.ctx.Done():
			return sc.ctx.Err()
		case <-time.After(sc.poll):
		}
	}
}
