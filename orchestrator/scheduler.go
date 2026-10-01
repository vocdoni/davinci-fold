package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/vocdoni/davinci-fold/log"

	"github.com/vocdoni/davinci-fold/storage"
	"github.com/vocdoni/davinci-fold/types"
	"github.com/vocdoni/davinci-fold/workers"
	davinci "github.com/vocdoni/davinci-zkvm/go-sdk"
	"github.com/vocdoni/davinci-zkvm/go-sdk/chain"
)

// maxJobAttempts bounds how many times a failed prove/fold job is resubmitted
// with the same (deterministic) input before giving up. ZisK occasionally
// flakes at the recursion stage; the input is identical, so resubmitting
// elsewhere is always safe. Mirrors chain.Sequencer.
const maxJobAttempts = 3

const (
	// defaultJobTimeout bounds each prove, fold and finalize job.
	defaultJobTimeout = 30 * time.Minute
	// defaultJobPoll is how often a running job is polled, and the pause
	// between two attempts on the fold worker.
	defaultJobPoll = 5 * time.Second
	// maxPollErrs is how many consecutive failed polls end the wait for a
	// job, as in davinci.Client.WaitForJob.
	maxPollErrs = 5
	// reservationMargin is added to the longest a batch is legitimately
	// reserved (maxJobAttempts prove jobs) to get the age at which the storage
	// monitor frees a reservation.
	reservationMargin = 5 * time.Minute
	// maxFoldFailures is how many fold jobs in a row may fail on an unchanged
	// chain head before the pending batches are proved again (see foldFailed).
	maxFoldFailures = 3
)

var (
	// errNoWorker is returned when no healthy, non-banned worker is available.
	errNoWorker = errors.New("no healthy worker available")
	// errWorkerRemoved is returned when the worker running a job is removed
	// from the pool.
	errWorkerRemoved = errors.New("worker removed from the pool")
	// errFoldWorker marks a failure of an election's fold worker that the
	// retries on it did not fix: the fold chain moves to another worker.
	errFoldWorker = errors.New("fold worker failed")
	// errJobFailed is a job the prover ran and reported as failed.
	errJobFailed = errors.New("job failed")
)

// Scheduler drives the scatter/gather proving for sealed batches: it scatters
// batch STARK proves across the whole pool, gathers each resulting proof onto
// the election's pinned fold worker (import), and folds them there on the
// configured cadence. It keeps in storage the proofs the chain needs to
// continue elsewhere, and moves the chain to another worker when the fold
// worker is lost or keeps failing. It adapts chain.Sequencer's fold
// orchestration to a multi-worker pool with persisted, re-drivable inputs.
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
	driveMu     sync.Map // electionID -> *sync.Mutex, serializes the work on a fold chain
}

// driveLock returns the per-election mutex that serializes all the work on
// an election's fold chain: the dispatch passes, the end-of-election drain
// and the finalize. A batch is never gathered twice and folds never run out
// of order.
func (sc *Scheduler) driveLock(id types.ElectionID) *sync.Mutex {
	m, _ := sc.driveMu.LoadOrStore(id.String(), &sync.Mutex{})
	return m.(*sync.Mutex)
}

// foldChain is an election's fold state, the multi-worker analog of the
// fields chain.Sequencer keeps for a single client. It is loaded from
// storage on first use and only used under the election's drive lock.
type foldChain struct {
	pin           string              // fold worker address (Election.FoldWorker)
	synced        bool                // lastFold and pending are on the pin
	resync        bool                // the pin failed: import the chain again wherever it goes
	aggVK         string              // aggregator program_vk, learned on first fold
	batchVK       string              // vote-batch program_vk, learned on first import
	lastFold      string              // last completed fold job, "" before genesis
	lastFoldOn    string              // address of the worker holding lastFold
	foldCount     uint64              // completed fold steps (digest step_count)
	batchesFolded uint64              // batches folded so far: the seqs below it
	stateRoot     string              // new state root of the last folded batch
	pending       []*types.BatchInput // batches imported on the pin, not folded, in seq order
	foldEvery     int                 // per-election fold cadence (falls back to the scheduler default)

	// Escalation of folds that keep failing on this head (see foldFailed).
	foldFailures int                 // fold jobs failed in a row
	reproved     bool                // the pending batches were proved again
	headSuspect  bool                // the last fold's proof was reported as suspect
	avoid        map[uint64][]string // seq -> workers whose proof of the batch was dropped
}

