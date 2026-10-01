package orchestrator

import (
	"sync"
	"time"

	"github.com/vocdoni/davinci-fold/log"
	"github.com/vocdoni/davinci-fold/types"
)

// Background tasks of an election that the monitor runs again after they
// fail.
const (
	taskEnd      = "end"      // end an election past its end time
	taskDispatch = "dispatch" // prove, import and fold the sealed batches
	taskDrain    = "drain"    // seal, prove and fold what an ended election has left
)

const (
	retryMinDelay = time.Second
	retryMaxDelay = 5 * time.Minute
	// retryErrorAfter is the number of consecutive failures of a task logged
	// as warnings before one is logged as an error; later ones are logged at
	// debug level.
	retryErrorAfter = 5
)

// retries tracks the failing background tasks of each election and when
// they may run again: the delay doubles with every consecutive failure, up
// to retryMaxDelay. A task that never failed is always due.
type retries struct {
	mu    sync.Mutex
	tasks map[string]*retryState // electionID/task
}

type retryState struct {
	failures int
	next     time.Time
}

func newRetries() *retries {
	return &retries{tasks: make(map[string]*retryState)}
}

func retryKey(id types.ElectionID, task string) string { return id.String() + "/" + task }

// retryDelay is the wait after the given number of consecutive failures.
func retryDelay(failures int) time.Duration {
	return min(retryMinDelay<<min(failures-1, 16), retryMaxDelay)
}

// due reports whether task may run now: it has not failed, or its delay has
// passed.
func (r *retries) due(id types.ElectionID, task string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.tasks[retryKey(id, task)]
	return st == nil || !time.Now().Before(st.next)
}

// take reports whether task is scheduled (it failed, or schedule asked for
// it) and due, and if so holds off the next run for another delay, so the run
// starting now is not doubled.
func (r *retries) take(id types.ElectionID, task string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.tasks[retryKey(id, task)]
	if st == nil || time.Now().Before(st.next) {
		return false
	}
	st.next = time.Now().Add(retryDelay(max(st.failures, 1)))
	return true
}

// schedule asks for task to run at the next take, without counting a failure.
func (r *retries) schedule(id types.ElectionID, task string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tasks[retryKey(id, task)]; !ok {
		r.tasks[retryKey(id, task)] = &retryState{}
	}
}

// failed records a failure of task and logs it: as a warning, then once as
// an error when it has failed retryErrorAfter times in a row, then at debug
// level.
func (r *retries) failed(id types.ElectionID, task string, err error) {
	r.mu.Lock()
	st := r.tasks[retryKey(id, task)]
	if st == nil {
		st = &retryState{}
		r.tasks[retryKey(id, task)] = st
	}
	st.failures++
	delay := retryDelay(st.failures)
	st.next = time.Now().Add(delay)
	failures := st.failures
	r.mu.Unlock()

	fields := []any{
		"election", id.String(), "task", task, "failures", failures,
		"retryIn", delay.String(), "error", err.Error(),
	}
	switch {
	case failures < retryErrorAfter:
		log.Warnw("election task failed", fields...)
	case failures == retryErrorAfter:
		log.Logger().Error().Fields(fields).Msg("election task keeps failing")
	default:
		log.Debugw("election task failed", fields...)
	}
}

// succeeded clears the failures of task.
func (r *retries) succeeded(id types.ElectionID, task string) {
	r.mu.Lock()
	st := r.tasks[retryKey(id, task)]
	delete(r.tasks, retryKey(id, task))
	r.mu.Unlock()
	if st != nil && st.failures >= retryErrorAfter {
		log.Infow("election task recovered", "election", id.String(), "task", task, "failures", st.failures)
	}
}

// forget drops the tasks of an election that is canceled or has its results.
func (r *retries) forget(id types.ElectionID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, task := range []string{taskEnd, taskDispatch, taskDrain} {
		delete(r.tasks, retryKey(id, task))
	}
}
