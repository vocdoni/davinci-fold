package workers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
)

// healthServer is a fake Rust prover exposing GET /health with a settable queue
// length and reachability, so the manager's health poll can be exercised
// without a real worker.
type healthServer struct {
	srv      *httptest.Server
	queueLen int64 // atomic
	down     int32 // atomic bool
}

func newHealthServer(t *testing.T, queueLen int) *healthServer {
	t.Helper()
	hs := &healthServer{queueLen: int64(queueLen)}
	hs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.LoadInt32(&hs.down) == 1 {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":    "ok",
			"version":   "test",
			"queue_len": atomic.LoadInt64(&hs.queueLen),
		})
	}))
	t.Cleanup(hs.srv.Close)
	return hs
}

func (hs *healthServer) setDown(d bool) {
	var v int32
	if d {
		v = 1
	}
	atomic.StoreInt32(&hs.down, v)
}

func TestAddAndGetWorker(t *testing.T) {
	c := qt.New(t)
	wm := NewWorkerManager(nil)

	w := wm.AddWorker("http://a", "alpha")
	c.Assert(w, qt.IsNotNil)
	c.Assert(w.Address, qt.Equals, "http://a")
	c.Assert(w.Client(), qt.IsNotNil)

	// Re-adding returns the same instance.
	again := wm.AddWorker("http://a", "")
	c.Assert(again, qt.Equals, w)

	got, ok := wm.GetWorker("http://a")
	c.Assert(ok, qt.IsTrue)
	c.Assert(got, qt.Equals, w)

	_, ok = wm.GetWorker("http://missing")
	c.Assert(ok, qt.IsFalse)
}

func TestWorkerResultBanUnban(t *testing.T) {
	c := qt.New(t)
	rules := &WorkerBanRules{BanTimeout: 50 * time.Millisecond, FailuresToGetBanned: 2}
	wm := NewWorkerManager(rules)
	wm.AddWorker("http://a", "")

	// Below the threshold: not banned.
	wm.WorkerResult("http://a", false)
	wm.WorkerResult("http://a", false)
	w, _ := wm.GetWorker("http://a")
	c.Assert(w.IsBanned(rules), qt.IsFalse)

	// One more failure crosses FailuresToGetBanned (strict >): now banned.
	wm.WorkerResult("http://a", false)
	c.Assert(w.IsBanned(rules), qt.IsTrue)

	// The ticker policy assigns a ban window, then clears it once elapsed.
	wm.expireBans()
	c.Assert(w.GetBannedUntil().IsZero(), qt.IsFalse)
	time.Sleep(60 * time.Millisecond)
	wm.expireBans()
	c.Assert(w.IsBanned(rules), qt.IsFalse)

	// A success resets the consecutive-failure counter.
	wm.WorkerResult("http://a", false)
	wm.WorkerResult("http://a", true)
	c.Assert(w.IsBanned(rules), qt.IsFalse)
}

func TestLeastLoadedSkipsUnhealthyAndBanned(t *testing.T) {
	c := qt.New(t)
	hsA := newHealthServer(t, 5)
	hsB := newHealthServer(t, 1)
	hsC := newHealthServer(t, 0)

	rules := &WorkerBanRules{BanTimeout: time.Minute, FailuresToGetBanned: 0}
	wm := NewWorkerManager(rules, 10*time.Millisecond)
	wm.AddWorker(hsA.srv.URL, "a")
	wm.AddWorker(hsB.srv.URL, "b")
	wm.AddWorker(hsC.srv.URL, "c")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wm.Start(ctx)
	defer wm.Stop()

	// After a poll, C (queue 0) is least loaded.
	time.Sleep(30 * time.Millisecond)
	best := wm.LeastLoaded()
	c.Assert(best, qt.IsNotNil)
	c.Assert(best.Address, qt.Equals, hsC.srv.URL)

	// Take C down: B (queue 1) becomes least loaded after the next poll.
	hsC.setDown(true)
	time.Sleep(40 * time.Millisecond)
	best = wm.LeastLoaded()
	c.Assert(best, qt.IsNotNil)
	c.Assert(best.Address, qt.Equals, hsB.srv.URL)

	// Ban B: only A remains selectable.
	wm.WorkerResult(hsB.srv.URL, false) // FailuresToGetBanned=0, strict > => banned at 1
	best = wm.LeastLoaded()
	c.Assert(best, qt.IsNotNil)
	c.Assert(best.Address, qt.Equals, hsA.srv.URL)
}

func TestLeastLoadedNoneAvailable(t *testing.T) {
	c := qt.New(t)
	wm := NewWorkerManager(nil)
	// Added but never polled => not healthy => not selectable.
	wm.AddWorker("http://127.0.0.1:0", "")
	c.Assert(wm.LeastLoaded(), qt.IsNil)
}

func TestWorkerIDAndRemoval(t *testing.T) {
	c := qt.New(t)
	wm := NewWorkerManager(nil)

	w := wm.AddWorker("http://a", "alpha")
	c.Assert(w.ID, qt.Equals, WorkerID("http://a"))
	c.Assert(w.ID, qt.HasLen, 16)
	c.Assert(WorkerID("http://b"), qt.Not(qt.Equals), w.ID)
	c.Assert(w.Info(nil).ID, qt.Equals, w.ID)

	got, ok := wm.WorkerByID(w.ID)
	c.Assert(ok, qt.IsTrue)
	c.Assert(got, qt.Equals, w)
	c.Assert(wm.Has(w), qt.IsTrue)

	wm.RemoveWorker(w.Address)
	_, ok = wm.WorkerByID(w.ID)
	c.Assert(ok, qt.IsFalse)
	c.Assert(wm.Has(w), qt.IsFalse)

	// Registered again, it has the same ID but is another worker.
	again := wm.AddWorker("http://a", "alpha")
	c.Assert(again.ID, qt.Equals, w.ID)
	c.Assert(wm.Has(w), qt.IsFalse)
	c.Assert(wm.Has(again), qt.IsTrue)
}

