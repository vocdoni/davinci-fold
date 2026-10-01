package tests

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"

	"github.com/vocdoni/davinci-fold/api"
	"github.com/vocdoni/davinci-fold/orchestrator"
	"github.com/vocdoni/davinci-fold/storage"
	"github.com/vocdoni/davinci-fold/tests/helpers"
	"github.com/vocdoni/davinci-fold/types"
	"github.com/vocdoni/davinci-fold/workers"
	davinci "github.com/vocdoni/davinci-zkvm/go-sdk"
	"github.com/vocdoni/davinci-zkvm/go-sdk/tests/integration"
)

// The chaos tests run one election per test against fake provers (see
// helpers.FakeProver), with real ballots, and break the orchestrator or a
// prover at a chosen point. Each checks that the election still reaches its
// results, that every accepted vote is counted once, and that the fold
// chain behind the results is contiguous.

const (
	chaosVoters  = 6 // voters with a first ballot
	chaosRevotes = 2 // the first voters, voting again
)

// chaosBallots is the election the chaos tests share: each runs it on its
// own orchestrator and provers.
type chaosBallots struct {
	election *integration.Election
	votes    []*orchestrator.VoteSubmission // in submission order: first ballots, then the revotes
	tally    []uint64                       // the last ballot of each voter, summed
}

var (
	chaosOnce sync.Once
	chaosData *chaosBallots
	chaosErr  error
)

// chaosElection returns the shared election, generating its ballots on the
// first call.
func chaosElection(t *testing.T) *chaosBallots {
	t.Helper()
	chaosOnce.Do(func() { chaosData, chaosErr = newChaosBallots() })
	if chaosErr != nil {
		t.Fatalf("chaos ballots: %v", chaosErr)
	}
	return chaosData
}

func newChaosBallots() (*chaosBallots, error) {
	election, err := integration.NewElection(chaosVoters)
	if err != nil {
		return nil, err
	}
	census, err := election.BuildCensusProofs(election.Voters)
	if err != nil {
		return nil, err
	}
	first, err := integration.GenerateBallotBatch(election.ProcessID, election.EncKey, election.Voters, 42)
	if err != nil {
		return nil, err
	}
	again, err := integration.GenerateBallotBatch(election.ProcessID, election.EncKey, election.Voters[:chaosRevotes], 142)
	if err != nil {
		return nil, err
	}
	b := &chaosBallots{election: election, tally: make([]uint64, davinci.NumFields)}
	for i, v := range election.Voters {
		b.votes = append(b.votes, voteSubmission(v, first.Results[i], census[i]))
		seed := int64(42 + i)
		if i < chaosRevotes {
			seed = int64(142 + i)
		}
		for f, val := range ballotFields(seed) {
			b.tally[f] += uint64(val)
		}
	}
	for i, v := range election.Voters[:chaosRevotes] {
		b.votes = append(b.votes, voteSubmission(v, again.Results[i], census[i]))
	}
	return b, nil
}

// chaosStack is an orchestrator over a data directory it can be restarted
// from, running the shared election.
type chaosStack struct {
	t       *testing.T
	ctx     context.Context
	ballots *chaosBallots
	ledger  *helpers.Ledger
	dir     string
	opts    helpers.Options
	svc     *helpers.TestServices
	stop    func()
	id      string // election ID, hex
}

// newChaosStack starts an orchestrator that folds every foldEvery batches,
// with short job and health polls.
func newChaosStack(t *testing.T, foldEvery int) *chaosStack {
	s := &chaosStack{
		t:       t,
		ctx:     context.Background(),
		ballots: chaosElection(t),
		ledger:  helpers.NewLedger(),
		dir:     t.TempDir(),
		opts: helpers.Options{
			BatchSize:       2,
			BatchTimeWindow: time.Hour,
			FoldEvery:       foldEvery,
			JobTimeout:      time.Minute,
			JobPoll:         10 * time.Millisecond,
			WorkerPoll:      20 * time.Millisecond,
		},
	}
	s.id = hex.EncodeToString(s.ballots.election.ProcessID[:])
	s.start()
	t.Cleanup(func() {
		if s.stop != nil {
			s.stop()
		}
	})
	return s
}

