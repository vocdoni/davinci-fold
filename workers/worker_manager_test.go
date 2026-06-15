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

func (hs *healthServer) setQueue(n int) { atomic.StoreInt64(&hs.queueLen, int64(n)) }
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
