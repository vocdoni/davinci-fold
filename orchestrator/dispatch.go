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
	key := id.String()
	d, loaded := sc.dispatchers.LoadOrStore(key, &dispatcher{trigger: make(chan struct{}, 1)})
	disp := d.(*dispatcher)
	if !loaded {
		sc.wg.Add(1)
		go sc.dispatchLoop(id, disp)
	}
	select {
	case disp.trigger <- struct{}{}:
	default: // a pass is already pending; it will pick up the new batch
	}
}

// dispatchLoop drains an election's trigger channel and runs Dispatch until the
// scheduler context is canceled. A failed pass is run again by the engine's
// monitor, with backoff.
func (sc *Scheduler) dispatchLoop(id types.ElectionID, disp *dispatcher) {
	defer sc.wg.Done()
	for {
		select {
		case <-sc.ctx.Done():
			return
		case <-disp.trigger:
			err := sc.Dispatch(id)
			switch {
			case sc.ctx.Err() != nil:
			case err != nil:
				sc.engine.retries.failed(id, taskDispatch, err)
			default:
				sc.engine.retries.succeeded(id, taskDispatch)
			}
		}
	}
}

// Stop cancels all dispatch loops and waits for them to exit.
func (sc *Scheduler) Stop() {
	if sc.cancel != nil {
		sc.cancel()
	}
	sc.wg.Wait()
}