func (s *chaosStack) start() {
	svc, stop, err := helpers.NewTestServices(s.ctx, s.dir, s.opts)
	qt.Assert(s.t, err, qt.IsNil)
	s.svc, s.stop = svc, stop
}

// restart stops the orchestrator where it is, as a crash would, and starts
// it again from its storage.
func (s *chaosStack) restart() {
	s.t.Log("restarting the orchestrator")
	s.stop()
	s.stop = nil
	s.start()
}

// prover starts a fake prover reporting queueLen queued jobs, registers it
// and waits for the pool to find it healthy.
func (s *chaosStack) prover(name string, queueLen int) *helpers.FakeProver {
	p := helpers.NewFakeProver(s.ledger)
	s.t.Cleanup(p.Kill)
	p.SetQueueLen(queueLen)
	qt.Assert(s.t, s.svc.Client.RegisterWorker(s.ctx, helpers.AdminToken(), p.URL, name), qt.IsNil)
	s.waitFor(name+" healthy", func() bool {
		w, ok := s.svc.Pool.GetWorker(p.URL)
		return ok && w.Healthy()
	})
	return p
}

// worker returns the pool's view of the prover at address.
func (s *chaosStack) worker(address string) *workers.WorkerInfo {
	list, err := s.svc.Client.ListWorkers(s.ctx)
	qt.Assert(s.t, err, qt.IsNil)
	i := slices.IndexFunc(list.Workers, func(w *workers.WorkerInfo) bool { return w.Address == address })
	qt.Assert(s.t, i >= 0, qt.IsTrue, qt.Commentf("worker %s", address))
	return list.Workers[i]
}

// create creates the election with the given batch size.
func (s *chaosStack) create(batchSize int) {
	req := electionRequest(s.t, s.ballots.election, batchSize, s.opts.FoldEvery, time.Time{})
	_, err := s.svc.Client.CreateElection(s.ctx, helpers.AdminToken(), req)
	qt.Assert(s.t, err, qt.IsNil)
}

// vote submits the shared votes from..to-1.
func (s *chaosStack) vote(from, to int) {
	for i := from; i < to; i++ {
		_, err := s.svc.Client.SubmitVote(s.ctx, s.id, s.ballots.votes[i])
		qt.Assert(s.t, err, qt.IsNil, qt.Commentf("vote %d", i))
	}
}

func (s *chaosStack) electionID() types.ElectionID {
	return types.ElectionID(s.ballots.election.ProcessID[:])
}

func (s *chaosStack) election() *api.ElectionResponse {
	el, err := s.svc.Client.GetElection(s.ctx, s.id)
	qt.Assert(s.t, err, qt.IsNil)
	return el
}

// foldCount returns the folds the stored checkpoint counts.
func (s *chaosStack) foldCount() uint64 {
	cp, err := s.svc.Storage.FoldCheckpoint(s.electionID())
	if err != nil {
		return 0
	}
	return cp.FoldCount
}

// batches returns the stored batches.
func (s *chaosStack) batches() []*types.BatchInput {
	bs, err := s.svc.Storage.ListBatchInputs(s.electionID())
	qt.Assert(s.t, err, qt.IsNil)
	return bs
}