// next is the seq of the first batch not imported on the pin.
func (fc *foldChain) next() uint64 { return fc.batchesFolded + uint64(len(fc.pending)) }

// wantBatchVK is the program vk every batch proof must have: the pinned
// circuit release's, or else the one learned from the first batch ("" before
// it).
func (fc *foldChain) wantBatchVK() string {
	if chain.CircuitRelease.IsSet() {
		return chain.CircuitRelease.BatchVK
	}
	return fc.batchVK
}

// wantAggVK is the program vk every fold proof must have: the pinned circuit
// release's, or else the one the bootstrap fold learned ("" before it).
func (fc *foldChain) wantAggVK() string {
	if chain.CircuitRelease.IsSet() {
		return chain.CircuitRelease.AggVK
	}
	return fc.aggVK
}

// pendingSeqs returns the seqs of the pending batches.
func (fc *foldChain) pendingSeqs() []uint64 {
	seqs := make([]uint64, len(fc.pending))
	for i, bi := range fc.pending {
		seqs[i] = bi.Seq
	}
	return seqs
}

// sameVK reports whether two program vks are equal, ignoring the 0x prefix
// and the case.
func sameVK(a, b string) bool {
	return strings.EqualFold(strings.TrimPrefix(a, "0x"), strings.TrimPrefix(b, "0x"))
}

// checkpoint is the head of the chain as persisted.
func (fc *foldChain) checkpoint(id types.ElectionID) *types.FoldCheckpoint {
	return &types.FoldCheckpoint{
		ElectionID:    id,
		FoldCount:     fc.foldCount,
		LastFoldJob:   fc.lastFold,
		FoldWorker:    fc.lastFoldOn,
		StateRoot:     fc.stateRoot,
		BatchesFolded: fc.batchesFolded,
		AggVK:         fc.aggVK,
		BatchVK:       fc.batchVK,
		Version:       types.FoldCheckpointVersion,
	}
}

// NewScheduler builds a scheduler over the engine's storage and a worker pool.
// timeout bounds each job and poll is how often a running job is polled;
// a batch reservation older than every attempt at proving it is freed by the
// storage monitor.
func NewScheduler(engine *Engine, pool *workers.WorkerManager, foldEvery int, timeout, poll time.Duration) *Scheduler {
	if foldEvery <= 0 {
		foldEvery = defaultFoldEvery
	}
	if timeout <= 0 {
		timeout = defaultJobTimeout
	}
	if poll <= 0 {
		poll = defaultJobPoll
	}
	engine.store.SetReservationTimeout(maxJobAttempts*timeout + reservationMargin)
	ctx, cancel := context.WithCancel(context.Background())
	return &Scheduler{
		engine:    engine,
		store:     engine.store,
		pool:      pool,
		timeout:   timeout,
		poll:      poll,
		foldEvery: foldEvery,
		ctx:       ctx,
		cancel:    cancel,
		chains:    make(map[string]*foldChain),
	}
}

