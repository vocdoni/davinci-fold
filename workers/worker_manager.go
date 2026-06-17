// Package workers is the prover-worker registry: it tracks subscribed remote
// Rust prover services, health-polls them, applies ban/backoff on failure, and
// hands out go-sdk clients to the least-loaded healthy worker.
//
// It mirrors davinci-node's workers.WorkerManager (sync.Map of workers, atomic
// counters, a ticker-driven ban/unban loop) but is keyed by worker base URL and
// additionally owns a per-worker davinci.Client plus a health-polled queue
// length, so the scheduler can scatter batch proofs to the least-loaded worker.
package workers

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vocdoni/davinci-fold/log"
	davinci "github.com/vocdoni/davinci-zkvm/go-sdk"
)

// WorkerBanRules defines the rules for banning workers: how long a banned
// worker stays out and how many consecutive failures trigger a ban.
type WorkerBanRules struct {
	BanTimeout          time.Duration // duration for which the worker is banned
	FailuresToGetBanned int           // consecutive failed jobs before banning
}

// DefaultWorkerBanRules provides the default ban rules for workers.
var DefaultWorkerBanRules = &WorkerBanRules{
	BanTimeout:          30 * time.Minute,
	FailuresToGetBanned: 3,
}

// WorkerInfo is the public, serializable snapshot of a worker.
type WorkerInfo struct {
	Address      string `json:"address"`
	Name         string `json:"name"`
	Healthy      bool   `json:"healthy"`
	QueueLen     int    `json:"queueLen"`
	Banned       bool   `json:"banned"`
	SuccessCount int64  `json:"successCount"`
	FailedCount  int64  `json:"failedCount"`
}

// Worker is a remote Rust prover service. Counters are accessed atomically so a
// Worker is safe for concurrent use without an external lock.
type Worker struct {
	Address string // base URL, the map key
	Name    string

	client *davinci.Client

	consecutiveFails int64 // atomic
	bannedUntilNanos int64 // atomic Unix nanoseconds, 0 = not banned
	successCount     int64 // atomic
	failedCount      int64 // atomic
	queueLen         int64 // atomic, refreshed by the health poll
	healthy          int32 // atomic bool (1 = reachable at last poll)
}

// Client returns the worker's go-sdk client.
func (w *Worker) Client() *davinci.Client { return w.client }

// QueueLen returns the worker's last-polled queue length.
func (w *Worker) QueueLen() int { return int(atomic.LoadInt64(&w.queueLen)) }

// Healthy reports whether the worker answered the last health poll.
func (w *Worker) Healthy() bool { return atomic.LoadInt32(&w.healthy) == 1 }

// IsBanned checks if the worker is banned per the provided rules: either too
// many consecutive failures or still inside an active ban window.
func (w *Worker) IsBanned(rules *WorkerBanRules) bool {
	if rules == nil {
		return false
	}
	if atomic.LoadInt64(&w.consecutiveFails) > int64(rules.FailuresToGetBanned) {
		return true
	}
	bannedUntil := atomic.LoadInt64(&w.bannedUntilNanos)
	if bannedUntil == 0 {
		return false
	}
	return time.Now().UnixNano() < bannedUntil
}

// GetBannedUntil returns the ban expiration as a time.Time (zero if not banned).
func (w *Worker) GetBannedUntil() time.Time {
	nanos := atomic.LoadInt64(&w.bannedUntilNanos)
	if nanos == 0 {
		return time.Time{}
	}
	return time.Unix(0, nanos)
}

// SetBannedUntil sets the ban expiration time atomically.
func (w *Worker) SetBannedUntil(t time.Time) {
	var nanos int64
	if !t.IsZero() {
		nanos = t.UnixNano()
	}
	atomic.StoreInt64(&w.bannedUntilNanos, nanos)
}

// Info returns a serializable snapshot of the worker.
func (w *Worker) Info(rules *WorkerBanRules) *WorkerInfo {
	return &WorkerInfo{
		Address:      w.Address,
		Name:         w.Name,
		Healthy:      w.Healthy(),
		QueueLen:     w.QueueLen(),
		Banned:       w.IsBanned(rules),
		SuccessCount: atomic.LoadInt64(&w.successCount),
		FailedCount:  atomic.LoadInt64(&w.failedCount),
	}
}

// WorkerManager manages the worker pool: registration, health polling, and
// ban/backoff. A single ticker goroutine refreshes health and expires bans.
type WorkerManager struct {
	workers        sync.Map // address -> *Worker
	cancelFunc     context.CancelFunc
	rules          *WorkerBanRules
	tickerInterval time.Duration
	healthTimeout  time.Duration
}

// NewWorkerManager creates a worker manager with the given ban rules. An
// optional ticker interval may be supplied; it defaults to 10s.
func NewWorkerManager(rules *WorkerBanRules, tickerInterval ...time.Duration) *WorkerManager {
	interval := 10 * time.Second
	if len(tickerInterval) > 0 && tickerInterval[0] > 0 {
		interval = tickerInterval[0]
	}
	banRules := DefaultWorkerBanRules
	if rules != nil {
		banRules = rules
	}
	return &WorkerManager{
		rules:          banRules,
		tickerInterval: interval,
		healthTimeout:  5 * time.Second,
	}
}