// waitFor polls cond until it holds.
func (s *chaosStack) waitFor(what string, cond func() bool) {
	s.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			s.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (s *chaosStack) setStatus(status types.Status) {
	_, code, err := s.svc.Client.SetElectionStatus(s.ctx, helpers.AdminToken(), s.id, status.String())
	qt.Assert(s.t, err, qt.IsNil, qt.Commentf("status %d", code))
}

// submitKey hands the decryption key in.
func (s *chaosStack) submitKey() {
	_, code, err := s.svc.Client.SubmitDecryptionKey(s.ctx, helpers.KeywardenToken(), s.id, s.ballots.election.EncPrivKey)
	qt.Assert(s.t, err, qt.IsNil)
	qt.Assert(s.t, code, qt.Equals, http.StatusAccepted)
}

// finish ends the election if it is still open, waits for the encrypted
// results, hands the key in and waits for the results.
func (s *chaosStack) finish() *api.ResultsResponse {
	if st := s.election().Status; st == types.StatusActive.String() || st == types.StatusPaused.String() {
		s.setStatus(types.StatusEnded)
	}
	s.waitFor("decrypting", func() bool { return s.election().Status == types.StatusDecrypting.String() })
	s.submitKey()
	return s.results()
}

// results waits for the results of a finalize in progress.
func (s *chaosStack) results() *api.ResultsResponse {
	s.waitFor("results", func() bool {
		el := s.election()
		if el.Status == types.StatusDecrypting.String() && el.FinalizeError != "" {
			s.t.Fatalf("finalize failed: %s", el.FinalizeError)
		}
		return el.Status == types.StatusResults.String()
	})
	res, err := s.svc.Client.Results(s.ctx, s.id)
	qt.Assert(s.t, err, qt.IsNil)
	return res
}

// check asserts the results count every accepted vote once, that every vote
// is settled, that the fold chain behind the results folds every sealed
// batch once, in order, and that the proofs kept for moving the chain are
// gone.
func (s *chaosStack) check(res *api.ResultsResponse) {
	c := qt.New(s.t)
	c.Assert(res.Tally, qt.DeepEquals, s.ballots.tally)
	for i, v := range s.ballots.votes {
		st, err := s.svc.Client.Vote(s.ctx, s.id, hex.EncodeToString(v.VoteID))
		c.Assert(err, qt.IsNil)
		c.Assert(st.Status, qt.Equals, types.VoteStatusSettled.String(), qt.Commentf("vote %d", i))
	}

	folded, err := s.ledger.FinalChain()
	c.Assert(err, qt.IsNil)
	sealed := s.batches()
	c.Assert(len(folded), qt.Equals, len(sealed))
	var voters, overwrites uint64
	for i, b := range folded {
		c.Assert(b.NewRoot, qt.Equals, sealed[i].NewStateRoot, qt.Commentf("batch %d", i))
		voters += b.Voters
		overwrites += b.Overwrites
	}
	c.Assert(voters, qt.Equals, uint64(len(s.ballots.votes)))
	c.Assert(overwrites, qt.Equals, uint64(chaosRevotes))

	s.waitFor("the stored proofs dropped", func() bool {
		if _, err := s.svc.Storage.FoldProof(s.electionID()); !errors.Is(err, storage.ErrNotFound) {
			return false
		}
		for _, bi := range sealed {
			if _, err := s.svc.Storage.BatchProof(s.electionID(), bi.Seq); !errors.Is(err, storage.ErrNotFound) {
				return false
			}
		}
		return true
	})
}

// TestChaosRestartUnsealedVotes restarts the orchestrator with an accepted
// vote no batch holds yet: it is reloaded, still refused as a duplicate, and
// sealed in the next batch.
func TestChaosRestartUnsealedVotes(t *testing.T) {
	t.Parallel()
	c := qt.New(t)
	s := newChaosStack(t, 1)
	s.prover("p0", 0)
	s.create(2)

	s.vote(0, 3)
	s.waitFor("the first batch folded", func() bool { return s.foldCount() == 1 })
	s.restart()

	third := hex.EncodeToString(s.ballots.votes[2].VoteID)
	st, err := s.svc.Client.Vote(s.ctx, s.id, third)
	c.Assert(err, qt.IsNil)
	c.Assert(st.Status, qt.Equals, types.VoteStatusPending.String())
	_, err = s.svc.Client.SubmitVote(s.ctx, s.id, s.ballots.votes[2])
	c.Assert(err, qt.ErrorMatches, ".*status 409.*40012.*")

	s.vote(3, len(s.ballots.votes))
	s.check(s.finish())
	c.Assert(s.batches()[1].VoteIDs[0], qt.DeepEquals, types.VoteID(s.ballots.votes[2].VoteID))
}

// TestChaosRestartImportedBatches restarts the orchestrator with two batches
// imported on the fold worker but not folded (one fold every three): the
// chain is rebuilt with them, so the first fold folds three batches and no
// batch is proved or imported twice.
func TestChaosRestartImportedBatches(t *testing.T) {
	t.Parallel()
	c := qt.New(t)
	s := newChaosStack(t, 3)
	p := s.prover("p0", 0)
	s.create(2)

	s.vote(0, 4)
	s.waitFor("two batches imported", func() bool {
		bs := s.batches()
		return len(bs) == 2 && bs[0].ImportedID != "" && bs[1].ImportedID != ""
	})
	c.Assert(p.Count("fold"), qt.Equals, 0)
	s.restart()

	s.vote(4, len(s.ballots.votes))
	s.check(s.finish())
	c.Assert(p.Count("prove"), qt.Equals, 4)
	c.Assert(p.Count("import:batch"), qt.Equals, 4)
	// A bootstrap pass, the fold of three batches and the drain's fold of one.
	folds := p.Folds()
	c.Assert(len(folds), qt.Equals, 3)
	c.Assert(len(folds[1].BatchJobs), qt.Equals, 3)
	c.Assert(len(folds[2].BatchJobs), qt.Equals, 1)
}

// TestChaosRestartMidProve restarts the orchestrator while a batch is being
// proved: the restarted orchestrator waits for the same prove job instead
// of proving the batch again.
func TestChaosRestartMidProve(t *testing.T) {
	t.Parallel()
	c := qt.New(t)
	s := newChaosStack(t, 1)
	p := s.prover("p0", 0)
	s.create(2)

	p.Hold(helpers.JobBatch)
	s.vote(0, 2)
	s.waitFor("a prove running", func() bool { return p.Running(helpers.JobBatch) > 0 })
	s.restart()

	p.Release(helpers.JobBatch)
	s.vote(2, len(s.ballots.votes))
	s.check(s.finish())
	c.Assert(p.Count("prove"), qt.Equals, len(s.batches()))
}

// TestChaosRestartMidDrain restarts the orchestrator while an ended election
// is being drained, with a fold running: the drain resumes after the
// restart.
func TestChaosRestartMidDrain(t *testing.T) {
	t.Parallel()
	c := qt.New(t)
	s := newChaosStack(t, 1)
	p := s.prover("p0", 0)
	s.create(16) // nothing seals before the end

	p.Hold(helpers.JobFold)
	s.vote(0, len(s.ballots.votes))
	c.Assert(len(s.batches()), qt.Equals, 0)
	s.setStatus(types.StatusEnded)
	c.Assert(len(s.batches()), qt.Equals, 2)
	s.waitFor("a fold running", func() bool { return p.Running(helpers.JobFold) > 0 })
	s.restart()

	c.Assert(s.election().Status, qt.Equals, types.StatusEnded.String())
	p.Release(helpers.JobFold)
	s.check(s.finish())
}

// TestChaosRestartMidFinalize restarts the orchestrator while the final
// proof runs: the election goes back to decrypting, and the key submitted
// again finalizes it.
func TestChaosRestartMidFinalize(t *testing.T) {
	t.Parallel()
	c := qt.New(t)
	s := newChaosStack(t, 2)
	p := s.prover("p0", 0)
	s.create(2)

	s.vote(0, len(s.ballots.votes))
	s.setStatus(types.StatusEnded)
	s.waitFor("decrypting", func() bool { return s.election().Status == types.StatusDecrypting.String() })
	p.Hold(helpers.JobFinalize)
	s.submitKey()
	s.waitFor("the final proof running", func() bool { return p.Running(helpers.JobFinalize) > 0 })
	s.restart()

	el := s.election()
	c.Assert(el.Status, qt.Equals, types.StatusDecrypting.String())
	c.Assert(el.FinalizeError, qt.Equals, "finalize interrupted")
	p.Release(helpers.JobFinalize)
	s.submitKey()
	s.check(s.results())
}

// TestChaosFoldWorkerDeathBetweenFolds kills the fold worker after two
// folds: the chain moves to the other prover, which imports the last fold
// and goes on folding.
func TestChaosFoldWorkerDeathBetweenFolds(t *testing.T) {
	t.Parallel()
	c := qt.New(t)
	s := newChaosStack(t, 1)
	a := s.prover("a", 0)
	b := s.prover("b", 1)
	s.create(2)

	s.vote(0, 4)
	s.waitFor("two folds", func() bool { return s.foldCount() == 2 })
	c.Assert(s.election().FoldWorker, qt.Equals, a.URL)
	a.Kill()

	s.vote(4, len(s.ballots.votes))
	s.check(s.finish())
	c.Assert(s.election().FoldWorker, qt.Equals, b.URL)
	c.Assert(b.Count("import:fold"), qt.Equals, 1)
}

// TestChaosFoldWorkerDeathDuringFold kills the fold worker while it runs
// the second fold: after the retries the chain moves to the other prover,
// which imports the first fold and the two batches and folds them.
func TestChaosFoldWorkerDeathDuringFold(t *testing.T) {
	t.Parallel()
	c := qt.New(t)
	s := newChaosStack(t, 2)
	a := s.prover("a", 0)
	b := s.prover("b", 1)
	s.create(2)

	s.vote(0, 4)
	s.waitFor("the first fold", func() bool { return s.foldCount() == 1 })
	a.Hold(helpers.JobFold)
	s.vote(4, len(s.ballots.votes))
	s.waitFor("the second fold running", func() bool { return a.Running(helpers.JobFold) > 0 })
	a.Kill()

	s.check(s.finish())
	c.Assert(s.election().FoldWorker, qt.Equals, b.URL)
	c.Assert(b.Count("import:fold"), qt.Equals, 1)
	c.Assert(b.Count("import:batch"), qt.Equals, 2)
	c.Assert(len(b.Folds()[0].BatchJobs), qt.Equals, 2)
}

// TestChaosFoldWorkerFailing makes the fold worker fail folds while it
// stays up: a fold that fails once is run again on it, a fold that fails
// every attempt moves the chain to the other prover.
func TestChaosFoldWorkerFailing(t *testing.T) {
	t.Parallel()
	c := qt.New(t)
	s := newChaosStack(t, 1)
	a := s.prover("a", 0)
	b := s.prover("b", 1)
	s.create(2)

	a.FailNext(helpers.JobFold, 1)
	s.vote(0, 2)
	s.waitFor("the first fold", func() bool { return s.foldCount() == 1 })
	c.Assert(s.election().FoldWorker, qt.Equals, a.URL)
	c.Assert(a.Count("fold"), qt.Equals, 3) // a failed bootstrap pass, then the bootstrap and genesis folds

	a.FailNext(helpers.JobFold, 1000)
	s.vote(2, len(s.ballots.votes))
	s.check(s.finish())
	c.Assert(s.election().FoldWorker, qt.Equals, b.URL)
	c.Assert(b.Count("import:fold"), qt.Equals, 1)
}

// TestChaosStaleProver runs a prover not upgraded to the pinned circuit
// release, as in a rolling upgrade: its batch proofs are refused when they
// are made and proved on the other prover, and the fold chain first pinned
// to it moves when its aggregator answers with another vk. Nothing it
// proves is stored or folded.
func TestChaosStaleProver(t *testing.T) {
	t.Parallel()
	c := qt.New(t)
	s := newChaosStack(t, 1)
	a := s.prover("a", 5)
	b := s.prover("b", 0) // picked first
	b.SetVKs("0x"+strings.Repeat("0b", 32), "0x"+strings.Repeat("0a", 32))
	s.create(2)

	s.vote(0, len(s.ballots.votes))
	s.check(s.finish())
	c.Assert(s.election().FoldWorker, qt.Equals, a.URL)
	c.Assert(b.Count("prove") > 0, qt.IsTrue)
	c.Assert(b.Count("fold") > 0, qt.IsTrue)
	for _, bi := range s.batches() {
		c.Assert(bi.Worker, qt.Equals, a.URL, qt.Commentf("batch %d", bi.Seq))
	}
	c.Assert(s.worker(b.URL).FailedCount > 0, qt.IsTrue)
	c.Assert(a.Count("import:foreign")+b.Count("import:foreign"), qt.Equals, 0)
}

// TestChaosRejectedProof makes a prover's first batch proof one every fold
// rejects, although it is of the batch circuit: after a few failed folds on
// both provers the batch is proved again on the other prover, and the
// election finishes.
func TestChaosRejectedProof(t *testing.T) {
	t.Parallel()
	c := qt.New(t)
	s := newChaosStack(t, 1)
	a := s.prover("a", 0)
	b := s.prover("b", 5)
	a.Poison(1)
	s.create(2)

	s.vote(0, len(s.ballots.votes))
	s.check(s.finish())
	c.Assert(s.batches()[0].Worker, qt.Equals, b.URL)
	c.Assert(a.Count("fold")+b.Count("fold") > 3, qt.IsTrue)
}

// TestChaosBatchProverDeath kills a prover while it proves a batch: the
// batch is proved on the other one, and the fold chain stays where it is.
func TestChaosBatchProverDeath(t *testing.T) {
	t.Parallel()
	c := qt.New(t)
	s := newChaosStack(t, 1)
	a := s.prover("a", 0)
	b := s.prover("b", 3)
	s.create(2)

	s.vote(0, 2)
	s.waitFor("the first fold", func() bool { return s.foldCount() == 1 })
	c.Assert(s.election().FoldWorker, qt.Equals, a.URL)

	a.SetQueueLen(10) // b is now the least loaded
	s.waitFor("b least loaded", func() bool {
		w := s.svc.Pool.LeastLoaded()
		return w != nil && w.Address == b.URL
	})
	b.Hold(helpers.JobBatch)
	s.vote(2, 4)
	s.waitFor("a prove running on b", func() bool { return b.Running(helpers.JobBatch) > 0 })
	b.Kill()

	s.vote(4, len(s.ballots.votes))
	s.check(s.finish())
	c.Assert(s.election().FoldWorker, qt.Equals, a.URL)
	c.Assert(s.batches()[1].Worker, qt.Equals, a.URL)
	c.Assert(a.Count("import:fold"), qt.Equals, 0)
}

// TestChaosRemoveFoldWorker removes the fold worker through the API: the
// chain moves to the other prover at once, the removal outlives a restart,
// and the election finishes on the other prover.
func TestChaosRemoveFoldWorker(t *testing.T) {
	t.Parallel()
	c := qt.New(t)
	s := newChaosStack(t, 1)
	a := s.prover("a", 0)
	b := s.prover("b", 1)
	s.create(2)

	s.vote(0, 4)
	s.waitFor("two folds", func() bool { return s.foldCount() == 2 })
	c.Assert(s.election().FoldWorker, qt.Equals, a.URL)

	code, err := s.svc.Client.RemoveWorker(s.ctx, helpers.AdminToken(), s.worker(a.URL).ID)
	c.Assert(err, qt.IsNil, qt.Commentf("status %d", code))
	provedOnA := a.Count("prove")
	s.waitFor("the chain on b", func() bool {
		return s.election().FoldWorker == b.URL && b.Count("import:fold") == 1
	})

	s.restart()
	list, err := s.svc.Client.ListWorkers(s.ctx)
	c.Assert(err, qt.IsNil)
	c.Assert(len(list.Workers), qt.Equals, 1)
	c.Assert(list.Workers[0].Address, qt.Equals, b.URL)

	s.vote(4, len(s.ballots.votes))
	s.check(s.finish())
	c.Assert(s.election().FoldWorker, qt.Equals, b.URL)
	c.Assert(a.Count("prove"), qt.Equals, provedOnA)
}

// TestChaosScatter proves the batches of an election on three provers at
// once, one batch per vote: the proves overlap, no prover ever runs two jobs
// at a time, a prover that dies while it proves a batch has that batch
// proved on another one while the others go on, and the batches are
// imported and folded in seq order.
func TestChaosScatter(t *testing.T) {
	t.Parallel()
	c := qt.New(t)
	s := newChaosStack(t, 2)
	a := s.prover("a", 0) // the fold worker
	b := s.prover("b", 1)
	d := s.prover("c", 2)
	provers := []*helpers.FakeProver{a, b, d}
	for _, p := range provers {
		p.SetDelay(20 * time.Millisecond)
		p.Hold(helpers.JobBatch)
	}
	s.create(1)

	s.vote(0, len(s.ballots.votes))
	s.waitFor("a prove running on every prover", func() bool {
		for _, p := range provers {
			if p.Running(helpers.JobBatch) == 0 {
				return false
			}
		}
		return true
	})
	var dying uint64
	s.waitFor("the batch c proves", func() bool {
		i := slices.IndexFunc(s.batches(), func(bi *types.BatchInput) bool { return bi.Worker == d.URL })
		dying = uint64(i)
		return i >= 0
	})
	d.Kill()
	a.Release(helpers.JobBatch)
	b.Release(helpers.JobBatch)

	s.check(s.finish())
	sealed := s.batches()
	c.Assert(len(sealed), qt.Equals, len(s.ballots.votes))
	c.Assert(s.election().FoldWorker, qt.Equals, a.URL)
	for _, bi := range sealed {
		c.Assert(bi.Worker == a.URL || bi.Worker == b.URL, qt.IsTrue, qt.Commentf("batch %d on %s", bi.Seq, bi.Worker))
	}
	c.Assert(d.Count("prove"), qt.Equals, 1)
	c.Assert(a.Count("prove")+b.Count("prove"), qt.Equals, len(sealed), qt.Commentf("batch %d proved again", dying))

	var proves []helpers.Interval
	for _, p := range provers {
		c.Assert(helpers.MaxConcurrent(p.Intervals()), qt.Equals, 1, qt.Commentf("jobs at once on %s", p.URL))
		proves = append(proves, p.Intervals(helpers.JobBatch)...)
	}
	c.Assert(helpers.MaxConcurrent(proves), qt.Equals, len(provers))

	imported := a.Imported()
	c.Assert(len(imported), qt.Equals, len(sealed))
	for i, p := range imported {
		c.Assert(p.NewRoot, qt.Equals, sealed[i].NewStateRoot, qt.Commentf("import %d", i))
	}
}

// TestChaosRestartMidScatter ends an election while three of its batches are
// proved at once, on three provers, and restarts the orchestrator during the
// drain: each of those batches waits for its prove job again, so no batch is
// proved twice and no prover runs two jobs at a time.
func TestChaosRestartMidScatter(t *testing.T) {
	t.Parallel()
	c := qt.New(t)
	s := newChaosStack(t, 2)
	provers := []*helpers.FakeProver{s.prover("a", 0), s.prover("b", 1), s.prover("c", 2)}
	for _, p := range provers {
		p.SetDelay(20 * time.Millisecond)
		p.Hold(helpers.JobBatch)
	}
	s.create(1)

	s.vote(0, len(s.ballots.votes))
	s.setStatus(types.StatusEnded)
	jobs := map[uint64]string{} // seq -> prove job, of the batches being proved
	s.waitFor("three batches proved at once", func() bool {
		for _, bi := range s.batches() {
			if bi.JobID != "" {
				jobs[bi.Seq] = bi.JobID
			}
		}
		return len(jobs) == len(provers)
	})
	for _, p := range provers {
		c.Assert(p.Running(helpers.JobBatch), qt.Equals, 1)
	}
	s.restart()

	c.Assert(s.election().Status, qt.Equals, types.StatusEnded.String())
	for _, p := range provers {
		p.Release(helpers.JobBatch)
	}
	s.check(s.finish())
	proves := 0
	for _, p := range provers {
		proves += p.Count("prove")
		c.Assert(helpers.MaxConcurrent(p.Intervals()), qt.Equals, 1, qt.Commentf("jobs at once on %s", p.URL))
	}
	c.Assert(proves, qt.Equals, len(s.batches()))
	for _, bi := range s.batches() {
		if job, ok := jobs[bi.Seq]; ok {
			c.Assert(bi.JobID, qt.Equals, job, qt.Commentf("batch %d", bi.Seq))
		}
	}
}
