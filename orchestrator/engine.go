// Package orchestrator owns the canonical election state: it builds genesis
// state trees, drives the election lifecycle state machine, ingests and
// de-duplicates self-authenticating votes, and seals batches (ApplyBatch +
// persisted prove inputs). The scatter/gather scheduler and fold-chain driver
// that dispatch those batches to the worker pool build on top of this engine.
package orchestrator

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"sync"
	"time"

	"github.com/vocdoni/davinci-fold/log"

	"github.com/vocdoni/davinci-fold/storage"
	"github.com/vocdoni/davinci-fold/types"
	"github.com/vocdoni/davinci-fold/workers"
	davinci "github.com/vocdoni/davinci-zkvm/go-sdk"
	"github.com/vocdoni/davinci-zkvm/go-sdk/chain"
	"github.com/vocdoni/davinci-zkvm/go-sdk/vocdoni/circuits/ballotproof"
)

// Default sealing parameters.
const (
	defaultBatchSize       = 64
	defaultFoldEvery       = 1
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
	// validator (census proof + ECDSA signature + Groth16 ballot proof + key
	// bindings).
	Validator Validator
	// Pool, when set, enables scatter/gather proving: sealed batches are
	// dispatched to the worker pool and folded on cadence. Nil leaves the
	// engine in ingest-only mode (batches are sealed and persisted but not
	// proved), which is what the unit tests exercise.
	Pool *workers.WorkerManager
	// FoldEvery is the fold cadence (batches per fold) of the elections that
	// do not set their own.
	FoldEvery int
	// JobTimeout bounds each prove, fold and finalize job.
	JobTimeout time.Duration
	// JobPoll is how often a running job is polled (5s if unset).
	JobPoll time.Duration
}

// Engine is the orchestrator's stateful core.
type Engine struct {
	store     *storage.Storage
	validator Validator
	batchSize int
	foldEvery int
	window    time.Duration

	ctx    context.Context
	cancel context.CancelFunc

	// pool and scheduler are nil in ingest-only mode (no worker pool
	// configured).
	pool      *workers.WorkerManager
	scheduler *Scheduler
	// finalize proves an election's results with its decryption key:
	// Scheduler.Finalize, nil in ingest-only mode.
	finalize func(id types.ElectionID, key *big.Int) (*types.Results, error)

	// draining guards the per-election Ended->Decrypting drive so the
	// once-per-second monitor never starts a second drain for the same election.
	draining sync.Map // electionID -> struct{}

	// finalizing guards the per-election Decrypting->Finalizing->Results drive so
	// two concurrent decryption-key submissions cannot both finalize (the second
	// would re-run the PLONK and its rollback-on-error could clobber Results).
	finalizing sync.Map // electionID -> struct{}

	// retries spaces the runs of failing background tasks.
	retries *retries

	// bg tracks the background goroutines (monitor, drains, finalizes) Stop
	// waits for; bgStopped refuses new ones once Stop started.
	bgMu      sync.Mutex
	bg        sync.WaitGroup
	bgStopped bool

	mu       sync.RWMutex
	runtimes map[string]*electionRuntime
}

// electionRuntime is the live, in-memory working set for one election. mu
// guards pending, batchSeq, dirty and state, which sealing mutates and may
// replace.
type electionRuntime struct {
	mu        sync.Mutex
	id        types.ElectionID
	cfg       chain.Config
	rules     *voteRules // what ingest checks this election's votes against
	state     *chain.State
	dirty     bool          // state changed since the persisted snapshot
	pending   []*types.Vote // buffered votes not yet sealed, in arrival order
	batchSeq  uint64        // next batch sequence number
	batchSize int           // per-election seal size (falls back to the engine default)
}

