package orchestrator

import "github.com/vocdoni/davinci-fold/types"

// dispatcher is a per-election, single-flight driver. Notify coalesces seal
// events into a buffered trigger so at most one Dispatch runs per election at a
// time while never missing a newly sealed batch (a trigger that arrives mid-run
// is preserved and drives a follow-up pass).
type dispatcher struct {
	trigger chan struct{}
}

// Notify schedules a dispatch pass for an election, spawning its driver
// goroutine on first use. Safe for concurrent callers; non-blocking.
func (sc *Scheduler) Notify(id types.ElectionID) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.ctx.Err() != nil {
		return // stopping
	}
	disp, ok := sc.dispatchers[id.String()]
	if !ok {
		disp = &dispatcher{trigger: make(chan struct{}, 1)}
		sc.dispatchers[id.String()] = disp
		sc.wg.Add(1)
		go sc.dispatchLoop(id, disp)
	}
	select {
	case disp.trigger <- struct{}{}:
	default: // a pass is already pending; it will pick up the new batch
	}
}

// Forget drops what the scheduler keeps for an election that was canceled
// or has its results: its dispatch loop runs one more pass, which finds the
// election finished, and exits (see dispatchLoop).
func (sc *Scheduler) Forget(id types.ElectionID) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if disp, ok := sc.dispatchers[id.String()]; ok {
		select {
		case disp.trigger <- struct{}{}:
		default:
		}
		return
	}
	sc.forgetLocked(id)
}

// forgetLocked drops an election's fold chain and drive lock. No work runs
// for a finished election, so nothing holds them. The caller holds sc.mu.
func (sc *Scheduler) forgetLocked(id types.ElectionID) {
	delete(sc.chains, id.String())
	sc.driveMu.Delete(id.String())
}

// dispatchLoop drains an election's trigger channel and runs Dispatch until the
// scheduler context is canceled or the election is finished. A failed pass is
// run again by the engine's monitor, with backoff.
func (sc *Scheduler) dispatchLoop(id types.ElectionID, disp *dispatcher) {
	defer sc.wg.Done()
	for {
		select {
		case <-sc.ctx.Done():
			return
		case <-disp.trigger:
		}
		err := sc.Dispatch(id)
		switch {
		case sc.ctx.Err() != nil:
		case sc.finished(id):
			sc.engine.retries.forget(id)
			sc.mu.Lock()
			delete(sc.dispatchers, id.String())
			sc.forgetLocked(id)
			sc.mu.Unlock()
			return
		case err != nil:
			sc.engine.retries.failed(id, taskDispatch, err)
		default:
			sc.engine.retries.succeeded(id, taskDispatch)
		}
	}
}

// finished reports whether an election was canceled or has its results:
// nothing is dispatched for it any more.
func (sc *Scheduler) finished(id types.ElectionID) bool {
	el, err := sc.store.Election(id)
	return err == nil && (el.Status == types.StatusCanceled || el.Status == types.StatusResults)
}

// Stop cancels all dispatch loops and waits for them to exit.
func (sc *Scheduler) Stop() {
	sc.mu.Lock()
	sc.cancel()
	sc.mu.Unlock()
	sc.wg.Wait()
}