// Start launches the background loop that health-polls workers and expires
// bans. It returns immediately; the loop stops when ctx is canceled or Stop is
// called.
func (wm *WorkerManager) Start(ctx context.Context) {
	ctx, wm.cancelFunc = context.WithCancel(ctx)
	wm.pollHealth() // prime queue lengths before the first tick
	go func() {
		ticker := time.NewTicker(wm.tickerInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				wm.pollHealth()
				wm.expireBans()
			}
		}
	}()
	log.Infow("worker manager started",
		"banTimeout", wm.rules.BanTimeout.String(),
		"failuresToGetBanned", wm.rules.FailuresToGetBanned,
		"tickerInterval", wm.tickerInterval.String())
}

// Stop cancels the background loop and clears the worker map.
func (wm *WorkerManager) Stop() {
	if wm.cancelFunc != nil {
		wm.cancelFunc()
	}
	wm.workers.Range(func(key, _ any) bool {
		wm.workers.Delete(key)
		return true
	})
}

// AddWorker registers a worker by base URL, constructing its go-sdk client. If
// the worker already exists it is returned unchanged (name filled in if empty).
func (wm *WorkerManager) AddWorker(address, name string) *Worker {
	if w, exists := wm.GetWorker(address); exists {
		if name != "" && w.Name == "" {
			w.Name = name
			wm.workers.Store(address, w)
		}
		return w
	}
	w := &Worker{
		Address: address,
		Name:    name,
		client:  davinci.NewClient(address),
	}
	wm.workers.Store(address, w)
	log.Debugw("worker added", "address", address, "name", name)
	return w
}

// RemoveWorker removes a worker from the pool.
func (wm *WorkerManager) RemoveWorker(address string) {
	wm.workers.Delete(address)
}

// GetWorker retrieves a worker by address.
func (wm *WorkerManager) GetWorker(address string) (*Worker, bool) {
	if w, ok := wm.workers.Load(address); ok {
		return w.(*Worker), true
	}
	return nil, false
}

// LeastLoaded returns the healthy, non-banned worker with the smallest polled
// queue length, or nil if none are available.
func (wm *WorkerManager) LeastLoaded() *Worker {
	var best *Worker
	bestQ := int(^uint(0) >> 1) // max int
	wm.workers.Range(func(_, value any) bool {
		w, ok := value.(*Worker)
		if !ok || !w.Healthy() || w.IsBanned(wm.rules) {
			return true
		}
		if q := w.QueueLen(); q < bestQ {
			best, bestQ = w, q
		}
		return true
	})
	return best
}

// BannedWorkers returns the currently banned workers.
func (wm *WorkerManager) BannedWorkers() []*Worker {
	var banned []*Worker
	wm.workers.Range(func(_, value any) bool {
		if w, ok := value.(*Worker); ok && w.IsBanned(wm.rules) {
			banned = append(banned, w)
		}
		return true
	})
	return banned
}

// SetBanDuration bans a worker for the configured ban timeout.
func (wm *WorkerManager) SetBanDuration(address string) {
	if w, ok := wm.GetWorker(address); ok {
		until := time.Now().Add(wm.rules.BanTimeout)
		w.SetBannedUntil(until)
		log.Warnw("worker banned", "address", address, "until", until.String())
	}
}

// ResetWorker clears a worker's failure counter and ban window, preserving its
// client and cumulative stats.
func (wm *WorkerManager) ResetWorker(address string) {
	if w, ok := wm.GetWorker(address); ok {
		atomic.StoreInt64(&w.consecutiveFails, 0)
		atomic.StoreInt64(&w.bannedUntilNanos, 0)
		log.Debugw("worker reset", "address", address)
	}
}

// WorkerResult records a job outcome: success zeroes the consecutive-failure
// counter, failure increments it (which may trip a ban on the next tick).
func (wm *WorkerManager) WorkerResult(address string, success bool) {
	w, ok := wm.GetWorker(address)
	if !ok {
		return
	}
	if success {
		atomic.StoreInt64(&w.consecutiveFails, 0)
		atomic.AddInt64(&w.successCount, 1)
	} else {
		atomic.AddInt64(&w.consecutiveFails, 1)
		atomic.AddInt64(&w.failedCount, 1)
	}
}

// ListWorkerStats returns a snapshot of every registered worker.
func (wm *WorkerManager) ListWorkerStats() []*WorkerInfo {
	out := []*WorkerInfo{}
	wm.workers.Range(func(_, value any) bool {
		if w, ok := value.(*Worker); ok {
			out = append(out, w.Info(wm.rules))
		}
		return true
	})
	return out
}

// pollHealth refreshes each worker's reachability and queue length.
func (wm *WorkerManager) pollHealth() {
	wm.workers.Range(func(_, value any) bool {
		w, ok := value.(*Worker)
		if !ok {
			return true
		}
		h, err := w.client.Health()
		if err != nil || h == nil {
			atomic.StoreInt32(&w.healthy, 0)
			return true
		}
		atomic.StoreInt32(&w.healthy, 1)
		atomic.StoreInt64(&w.queueLen, int64(h.QueueLen))
		return true
	})
}

// expireBans bans workers that crossed the failure threshold and unbans those
// whose ban window has elapsed (the davinci-node ticker policy).
func (wm *WorkerManager) expireBans() {
	for _, w := range wm.BannedWorkers() {
		bannedUntil := w.GetBannedUntil()
		switch {
		case bannedUntil.IsZero():
			wm.SetBanDuration(w.Address)
		case time.Now().After(bannedUntil):
			wm.ResetWorker(w.Address)
		}
	}
}