// chain returns the election's fold state, loading it from storage on first
// use: the pinned fold worker and the head of the chain. The imported
// batches are listed when the chain is synced onto its worker (sync).
func (sc *Scheduler) chain(id types.ElectionID) (*foldChain, error) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if fc, ok := sc.chains[id.String()]; ok {
		return fc, nil
	}
	el, err := sc.store.Election(id)
	if err != nil {
		return nil, fmt.Errorf("load election: %w", err)
	}
	fc := &foldChain{pin: el.FoldWorker, foldEvery: sc.foldEvery}
	if el.FoldEvery > 0 {
		fc.foldEvery = el.FoldEvery
	}
	cp, err := sc.store.FoldCheckpoint(id)
	switch {
	case err == nil:
		fc.aggVK, fc.batchVK = cp.AggVK, cp.BatchVK
		fc.lastFold, fc.lastFoldOn = cp.LastFoldJob, cp.FoldWorker
		fc.foldCount, fc.batchesFolded, fc.stateRoot = cp.FoldCount, cp.BatchesFolded, cp.StateRoot
		if cp.Version == 0 {
			fc.batchesFolded = sc.legacyBatchesFolded(id, cp.FoldCount, fc.foldEvery)
		}
	case !errors.Is(err, storage.ErrNotFound):
		return nil, fmt.Errorf("load fold checkpoint: %w", err)
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

// foldWorker returns the election's fold worker, pinning the least-loaded
// worker on first use. A pinned worker that is lost (see WorkerManager.Lost),
// or that failed (fc.resync), is replaced by the least-loaded other worker,
// or kept if there is no other one; the chain is then synced onto it again.
// The pin is persisted before anything is imported onto a new worker.
func (sc *Scheduler) foldWorker(id types.ElectionID, fc *foldChain) (*workers.Worker, error) {
	cur, pinned := sc.pool.GetWorker(fc.pin)
	if pinned && !fc.resync && !sc.pool.Lost(cur) {
		return cur, nil
	}
	w := sc.pool.LeastLoaded(fc.pin)
	if w == nil {
		if !pinned || sc.pool.Banned(cur) {
			return nil, errNoWorker
		}
		w = cur // the only worker left
	}
	fc.synced = false
	if w.Address == fc.pin {
		return w, nil
	}
	if err := sc.store.UpdateElection(id, func(el *types.Election) error {
		el.FoldWorker = w.Address
		return nil
	}); err != nil {
		return nil, fmt.Errorf("persist fold worker: %w", err)
	}
	if fc.pin == "" {
		log.Infow("fold worker pinned", "election", id.String(), "worker", w.Address)
	} else {
		log.Warnw("fold chain moved", "election", id.String(), "from", fc.pin, "to", w.Address)
		sc.engine.audit("system", "system", "move_fold_chain:"+w.Address, id)
	}
	fc.pin = w.Address
	return w, nil
}

// sync puts the chain on its fold worker w: from the proofs in storage, it
// imports the last fold and the proved batches above it that w does not hold
// (all of them after a failure), so that fc.pending lists every proved,
// unfolded batch in seq order. Without the last fold's proof the chain
// starts again from genesis, proving the batches again if needed.
func (sc *Scheduler) sync(id types.ElectionID, fc *foldChain, w *workers.Worker) error {
	force := fc.resync
	if fc.lastFold != "" && (force || fc.lastFoldOn != w.Address) {
		proof, err := sc.store.FoldProof(id)
		switch {
		case errors.Is(err, storage.ErrNotFound):
			log.Warnw("no stored proof of the last fold, folding again from genesis",
				"election", id.String(), "foldCount", fc.foldCount)
			fc.lastFold, fc.lastFoldOn, fc.stateRoot = "", "", ""
			fc.foldCount, fc.batchesFolded = 0, 0
		case err != nil:
			return fmt.Errorf("load fold proof: %w", err)
		default:
			jobID, err := sc.importProof(w, proof, davinci.ImportFold)
			if err != nil {
				return fmt.Errorf("import last fold: %w", err)
			}
			fc.lastFold, fc.lastFoldOn = jobID, w.Address
			if err := sc.store.SetFoldCheckpoint(fc.checkpoint(id)); err != nil {
				return fmt.Errorf("persist checkpoint: %w", err)
			}
			log.Infow("last fold imported", "election", id.String(), "worker", w.Address,
				"foldJob", jobID, "foldCount", fc.foldCount)
		}
	}
	batches, err := sc.store.ListBatchInputs(id)
	if err != nil {
		return fmt.Errorf("list batches: %w", err)
	}
	fc.pending = nil
	for _, bi := range batches {
		if bi.Seq < fc.next() {
			continue
		}
		if bi.Seq != fc.next() {
			return fmt.Errorf("batch %d missing", fc.next())
		}
		if !force && bi.ImportedID != "" && bi.FoldWorker == w.Address {
			fc.pending = append(fc.pending, bi)
			continue
		}
		proof, err := sc.store.BatchProof(id, bi.Seq)
		if errors.Is(err, storage.ErrNotFound) {
			break // not proved yet
		}
		if err != nil {
			return fmt.Errorf("load proof of batch %d: %w", bi.Seq, err)
		}
		if err := sc.importBatch(id, fc, w, bi, proof); err != nil {
			return fmt.Errorf("batch %d: %w", bi.Seq, err)
		}
	}
	fc.synced, fc.resync = true, false
	return nil
}

// withFoldWorker runs op on the election's fold worker with the chain synced
// onto it. If the worker fails (errFoldWorker) after the retries on it, the
// chain moves to another worker, or is imported again onto the same one when
// no other is available, and op runs once more.
func (sc *Scheduler) withFoldWorker(id types.ElectionID, fc *foldChain, op func(w *workers.Worker) error) error {
	for attempt := 1; ; attempt++ {
		w, err := sc.foldWorker(id, fc)
		if err != nil {
			return err
		}
		if !fc.synced {
			err = sc.sync(id, fc, w)
		}
		if err == nil {
			err = op(w)
		}
		if !errors.Is(err, errFoldWorker) {
			return err
		}
		fc.resync = true
		if attempt == 2 || sc.ctx.Err() != nil {
			return err
		}
		log.Warnw("fold worker failed, moving the fold chain",
			"election", id.String(), "worker", w.Address, "error", err.Error())
	}
}

// Dispatch proves, imports and folds an election's sealed batches (see
// advance). It is idempotent, so it is safe to run again after a failure or
// a restart.
func (sc *Scheduler) Dispatch(id types.ElectionID) error {
	return sc.drive(id, false)
}

// Drain is Dispatch for an ended election: it also folds the batches left
// below the fold cadence.
func (sc *Scheduler) Drain(id types.ElectionID) error {
	return sc.drive(id, true)
}

func (sc *Scheduler) drive(id types.ElectionID, drain bool) error {
	m := sc.driveLock(id)
	m.Lock()
	defer m.Unlock()
	if sc.stopped(id) {
		return nil
	}
	fc, err := sc.chain(id)
	if err != nil {
		return err
	}
	return sc.advance(id, fc, drain)
}

// advance brings an election's fold chain up to its last sealed batch: in
// seq order, each batch is proved (or its stored proof reused), imported onto
// the fold worker and folded on the cadence; with drain, the batches left
// below the cadence are folded too. An election without batches has no
// chain yet. The caller holds the drive lock.
func (sc *Scheduler) advance(id types.ElectionID, fc *foldChain, drain bool) error {
	batches, err := sc.store.ListBatchInputs(id)
	if err != nil {
		return fmt.Errorf("list batches: %w", err)
	}
	if len(batches) == 0 {
		return nil
	}
	return sc.withFoldWorker(id, fc, func(w *workers.Worker) error {
		for _, bi := range batches {
			if len(fc.pending) >= fc.foldEvery {
				if err := sc.fold(id, fc, w); err != nil {
					return fmt.Errorf("fold: %w", err)
				}
			}
			if bi.Seq < fc.next() {
				continue
			}
			if bi.Seq != fc.next() {
				return fmt.Errorf("batch %d missing", fc.next())
			}
			if sc.stopped(id) {
				return nil
			}
			proof, err := sc.batchProof(id, fc, bi)
			if err != nil {
				return fmt.Errorf("batch %d: %w", bi.Seq, err)
			}
			if sc.stopped(id) {
				return nil
			}
			if err := sc.importBatch(id, fc, w, bi, proof); err != nil {
				return fmt.Errorf("batch %d: %w", bi.Seq, err)
			}
			log.Infow("batch gathered", "election", id.String(), "seq", bi.Seq,
				"worker", bi.Worker, "job", bi.JobID, "foldWorker", w.Address, "imported", bi.ImportedID)
		}
		if drain || len(fc.pending) >= fc.foldEvery {
			return sc.fold(id, fc, w)
		}
		return nil
	})
}

// batchProof returns a batch's proof.bin: the stored one; or else the one of
// the prove job a previous run left on its worker, if the worker still has
// it; or else the one of a new prove. A new proof is checked to be of the
// batch circuit and stored before it is returned, so the batch is never
// proved twice to the same end.
func (sc *Scheduler) batchProof(id types.ElectionID, fc *foldChain, bi *types.BatchInput) ([]byte, error) {
	proof, err := sc.store.BatchProof(id, bi.Seq)
	if err == nil {
		return proof, nil
	}
	if !errors.Is(err, storage.ErrNotFound) {
		return nil, fmt.Errorf("load proof: %w", err)
	}
	if !sc.store.IsBatchReserved(id, bi.Seq) {
		if err := sc.store.ReserveBatch(id, bi.Seq); err != nil {
			return nil, fmt.Errorf("reserve: %w", err)
		}
	}
	defer func() { _ = sc.store.ReleaseBatch(id, bi.Seq) }()

	if proof = sc.reusedProof(id, fc, bi); proof == nil {
		if proof, err = sc.prove(id, fc, bi); err != nil {
			return nil, err
		}
	}
	if sc.stopped(id) {
		return proof, nil
	}
	if err := sc.store.SetBatchProof(id, bi.Seq, proof); err != nil {
		return nil, fmt.Errorf("persist proof: %w", err)
	}
	return proof, nil
}

// reusedProof returns the proof of the prove job a previous run submitted
// for the batch, waiting for the job if it still runs. It returns nil, and
// the batch is proved again, if the worker or the job is gone, the job
// failed, or it is not a STARK of the batch circuit (its program vk), or if
// a proof of that worker was already dropped for the batch.
func (sc *Scheduler) reusedProof(id types.ElectionID, fc *foldChain, bi *types.BatchInput) []byte {
	if bi.JobID == "" || slices.Contains(fc.avoid[bi.Seq], bi.Worker) {
		return nil
	}
	w, ok := sc.pool.GetWorker(bi.Worker)
	if !ok {
		return nil
	}
	job, err := w.Client().GetJob(bi.JobID)
	if err != nil {
		return nil
	}
	switch job.Status {
	case davinci.JobStatusQueued, davinci.JobStatusRunning, davinci.JobStatusDone:
	default:
		return nil
	}
	if err := sc.waitJob(w, bi.JobID); err != nil {
		return nil
	}
	if err := sc.checkBatchVK(fc, w, bi.JobID); err != nil {
		log.Warnw("not reusing the batch's prove job", "election", id.String(), "seq", bi.Seq,
			"worker", w.Address, "job", bi.JobID, "error", err.Error())
		return nil
	}
	proof, err := w.Client().FetchStarkRaw(bi.JobID)
	if err != nil {
		return nil
	}
	log.Infow("reused the batch's prove job", "election", id.String(), "seq", bi.Seq,
		"worker", w.Address, "job", bi.JobID)
	return proof
}

// prove proves a batch on the least-loaded worker and returns its proof.bin,
// moving to another worker when a job fails, up to maxJobAttempts jobs. The
// workers whose proof of the batch was dropped are tried last. The worker
// and job are persisted when the job is submitted, so a restart can reuse it
// (see reusedProof).
func (sc *Scheduler) prove(id types.ElectionID, fc *foldChain, bi *types.BatchInput) ([]byte, error) {
	var req davinci.ProveRequest
	if err := json.Unmarshal(bi.ProveRequest, &req); err != nil {
		return nil, fmt.Errorf("decode prove request: %w", err)
	}
	req.Output = "stark"

	var lastErr error
	failed := slices.Clone(fc.avoid[bi.Seq])
	for attempt := 1; attempt <= maxJobAttempts; attempt++ {
		w := sc.pool.LeastLoaded(failed...)
		if w == nil {
			w = sc.pool.LeastLoaded()
		}
		if w == nil {
			if lastErr == nil {
				return nil, errNoWorker
			}
			return nil, lastErr
		}
		proof, err := sc.proveOn(id, fc, bi, w, &req)
		if err == nil {
			sc.pool.WorkerResult(w.Address, true)
			return proof, nil
		}
		lastErr = fmt.Errorf("prove on %s (attempt %d/%d): %w", w.Address, attempt, maxJobAttempts, err)
		if sc.ctx.Err() != nil {
			break
		}
		log.Warnw("prove failed", "election", id.String(), "seq", bi.Seq, "worker", w.Address,
			"attempt", attempt, "error", err.Error())
		sc.pool.WorkerResult(w.Address, false)
		failed = append(failed, w.Address)
	}
	return nil, lastErr
}

// proveOn runs one prove job of a batch on w and returns its proof.bin,
// failing if the proof is not of the batch circuit (a worker running
// another guest, as in a rolling upgrade).
func (sc *Scheduler) proveOn(id types.ElectionID, fc *foldChain, bi *types.BatchInput, w *workers.Worker, req *davinci.ProveRequest) ([]byte, error) {
	jobID, err := w.Client().SubmitProve(req)
	if err != nil {
		return nil, fmt.Errorf("submit: %w", err)
	}
	bi.Worker, bi.JobID = w.Address, jobID
	if err := sc.store.SetBatchInput(bi); err != nil {
		log.Warnw("failed to persist the batch's prove job",
			"election", id.String(), "seq", bi.Seq, "error", err.Error())
	}
	if err := sc.waitJob(w, jobID); err != nil {
		return nil, fmt.Errorf("job %s: %w", jobID, err)
	}
	if err := sc.checkBatchVK(fc, w, jobID); err != nil {
		return nil, fmt.Errorf("job %s: %w", jobID, err)
	}
	proof, err := w.Client().FetchStarkRaw(jobID)
	if err != nil {
		return nil, fmt.Errorf("fetch proof of job %s: %w", jobID, err)
	}
	return proof, nil
}

// checkBatchVK checks the STARK of job jobID on w has the batch circuit's
// program vk, learning it from the first batch if no release is pinned.
func (sc *Scheduler) checkBatchVK(fc *foldChain, w *workers.Worker, jobID string) error {
	info, err := w.Client().FetchStarkInfo(jobID)
	if err != nil {
		return fmt.Errorf("stark info: %w", err)
	}
	switch want := fc.wantBatchVK(); {
	case want == "":
		fc.batchVK = info.ProgramVK
	case !sameVK(info.ProgramVK, want):
		return fmt.Errorf("program vk %s is not the batch vk %s", info.ProgramVK, want)
	}
	return nil
}

// importBatch imports a proved batch onto the fold worker w, persists its
// imported job and adds it to the pending batches. A proof the fold worker
// reads as another circuit's (its program vk) is dropped, so the batch is
// proved again.
func (sc *Scheduler) importBatch(id types.ElectionID, fc *foldChain, w *workers.Worker, bi *types.BatchInput, proof []byte) error {
	jobID, err := sc.importProof(w, proof, davinci.ImportBatch)
	if err != nil {
		return err
	}
	var vk string
	if err := sc.call(w, "batch info", func(c *davinci.Client) error {
		info, err := c.FetchStarkInfo(jobID)
		if err == nil {
			vk = info.ProgramVK
		}
		return err
	}); err != nil {
		return err
	}
	if want := fc.wantBatchVK(); want != "" && !sameVK(vk, want) {
		sc.dropBatchProof(id, fc, bi)
		return fmt.Errorf("proof has program vk %s, not the batch vk %s: dropped", vk, want)
	}
	if fc.batchVK == "" {
		fc.batchVK = vk
	}
	bi.ImportedID, bi.FoldWorker = jobID, w.Address
	if err := sc.store.SetBatchInput(bi); err != nil {
		return fmt.Errorf("persist batch: %w", err)
	}
	fc.pending = append(fc.pending, bi)
	return nil
}

// importProof imports a proof.bin onto the fold worker w as a job of the
// given kind and returns the job ID.
func (sc *Scheduler) importProof(w *workers.Worker, proof []byte, kind davinci.ImportKind) (string, error) {
	var jobID string
	err := sc.call(w, "import "+string(kind), func(c *davinci.Client) error {
		var err error
		jobID, err = c.ImportStarkAs(proof, kind)
		return err
	})
	return jobID, err
}

// fold folds the pending batches into the chain on the fold worker w. The
// first fold runs a bootstrap pass to learn the aggregator program_vk, then
// the genesis fold binds it; later folds chain via PrevFoldJob. The fold's
// proof is stored with the checkpoint, and the proofs of the batches it
// folded are dropped. Adapts chain.Sequencer.Fold to the pool.
func (sc *Scheduler) fold(id types.ElectionID, fc *foldChain, w *workers.Worker) error {
	if len(fc.pending) == 0 || sc.stopped(id) {
		return nil
	}
	rt, ok := sc.engine.runtime(id)
	if !ok {
		return nil
	}
	// The chain config comes from the election's immutable parameters.
	chainCfg := *rt.current().ChainConfig()

	pending := fc.pending
	batchJobs := make([]string, len(pending))
	folded := make([]uint64, len(pending))
	for i, bi := range pending {
		batchJobs[i], folded[i] = bi.ImportedID, bi.Seq
	}

	// Bootstrap pass to learn the aggregator program_vk (the guest cannot know
	// its own vk).
	if fc.aggVK == "" {
		bootID, err := sc.runFold(w, &davinci.FoldRequest{Config: chainCfg, BatchJobs: batchJobs[:1]})
		if err != nil {
			sc.foldFailed(id, fc, w, err)
			return fmt.Errorf("bootstrap fold: %w", err)
		}
		vk, err := sc.foldVK(fc, w, bootID)
		if err != nil {
			return fmt.Errorf("bootstrap fold: %w", err)
		}
		fc.aggVK = vk
	}

	req := &davinci.FoldRequest{Config: chainCfg, BatchJobs: batchJobs}
	if fc.lastFold == "" {
		req.FoldVK = fc.aggVK // genesis fold binds the learned vk
	} else {
		req.PrevFoldJob = fc.lastFold
	}
	foldID, err := sc.runFold(w, req)
	if err != nil {
		sc.foldFailed(id, fc, w, err)
		return err
	}
	if _, err := sc.foldVK(fc, w, foldID); err != nil {
		return err
	}
	var proof []byte
	if err := sc.call(w, "fold proof", func(c *davinci.Client) error {
		var err error
		proof, err = c.FetchStarkRaw(foldID)
		return err
	}); err != nil {
		return err
	}

	cp := fc.checkpoint(id)
	cp.FoldCount++
	cp.LastFoldJob, cp.FoldWorker = foldID, w.Address
	cp.BatchesFolded += uint64(len(pending))
	cp.StateRoot = pending[len(pending)-1].NewStateRoot
	if err := sc.store.CommitFold(cp, proof, folded); err != nil {
		return fmt.Errorf("persist fold: %w", err)
	}
	fc.lastFold, fc.lastFoldOn = foldID, w.Address
	fc.foldCount, fc.batchesFolded, fc.stateRoot = cp.FoldCount, cp.BatchesFolded, cp.StateRoot
	fc.pending = nil
	fc.foldFailures, fc.reproved, fc.headSuspect, fc.avoid = 0, false, false, nil

	for _, bi := range pending {
		for _, voteID := range bi.VoteIDs {
			if err := sc.store.SetVoteStatus(id, voteID, types.VoteStatusFolded); err != nil {
				log.Warnw("failed to set vote folded", "vote", voteID.String(), "error", err.Error())
			}
		}
	}
	log.Infow("folded batches",
		"election", id.String(), "foldJob", foldID, "worker", w.Address,
		"foldCount", cp.FoldCount, "batches", len(pending), "batchesFolded", cp.BatchesFolded)
	return nil
}

// foldVK returns the program vk of fold job jobID on the fold worker w,
// failing the worker if it is not the aggregator's (another guest, as in a
// rolling upgrade).
func (sc *Scheduler) foldVK(fc *foldChain, w *workers.Worker, jobID string) (string, error) {
	var vk string
	if err := sc.call(w, "fold info", func(c *davinci.Client) error {
		info, err := c.FetchStarkInfo(jobID)
		if err == nil {
			vk = info.ProgramVK
		}
		return err
	}); err != nil {
		return "", err
	}
	if want := fc.wantAggVK(); want != "" && !sameVK(vk, want) {
		sc.pool.WorkerResult(w.Address, false)
		return "", fmt.Errorf("%w %s: fold job %s has program vk %s, not the aggregator vk %s",
			errFoldWorker, w.Address, jobID, vk, want)
	}
	return vk, nil
}

// foldFailed logs a failed fold with the batches it held and the chain head.
// When the fold jobs themselves keep failing on an unchanged head, it
// escalates: the first time, the pending batches' proofs are dropped, so
// they are proved again on other workers than the ones that made them, in
// case one is a proof the aggregator rejects; if folds of the new proofs
// keep failing too, the stored last fold proof is reported as suspect, once,
// and the folds go on being retried with backoff.
func (sc *Scheduler) foldFailed(id types.ElectionID, fc *foldChain, w *workers.Worker, err error) {
	if errors.Is(err, errJobFailed) {
		fc.foldFailures++
	}
	log.Warnw("fold failed", "election", id.String(), "worker", w.Address,
		"batches", fc.pendingSeqs(), "headFoldJob", fc.lastFold, "foldCount", fc.foldCount,
		"failures", fc.foldFailures, "error", err.Error())
	if fc.foldFailures < maxFoldFailures {
		return
	}
	fc.foldFailures = 0
	if !fc.reproved {
		fc.reproved = true
		log.Warnw("folds keep failing, proving the pending batches again on other workers",
			"election", id.String(), "batches", fc.pendingSeqs(), "headFoldJob", fc.lastFold)
		for _, bi := range fc.pending {
			sc.dropBatchProof(id, fc, bi)
		}
		fc.pending = nil
		return
	}
	if !fc.headSuspect {
		fc.headSuspect = true
		// ponytail: a suspect last fold is only reported. Folding again from
		// genesis (sync's path when the fold proof is missing), which proves
		// every folded batch again, is the upgrade once it is needed.
		log.Logger().Error().Str("election", id.String()).Str("headFoldJob", fc.lastFold).
			Uint64("foldCount", fc.foldCount).Uints64("batches", fc.pendingSeqs()).
			Msg("folds of freshly proved batches keep failing: the stored last fold proof is suspect")
	}
}

// dropBatchProof forgets a batch's proof and the jobs that made and imported
// it, so the batch is proved again, on another worker than the one that made
// it if there is one.
func (sc *Scheduler) dropBatchProof(id types.ElectionID, fc *foldChain, bi *types.BatchInput) {
	if bi.Worker != "" {
		if fc.avoid == nil {
			fc.avoid = make(map[uint64][]string)
		}
		fc.avoid[bi.Seq] = append(fc.avoid[bi.Seq], bi.Worker)
	}
	bi.Worker, bi.JobID, bi.ImportedID, bi.FoldWorker = "", "", "", ""
	if err := sc.store.ResetBatch(bi); err != nil {
		log.Warnw("failed to drop the batch's proof", "election", id.String(), "seq", bi.Seq, "error", err.Error())
	}
}

// runFold submits a fold to the fold worker w and waits for it (see runJob).
func (sc *Scheduler) runFold(w *workers.Worker, req *davinci.FoldRequest) (string, error) {
	return sc.runJob(w, "fold", func(c *davinci.Client) (string, error) { return c.SubmitFold(req) })
}

// runJob submits a job to the fold worker w and waits for it, submitting the
// identical request again when it fails, up to maxJobAttempts times. A
// failure of every attempt is errFoldWorker.
func (sc *Scheduler) runJob(w *workers.Worker, what string, submit func(*davinci.Client) (string, error)) (string, error) {
	var lastErr error
	for attempt := 1; attempt <= maxJobAttempts; attempt++ {
		if attempt > 1 {
			if err := sc.pause(); err != nil {
				return "", err
			}
		}
		jobID, err := submit(w.Client())
		if err == nil {
			if err = sc.waitJob(w, jobID); err == nil {
				sc.pool.WorkerResult(w.Address, true)
				return jobID, nil
			}
			lastErr = fmt.Errorf("%s job %s (attempt %d/%d): %w", what, jobID, attempt, maxJobAttempts, err)
		} else {
			lastErr = fmt.Errorf("submit %s (attempt %d/%d): %w", what, attempt, maxJobAttempts, err)
		}
		if sc.ctx.Err() != nil {
			return "", lastErr
		}
		sc.pool.WorkerResult(w.Address, false)
		if errors.Is(err, errWorkerRemoved) {
			break
		}
	}
	return "", fmt.Errorf("%w %s: %w", errFoldWorker, w.Address, lastErr)
}

// call runs a request against the fold worker w, trying up to maxJobAttempts
// times. A failure of every attempt is errFoldWorker.
func (sc *Scheduler) call(w *workers.Worker, what string, req func(*davinci.Client) error) error {
	var lastErr error
	for attempt := 1; attempt <= maxJobAttempts; attempt++ {
		if attempt > 1 {
			if err := sc.pause(); err != nil {
				return err
			}
		}
		if !sc.pool.Has(w) {
			lastErr = errWorkerRemoved
			break
		}
		if lastErr = req(w.Client()); lastErr == nil {
			return nil
		}
		sc.pool.WorkerResult(w.Address, false)
	}
	return fmt.Errorf("%w %s: %s: %w", errFoldWorker, w.Address, what, lastErr)
}

// pause waits one poll interval between two attempts on a worker, or returns
// the context error when the scheduler stops.
func (sc *Scheduler) pause() error {
	select {
	case <-sc.ctx.Done():
		return sc.ctx.Err()
	case <-time.After(sc.poll):
		return nil
	}
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
			return fmt.Errorf("%w: %s", errJobFailed, msg)
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
