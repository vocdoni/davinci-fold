// Package orchestrator owns the canonical election state: it builds genesis
// state trees, drives the election lifecycle state machine, ingests and
// de-duplicates self-authenticating votes, and seals batches (ApplyBatch +
// persisted prove inputs). The scatter/gather scheduler and fold-chain driver
// that dispatch those batches to the worker pool build on top of this engine.
package orchestrator

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/vocdoni/davinci-fold/log"

	"github.com/vocdoni/davinci-fold/storage"
	"github.com/vocdoni/davinci-fold/types"
	"github.com/vocdoni/davinci-fold/workers"
	davinci "github.com/vocdoni/davinci-zkvm/go-sdk"
	"github.com/vocdoni/davinci-zkvm/go-sdk/chain"
)

// Default sealing parameters.
const (
	defaultBatchSize       = 64
	defaultBatchTimeWindow = 30 * time.Second
	monitorInterval        = time.Second
)

// Options configures the engine.
type Options struct {
	// BatchSize seals a batch once this many pending votes accumulate.
	BatchSize int
	// BatchTimeWindow seals a partial batch when its oldest vote is older
	// than this, so low-traffic elections still make progress.
	BatchTimeWindow time.Duration
	// Validator gates submissions; nil installs the full cryptographic
	// validator (ECDSA signature + Groth16 ballot proof + key bindings).
	Validator Validator
	// Pool, when set, enables scatter/gather proving: sealed batches are
	// dispatched to the worker pool and folded on cadence. Nil leaves the
	// engine in ingest-only mode (batches are sealed and persisted but not
	// proved), which is what the unit tests exercise.
	Pool *workers.WorkerManager
	// FoldEvery is the fold cadence handed to the scheduler (batches per fold).
	FoldEvery int
	// JobTimeout bounds each prove/fold WaitForJob.
	JobTimeout time.Duration
}

// Engine is the orchestrator's stateful core.
type Engine struct {
	store     *storage.Storage
	validator Validator
	batchSize int
	window    time.Duration

	ctx    context.Context
	cancel context.CancelFunc

	// scheduler is nil in ingest-only mode (no worker pool configured).
	scheduler *Scheduler

	// draining guards the per-election Ended->Decrypting drive so the
	// once-per-second monitor never starts a second drain for the same election.
	draining sync.Map // electionID -> struct{}

	// finalizing guards the per-election Decrypting->Finalizing->Results drive so
	// two concurrent decryption-key submissions cannot both finalize (the second
	// would re-run the PLONK and its rollback-on-error could clobber Results).
	finalizing sync.Map // electionID -> struct{}

	mu       sync.RWMutex
	runtimes map[string]*electionRuntime
}

// electionRuntime is the live, in-memory working set for one election.
type electionRuntime struct {
	mu        sync.Mutex
	id        types.ElectionID
	cfg       chain.Config
	state     *chain.State
	pending   []*types.Vote // buffered votes not yet sealed, in arrival order
	batchSeq  uint64        // next batch sequence number
	batchSize int           // per-election seal size (falls back to the engine default)
}