// current returns the election's state. Once the election stops sealing
// batches (it ended), the state no longer changes and can be read without
// rt.mu.
func (rt *electionRuntime) current() *chain.State {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.state
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
	if opts.FoldEvery <= 0 {
		opts.FoldEvery = defaultFoldEvery
	}
	if opts.BatchTimeWindow <= 0 {
		opts.BatchTimeWindow = defaultBatchTimeWindow
	}
	if opts.Validator == nil {
		opts.Validator = cryptoValidator{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &Engine{
		store:     store,
		validator: opts.Validator,
		batchSize: opts.BatchSize,
		foldEvery: opts.FoldEvery,
		window:    opts.BatchTimeWindow,
		ctx:       ctx,
		cancel:    cancel,
		retries:   newRetries(),
		runtimes:  make(map[string]*electionRuntime),
	}
	if opts.Pool != nil {
		e.pool = opts.Pool
		e.scheduler = NewScheduler(e, opts.Pool, opts.FoldEvery, opts.JobTimeout, opts.JobPoll)
		e.finalize = e.scheduler.Finalize
		if err := e.restoreWorkers(); err != nil {
			e.Stop()
			return nil, fmt.Errorf("restore workers: %w", err)
		}
	}
	if err := e.restore(); err != nil {
		e.Stop()
		return nil, fmt.Errorf("restore elections: %w", err)
	}
	e.goBackground(e.monitor)
	return e, nil
}

// Scheduler returns the engine's scatter/gather scheduler, or nil in
// ingest-only mode.
func (e *Engine) Scheduler() *Scheduler { return e.scheduler }

// Stop halts the lifecycle monitor, the dispatch loops and the drains and
// finalizes in flight, and waits for them to return.
func (e *Engine) Stop() {
	e.bgMu.Lock()
	e.bgStopped = true
	e.bgMu.Unlock()
	e.cancel()
	if e.scheduler != nil {
		e.scheduler.Stop()
	}
	e.bg.Wait()
}

// goBackground runs fn in a goroutine Stop waits for. It returns false, and
// does not run fn, once Stop started.
func (e *Engine) goBackground(fn func()) bool {
	e.bgMu.Lock()
	defer e.bgMu.Unlock()
	if e.bgStopped {
		return false
	}
	e.bg.Add(1)
	go func() {
		defer e.bg.Done()
		fn()
	}()
	return true
}

// restore rebuilds in-memory runtimes from persisted elections and snapshots.
func (e *Engine) restore() error {
	elections, err := e.store.ListElections()
	if err != nil {
		return err
	}
	for _, el := range elections {
		if el.Status == types.StatusCanceled || el.Status == types.StatusResults {
			e.deleteProofs(el.ID) // left by a stop right after the chain finished
			continue
		}
		if el.Status == types.StatusFinalizing {
			// The finalize died with the previous process; the keywarden
			// submits the key again.
			if err := e.store.UpdateElection(el.ID, func(stored *types.Election) error {
				stored.Status = types.StatusDecrypting
				stored.FinalizeError = errFinalizeInterrupted.Error()
				return nil
			}); err != nil {
				log.Warnw("failed to reset interrupted finalize", "election", el.ID.String(), "error", err.Error())
			}
			el.Status = types.StatusDecrypting
		}
		rt, err := e.runtimeFromStorage(el)
		if err != nil {
			log.Warnw("failed to restore election", "election", el.ID.String(), "error", err.Error())
			continue
		}
		e.runtimes[el.ID.String()] = rt
		log.Infow("restored election", "election", el.ID.String(), "status", el.Status.String(),
			"root", rt.state.Root(), "batches", rt.batchSeq, "pendingVotes", len(rt.pending))
		// Resume the fold chain at the first monitor tick, once the pool has
		// polled the provers; an ended election resumes its drain there.
		if el.Status != types.StatusEnded {
			e.retries.schedule(el.ID, taskDispatch)
		}
	}
	return nil
}

// restoreWorkers adds the stored worker registrations to the pool.
func (e *Engine) restoreWorkers() error {
	regs, err := e.store.ListWorkers()
	if err != nil {
		return err
	}
	for _, r := range regs {
		e.pool.AddWorker(r.Address, r.Name)
	}
	if len(regs) > 0 {
		log.Infow("restored workers", "count", len(regs))
	}
	return nil
}

// runtimeFromStorage restores a single election's State from its latest
// snapshot, recomputes the next batch sequence from persisted batches and
// reloads the accepted votes no batch holds into the pending buffer.
func (e *Engine) runtimeFromStorage(el *types.Election) (*electionRuntime, error) {
	rules, err := newVoteRules(el.Config)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	cfg := rules.cfg
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
	pending, err := e.unsealedVotes(el.ID, batches)
	if err != nil {
		return nil, fmt.Errorf("pending votes: %w", err)
	}
	bs := el.BatchSize
	if bs <= 0 {
		bs = e.batchSize
	}
	return &electionRuntime{
		id:        el.ID,
		cfg:       cfg,
		rules:     rules,
		state:     state,
		pending:   pending,
		batchSeq:  uint64(len(batches)),
		batchSize: bs,
	}, nil
}

// unsealedVotes returns the accepted votes of an election that no batch
// holds, in submission order: the pending buffer of the previous run. A
// vote's status moves from pending in the same write that seals its batch;
// the batches are checked too.
func (e *Engine) unsealedVotes(id types.ElectionID, batches []*types.BatchInput) ([]*types.Vote, error) {
	votes, err := e.store.VotesWithStatus(id, types.VoteStatusPending)
	if err != nil {
		return nil, err
	}
	sealed := make(map[string]struct{})
	for _, bi := range batches {
		for _, voteID := range bi.VoteIDs {
			sealed[string(voteID)] = struct{}{}
		}
	}
	pending := slices.DeleteFunc(votes, func(v *types.Vote) bool {
		_, ok := sealed[string(v.ID)]
		return ok
	})
	slices.SortFunc(pending, func(a, b *types.Vote) int { return cmp.Compare(a.Seq, b.Seq) })
	return pending, nil
}

// CreateElection validates the config, builds the genesis state, persists the
// election as Active and its genesis snapshot, and registers the runtime. An
// election without a vk gets the davinci-circom ballot-proof key; the vk is
// stored re-encoded as the provers parse it (see parseBallotVK).
func (e *Engine) CreateElection(subject string, el *types.Election) error {
	if len(el.Config.VK) == 0 || string(el.Config.VK) == "null" {
		el.Config.VK = ballotproof.CircomVerificationKey
	}
	_, vk, err := parseBallotVK(el.Config.VK)
	if err != nil {
		return fmt.Errorf("invalid election config: vk: %w", err)
	}
	el.Config.VK = vk
	rules, err := newVoteRules(el.Config)
	if err != nil {
		return fmt.Errorf("invalid election config: %w", err)
	}
	cfg := rules.cfg
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
	if el.FoldEvery <= 0 {
		el.FoldEvery = e.foldEvery
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
	e.runtimes[el.ID.String()] = &electionRuntime{id: el.ID, cfg: cfg, rules: rules, state: state, batchSize: el.BatchSize}
	e.mu.Unlock()

	e.audit(subject, "admin", "create_election", el.ID)
	log.Infow("created election", "election", el.ID.String(), "root", state.Root(), "endTime", el.EndTime.Format(time.RFC3339))
	return nil
}

// Election returns the persisted election record.
func (e *Engine) Election(id types.ElectionID) (*types.Election, error) {
	el, err := e.store.Election(id)
	if err != nil {
		return nil, err
	}
	return e.withDefaults(el), nil
}

// ListElections returns all persisted elections.
func (e *Engine) ListElections() ([]*types.Election, error) {
	els, err := e.store.ListElections()
	if err != nil {
		return nil, err
	}
	for _, el := range els {
		e.withDefaults(el)
	}
	return els, nil
}

// withDefaults sets the fold cadence of an election stored without one (it
// uses the engine default).
func (e *Engine) withDefaults(el *types.Election) *types.Election {
	if el.FoldEvery <= 0 {
		el.FoldEvery = e.foldEvery
	}
	return el
}

// runtime returns the live runtime for an election, if loaded.
func (e *Engine) runtime(id types.ElectionID) (*electionRuntime, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	rt, ok := e.runtimes[id.String()]
	return rt, ok
}

// RegisterWorker adds the prover at address to the pool and stores the
// registration, so the pool is rebuilt on restart. Registering a known
// address returns its worker, with the name filled in if it had none.
func (e *Engine) RegisterWorker(subject, address, name string) (*workers.Worker, error) {
	if e.pool == nil {
		return nil, fmt.Errorf("no worker pool configured")
	}
	_, known := e.pool.GetWorker(address)
	w := e.pool.AddWorker(address, name)
	if err := e.store.SetWorker(&types.WorkerRegistration{Address: w.Address, Name: w.Name}); err != nil {
		if !known {
			e.pool.RemoveWorker(address)
		}
		return nil, fmt.Errorf("persist worker: %w", err)
	}
	e.audit(subject, "admin", "register_worker:"+address, types.ElectionID{})
	return w, nil
}

// RemoveWorker takes the worker with the given ID out of the pool and
// deletes its registration. Its running jobs count as failed and go to other
// workers; the fold chains pinned to it move to other workers.
func (e *Engine) RemoveWorker(subject, workerID string) error {
	if e.pool == nil {
		return fmt.Errorf("%w: %s", ErrWorkerNotFound, workerID)
	}
	w, ok := e.pool.WorkerByID(workerID)
	if !ok {
		return fmt.Errorf("%w: %s", ErrWorkerNotFound, workerID)
	}
	if err := e.store.DeleteWorker(w.Address); err != nil {
		return fmt.Errorf("delete worker: %w", err)
	}
	e.pool.RemoveWorker(w.Address)
	e.audit(subject, "admin", "remove_worker:"+w.Address, types.ElectionID{})
	e.moveFoldChains(w.Address)
	return nil
}

// moveFoldChains starts a dispatch pass for every open election whose fold
// chain is pinned to the worker at address, which moves the chain.
func (e *Engine) moveFoldChains(address string) {
	e.mu.RLock()
	ids := make([]types.ElectionID, 0, len(e.runtimes))
	for _, rt := range e.runtimes {
		ids = append(ids, rt.id)
	}
	e.mu.RUnlock()
	for _, id := range ids {
		if el, err := e.store.Election(id); err == nil && el.FoldWorker == address {
			log.Infow("fold worker removed, moving the fold chain", "election", id.String(), "worker", address)
			e.scheduler.Notify(id)
		}
	}
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

// monitor periodically advances the lifecycle: it seals stale partial
// batches, ends the active and paused elections past their end time, drains
// the ended ones, and runs failed tasks again once their backoff has passed.
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
		case types.StatusActive, types.StatusPaused:
			if !el.EndTime.IsZero() && time.Now().After(el.EndTime) {
				e.endElection(rt)
				continue
			}
			e.sealIfStale(rt)
			e.retryDispatch(rt.id)
		case types.StatusEnded:
			// Drain the fold chain and publish the encrypted results. Runs in a
			// guarded goroutine so the GPU-bound fold work never stalls the sweep.
			if e.retries.due(rt.id, taskDrain) {
				e.goBackground(func() { e.drainAndPublish(rt) })
			}
		case types.StatusDecrypting:
			e.retryDispatch(rt.id)
		}
	}
}

// endElection ends an election past its end time, backing off if that
// fails.
func (e *Engine) endElection(rt *electionRuntime) {
	if !e.retries.due(rt.id, taskEnd) {
		return
	}
	err := e.changeStatus(rt, "system", "system", types.StatusEnded)
	switch {
	case err == nil, errors.Is(err, ErrInvalidTransition): // ended meanwhile
		e.retries.succeeded(rt.id, taskEnd)
	default:
		e.retries.failed(rt.id, taskEnd, err)
	}
}

// retryDispatch starts a dispatch pass for an election whose last pass
// failed, once its backoff has passed, or that was just restored.
func (e *Engine) retryDispatch(id types.ElectionID) {
	if e.scheduler != nil && e.retries.take(id, taskDispatch) {
		e.scheduler.Notify(id)
	}
}

// sealIfStale seals partial batches while the oldest pending vote has aged
// past the batch time window.
func (e *Engine) sealIfStale(rt *electionRuntime) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for len(rt.pending) > 0 && time.Since(rt.pending[0].SubmittedAt) >= e.window {
		if err := e.sealLocked(rt); err != nil {
			log.Warnw("failed to seal stale batch", "election", rt.id.String(), "error", err.Error())
			return
		}
	}
}