// TestLeastLoadedExclude checks the excluded workers are left out.
func TestLeastLoadedExclude(t *testing.T) {
	c := qt.New(t)
	hsA := newHealthServer(t, 0)
	hsB := newHealthServer(t, 3)
	wm := NewWorkerManager(nil)
	wm.AddWorker(hsA.srv.URL, "a")
	wm.AddWorker(hsB.srv.URL, "b")
	wm.pollHealth()

	c.Assert(wm.LeastLoaded().Address, qt.Equals, hsA.srv.URL)
	c.Assert(wm.LeastLoaded(hsA.srv.URL).Address, qt.Equals, hsB.srv.URL)
	c.Assert(wm.LeastLoaded(hsA.srv.URL, hsB.srv.URL), qt.IsNil)
}

// TestLost checks a worker is lost once removed, banned, or unreachable for
// more health polls in a row than the ban rules allow failed jobs.
func TestLost(t *testing.T) {
	c := qt.New(t)
	hs := newHealthServer(t, 0)
	wm := NewWorkerManager(&WorkerBanRules{BanTimeout: time.Minute, FailuresToGetBanned: 2})
	w := wm.AddWorker(hs.srv.URL, "a")
	wm.pollHealth()
	c.Assert(wm.Lost(w), qt.IsFalse)

	hs.setDown(true)
	for range 2 {
		wm.pollHealth()
	}
	c.Assert(w.Healthy(), qt.IsFalse)
	c.Assert(wm.Lost(w), qt.IsFalse) // two missed polls are tolerated
	wm.pollHealth()
	c.Assert(wm.Lost(w), qt.IsTrue)
	hs.setDown(false)
	wm.pollHealth()
	c.Assert(wm.Lost(w), qt.IsFalse)

	for range 3 {
		wm.WorkerResult(w.Address, false)
	}
	c.Assert(wm.Banned(w), qt.IsTrue)
	c.Assert(wm.Lost(w), qt.IsTrue)
	wm.ResetWorker(w.Address)
	c.Assert(wm.Lost(w), qt.IsFalse)

	wm.RemoveWorker(w.Address)
	c.Assert(wm.Lost(w), qt.IsTrue)
}

// TestJobClaims checks a worker runs one claimed job at a time: Acquire takes
// idle workers only, a Claim waits for the worker's job and goes before any
// Acquire, Hold counts a job already running, and Release frees the worker.
func TestJobClaims(t *testing.T) {
	c := qt.New(t)
	hsA := newHealthServer(t, 0)
	hsB := newHealthServer(t, 3)
	wm := NewWorkerManager(nil)
	a := wm.AddWorker(hsA.srv.URL, "a")
	b := wm.AddWorker(hsB.srv.URL, "b")
	wm.pollHealth()
	c.Assert(wm.Available(), qt.Equals, 2)

	c.Assert(wm.Acquire(), qt.Equals, a) // the least loaded
	c.Assert(wm.Acquire(), qt.Equals, b)
	c.Assert(wm.Acquire(), qt.IsNil)
	c.Assert(wm.LeastLoaded(), qt.Equals, a) // still a fold worker candidate

	// A Claim of a waits for a's job, and takes a before any Acquire.
	claimed := make(chan error, 1)
	go func() { claimed <- wm.Claim(context.Background(), a) }()
	waitClaimer(c, a)
	freed := wm.Freed()
	wm.Release(a)
	select {
	case <-freed:
	case <-time.After(5 * time.Second):
		c.Fatal("a release did not close Freed")
	}
	c.Assert(wm.Acquire(b.Address), qt.IsNil)
	c.Assert(receive(c, claimed), qt.IsNil)
	wm.Release(a)
	c.Assert(wm.Acquire(b.Address), qt.Equals, a)
	wm.Release(a)

	// A job left by a previous run keeps b busy.
	wm.Release(b)
	wm.Hold(b)
	c.Assert(wm.Acquire(a.Address), qt.IsNil)
	wm.Release(b)
	c.Assert(wm.Acquire(a.Address), qt.Equals, b)

	// A Claim gives up when the worker is removed or the context ends.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.Assert(wm.Claim(ctx, b), qt.ErrorIs, context.Canceled)
	go func() { claimed <- wm.Claim(context.Background(), b) }()
	waitClaimer(c, b)
	wm.RemoveWorker(b.Address)
	c.Assert(receive(c, claimed), qt.ErrorIs, ErrWorkerRemoved)
	c.Assert(wm.Available(), qt.Equals, 1)
}

// waitClaimer waits for a Claim to wait for w, failing after five seconds.
func waitClaimer(c *qt.C, w *Worker) {
	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt64(&w.claimers) == 0 {
		if time.Now().After(deadline) {
			c.Fatal("no claim is waiting")
		}
		time.Sleep(time.Millisecond)
	}
}

// receive returns the next error from ch, failing after five seconds.
func receive(c *qt.C, ch <-chan error) error {
	select {
	case err := <-ch:
		return err
	case <-time.After(5 * time.Second):
		c.Fatal("timed out waiting for a claim")
		return nil
	}
}