// NewEngine builds an engine over store, restores any persisted elections into
// memory, and starts the lifecycle monitor.
func NewEngine(store *storage.Storage, opts Options) (*Engine, error) {
	if store == nil {
		return nil, fmt.Errorf("nil storage")
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = defaultBatchSize
	}
	if opts.BatchTimeWindow <= 0 {
		opts.BatchTimeWindow = defaultBatchTimeWindow
	}
	if opts.Validator == nil {
		opts.Validator = newCryptoValidator()
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &Engine{
		store:     store,
		validator: opts.Validator,
		batchSize: opts.BatchSize,
		window:    opts.BatchTimeWindow,
		ctx:       ctx,
		cancel:    cancel,
		runtimes:  make(map[string]*electionRuntime),
	}
	if opts.Pool != nil {
		e.scheduler = NewScheduler(e, opts.Pool, opts.FoldEvery, opts.JobTimeout)
	}
	if err := e.restore(); err != nil {
		cancel()
		return nil, fmt.Errorf("restore elections: %w", err)
	}
	go e.monitor()
	return e, nil
}

// Scheduler returns the engine's scatter/gather scheduler, or nil in
// ingest-only mode.
func (e *Engine) Scheduler() *Scheduler { return e.scheduler }

// Stop halts the lifecycle monitor and any dispatch loops.
func (e *Engine) Stop() {
	e.cancel()
	if e.scheduler != nil {
		e.scheduler.Stop()
	}
}

// restore rebuilds in-memory runtimes from persisted elections and snapshots.
func (e *Engine) restore() error {
	elections, err := e.store.ListElections()
	if err != nil {
		return err
	}
	for _, el := range elections {
		if el.Status == types.StatusCanceled || el.Status == types.StatusResults {
			continue
		}
		rt, err := e.runtimeFromStorage(el)
		if err != nil {
			log.Warnw("failed to restore election", "election", el.ID.String(), "error", err.Error())
			continue
		}
		e.runtimes[el.ID.String()] = rt
		log.Infow("restored election", "election", el.ID.String(), "status", el.Status.String(), "root", rt.state.Root())
		// Resume proving any persisted-but-undispatched batches.
		if e.scheduler != nil {
			e.scheduler.Notify(el.ID)
		}
	}
	return nil
}

// runtimeFromStorage restores a single election's State from its latest
// snapshot and recomputes the next batch sequence from persisted batches.
func (e *Engine) runtimeFromStorage(el *types.Election) (*electionRuntime, error) {
	cfg, err := chainConfigFromElection(el.Config)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	blob, err := e.store.Snapshot(el.ID)
	if err != nil {
		return nil, fmt.Errorf("snapshot: %w", err)
	}
	state, err := chain.RestoreState(cfg, blob)
	if err != nil {
		return nil, fmt.Errorf("restore state: %w", err)
	}
	batches, err := e.store.ListBatchInputs(el.ID)
	if err != nil {
		return nil, fmt.Errorf("list batches: %w", err)
	}
	bs := el.BatchSize
	if bs <= 0 {
		bs = e.batchSize
	}
	return &electionRuntime{
		id:        el.ID,
		cfg:       cfg,
		state:     state,
		batchSeq:  uint64(len(batches)),
		batchSize: bs,
	}, nil
}

// CreateElection validates the config, builds the genesis state, persists the
// election as Active and its genesis snapshot, and registers the runtime.
func (e *Engine) CreateElection(subject string, el *types.Election) error {
	cfg, err := chainConfigFromElection(el.Config)
	if err != nil {
		return fmt.Errorf("invalid election config: %w", err)
	}
	state, err := chain.NewState(cfg)
	if err != nil {
		return fmt.Errorf("genesis state: %w", err)
	}
	snapshot, err := state.Snapshot()
	if err != nil {
		return fmt.Errorf("genesis snapshot: %w", err)
	}

	if el.BatchSize <= 0 {
		el.BatchSize = e.batchSize
	}
	if el.BatchSize > davinci.MaxBatchSize {
		return fmt.Errorf("batch size %d exceeds circuit maximum %d", el.BatchSize, davinci.MaxBatchSize)
	}
	el.Status = types.StatusActive
	if err := e.store.CreateElection(el); err != nil {
		return err
	}
	if err := e.store.SetSnapshot(el.ID, snapshot); err != nil {
		return fmt.Errorf("persist snapshot: %w", err)
	}
	e.mu.Lock()
	e.runtimes[el.ID.String()] = &electionRuntime{id: el.ID, cfg: cfg, state: state, batchSize: el.BatchSize}
	e.mu.Unlock()

	e.audit(subject, "admin", "create_election", el.ID)
	log.Infow("created election", "election", el.ID.String(), "root", state.Root(), "endTime", el.EndTime)
	return nil
}

// Election returns the persisted election record.
func (e *Engine) Election(id types.ElectionID) (*types.Election, error) {
	return e.store.Election(id)
}

// ListElections returns all persisted elections.
func (e *Engine) ListElections() ([]*types.Election, error) {
	return e.store.ListElections()
}

// runtime returns the live runtime for an election, if loaded.
func (e *Engine) runtime(id types.ElectionID) (*electionRuntime, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	rt, ok := e.runtimes[id.String()]
	return rt, ok
}

// AuditWorkerRegister records an admin worker-registration action. Worker
// registration is not scoped to an election, so the election ID is empty.
func (e *Engine) AuditWorkerRegister(subject, address string) {
	e.audit(subject, "admin", "register_worker:"+address, types.ElectionID{})
}

// audit appends an accountability record, logging on failure.
func (e *Engine) audit(subject, role, action string, id types.ElectionID) {
	if err := e.store.AppendAudit(&types.AuditRecord{
		Subject:    subject,
		Role:       role,
		Action:     action,
		ElectionID: id,
		Timestamp:  time.Now(),
	}); err != nil {
		log.Warnw("failed to append audit record", "action", action, "error", err.Error())
	}
}

// monitor periodically advances the lifecycle: it seals stale partial batches
// and moves Active elections past their end time to Ended.
func (e *Engine) monitor() {
	ticker := time.NewTicker(monitorInterval)
	defer ticker.Stop()
	for {
		select {
		case <-e.ctx.Done():
			return
		case <-ticker.C:
			e.tick()
		}
	}
}

// tick runs one lifecycle sweep over all loaded elections.
func (e *Engine) tick() {
	e.mu.RLock()
	rts := make([]*electionRuntime, 0, len(e.runtimes))
	for _, rt := range e.runtimes {
		rts = append(rts, rt)
	}
	e.mu.RUnlock()

	for _, rt := range rts {
		el, err := e.store.Election(rt.id)
		if err != nil {
			continue
		}
		switch el.Status {
		case types.StatusActive:
			if !el.EndTime.IsZero() && time.Now().After(el.EndTime) {
				e.endElection(rt, el)
				continue
			}
			e.sealIfStale(rt)
		case types.StatusEnded:
			// Drain the fold chain and publish the encrypted results. Runs in a
			// guarded goroutine so the GPU-bound fold work never stalls the sweep.
			go e.drainAndPublish(rt)
		}
	}
}

// sealIfStale seals a partial batch whose oldest pending vote has aged past the
// batch time window.
func (e *Engine) sealIfStale(rt *electionRuntime) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.pending) == 0 {
		return
	}
	if time.Since(rt.pending[0].SubmittedAt) < e.window {
		return
	}
	if err := e.sealLocked(rt); err != nil {
		log.Warnw("failed to seal stale batch", "election", rt.id.String(), "error", err.Error())
	}
}

// endElection seals any remaining votes and transitions the election to Ended.
func (e *Engine) endElection(rt *electionRuntime, el *types.Election) {
	rt.mu.Lock()
	if len(rt.pending) > 0 {
		if err := e.sealLocked(rt); err != nil {
			log.Warnw("failed to seal final batch", "election", rt.id.String(), "error", err.Error())
			rt.mu.Unlock()
			return
		}
	}
	rt.mu.Unlock()

	if err := e.store.SetElectionStatus(rt.id, types.StatusEnded); err != nil {
		log.Warnw("failed to set election ended", "election", rt.id.String(), "error", err.Error())
		return
	}
	e.audit("system", "system", "end_election", rt.id)
	log.Infow("election ended", "election", rt.id.String(), "root", rt.state.Root())
}
