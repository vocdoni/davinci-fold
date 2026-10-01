package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/fxamacker/cbor/v2"
	"github.com/vocdoni/davinci-node/db"
	"github.com/vocdoni/davinci-node/db/metadb"

	"github.com/vocdoni/davinci-fold/storage"
	"github.com/vocdoni/davinci-fold/types"
	davinci "github.com/vocdoni/davinci-zkvm/go-sdk"
)

// waitStatus waits for an election to reach status.
func waitStatus(t *testing.T, e *Engine, id types.ElectionID, status types.Status) *types.Election {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		el, err := e.Election(id)
		qt.Assert(t, err, qt.IsNil)
		if el.Status == status {
			return el
		}
		if time.Now().After(deadline) {
			t.Fatalf("election %s is %s, want %s", id, el.Status, status)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// voteStatus returns the status of the vote whose ID ends in voteID.
func voteStatus(t *testing.T, s *storage.Storage, id types.ElectionID, voteID byte) types.VoteStatus {
	t.Helper()
	st, err := s.VoteStatus(id, types.VoteID(testVoteIDBytes(voteID)))
	qt.Assert(t, err, qt.IsNil)
	return st
}

// TestStatusTransitions tries every organizer status change from every
// status: only active and paused elections change, and only as allowed.
func TestStatusTransitions(t *testing.T) {
	e, s := newTestEngine(t)
	defer e.Stop()

	allowed := map[[2]types.Status]bool{
		{types.StatusActive, types.StatusPaused}:   true,
		{types.StatusActive, types.StatusEnded}:    true,
		{types.StatusActive, types.StatusCanceled}: true,
		{types.StatusPaused, types.StatusActive}:   true,
		{types.StatusPaused, types.StatusEnded}:    true,
		{types.StatusPaused, types.StatusCanceled}: true,
	}
	froms := []types.Status{
		types.StatusActive, types.StatusPaused, types.StatusEnded, types.StatusDecrypting,
		types.StatusFinalizing, types.StatusResults, types.StatusCanceled,
	}
	tos := []types.Status{types.StatusActive, types.StatusPaused, types.StatusEnded, types.StatusCanceled}
	id := byte(0x40)
	for _, from := range froms {
		for _, to := range tos {
			t.Run(from.String()+" to "+to.String(), func(t *testing.T) {
				c := qt.New(t)
				id++
				el, _ := testElection(t, id, time.Now().Add(time.Hour))
				c.Assert(e.CreateElection("admin", el), qt.IsNil)
				switch from {
				case types.StatusActive:
				case types.StatusPaused, types.StatusEnded, types.StatusCanceled:
					c.Assert(e.SetStatus("admin", el.ID, from), qt.IsNil)
				default:
					c.Assert(s.SetElectionStatus(el.ID, from), qt.IsNil)
				}

				err := e.SetStatus("admin", el.ID, to)
				want := to
				if allowed[[2]types.Status{from, to}] {
					c.Assert(err, qt.IsNil)
				} else {
					c.Assert(err, qt.ErrorIs, ErrInvalidTransition)
					want = from
				}
				got, err := e.Election(el.ID)
				c.Assert(err, qt.IsNil)
				c.Assert(got.Status, qt.Equals, want)
			})
		}
	}

	err := e.SetStatus("admin", types.ElectionID{0xff}, types.StatusEnded)
	qt.Assert(t, err, qt.ErrorIs, ErrElectionNotFound)
}

// TestPauseAndResume checks a paused election refuses votes but still seals
// the ones it accepted, and takes votes again once resumed.
func TestPauseAndResume(t *testing.T) {
	c := qt.New(t)
	e, s := newTestEngine(t)
	defer e.Stop()

	el, encKey := testElection(t, 0x60, time.Now().Add(time.Hour))
	c.Assert(e.CreateElection("admin", el), qt.IsNil)
	_, err := e.SubmitVote(el.ID, makeSub(t, encKey, 0))
	c.Assert(err, qt.IsNil)

	c.Assert(e.SetStatus("admin", el.ID, types.StatusPaused), qt.IsNil)
	_, err = e.SubmitVote(el.ID, makeSub(t, encKey, 1))
	c.Assert(err, qt.ErrorIs, ErrNotAcceptingVotes)
	_, err = e.EncryptedResults(el.ID)
	c.Assert(err, qt.ErrorMatches, ".*not decrypting yet.*")

	// The accepted vote is sealed by the time window while paused.
	time.Sleep(20 * time.Millisecond)
	e.tick()
	bi, err := s.BatchInput(el.ID, 0)
	c.Assert(err, qt.IsNil)
	c.Assert(len(bi.VoteIDs), qt.Equals, 1)

	c.Assert(e.SetStatus("admin", el.ID, types.StatusActive), qt.IsNil)
	_, err = e.SubmitVote(el.ID, makeSub(t, encKey, 1))
	c.Assert(err, qt.IsNil)
}

// TestPausedElectionEnds checks the end time ends a paused election.
func TestPausedElectionEnds(t *testing.T) {
	c := qt.New(t)
	e, _ := newTestEngine(t)
	defer e.Stop()

	el, _ := testElection(t, 0x61, time.Now().Add(time.Hour))
	c.Assert(e.CreateElection("admin", el), qt.IsNil)
	c.Assert(e.SetStatus("admin", el.ID, types.StatusPaused), qt.IsNil)
	c.Assert(e.store.UpdateElection(el.ID, func(stored *types.Election) error {
		stored.EndTime = time.Now().Add(-time.Second)
		return nil
	}), qt.IsNil)
	e.tick()
	got, err := e.Election(el.ID)
	c.Assert(err, qt.IsNil)
	c.Assert(got.Status, qt.Equals, types.StatusEnded)
}

// TestCancelStopsWork checks a canceled election drops its pending votes,
// marks every vote as error, seals nothing more and stays readable.
func TestCancelStopsWork(t *testing.T) {
	c := qt.New(t)
	e, s := newTestEngine(t)
	defer e.Stop()

	el, encKey := testElection(t, 0x62, time.Now().Add(time.Hour))
	c.Assert(e.CreateElection("admin", el), qt.IsNil)
	for i := range 3 {
		_, err := e.SubmitVote(el.ID, makeSub(t, encKey, i))
		c.Assert(err, qt.IsNil)
	}
	c.Assert(voteStatus(t, s, el.ID, 0xa0), qt.Equals, types.VoteStatusBatched)
	c.Assert(voteStatus(t, s, el.ID, 0xa2), qt.Equals, types.VoteStatusPending)

	c.Assert(e.SetStatus("admin", el.ID, types.StatusCanceled), qt.IsNil)
	for _, v := range []byte{0xa0, 0xa1, 0xa2} {
		c.Assert(voteStatus(t, s, el.ID, v), qt.Equals, types.VoteStatusError)
	}
	_, loaded := e.runtime(el.ID)
	c.Assert(loaded, qt.IsFalse)

	// Nothing is sealed any more, not even past the window or the end time.
	time.Sleep(20 * time.Millisecond)
	e.tick()
	_, err := s.BatchInput(el.ID, 1)
	c.Assert(err, qt.Equals, storage.ErrNotFound)
	_, err = e.SubmitVote(el.ID, makeSub(t, encKey, 3))
	c.Assert(err, qt.ErrorIs, ErrNotAcceptingVotes)

	got, err := e.Election(el.ID)
	c.Assert(err, qt.IsNil)
	c.Assert(got.Status, qt.Equals, types.StatusCanceled)
	_, err = e.EncryptedResults(el.ID)
	c.Assert(err, qt.IsNotNil)

	// A restart does not load it again.
	e2, err := NewEngine(s, Options{BatchSize: 2, Validator: structuralValidator{}})
	c.Assert(err, qt.IsNil)
	defer e2.Stop()
	_, loaded = e2.runtime(el.ID)
	c.Assert(loaded, qt.IsFalse)
	_, err = e2.SubmitVote(el.ID, makeSub(t, encKey, 3))
	c.Assert(err, qt.ErrorIs, ErrNotAcceptingVotes)
}

// TestSealIfStaleSealsAllStale checks one monitor sweep seals every stale
// batch: three votes of one voter need three batches.
func TestSealIfStaleSealsAllStale(t *testing.T) {
	c := qt.New(t)
	e, s := newTestEngine(t)
	defer e.Stop()

	el, encKey := testElection(t, 0x63, time.Now().Add(time.Hour))
	c.Assert(e.CreateElection("admin", el), qt.IsNil)
	for _, v := range []byte{0xa0, 0xb0, 0xc0} {
		_, err := e.SubmitVote(el.ID, makeVote(t, encKey, 0, v))
		c.Assert(err, qt.IsNil)
	}
	time.Sleep(20 * time.Millisecond)
	rt, _ := e.runtime(el.ID)
	e.sealIfStale(rt)
	for seq, want := range []string{"a0", "b0", "c0"} {
		ids, _ := batchVotes(t, s, el.ID, uint64(seq))
		c.Assert(ids, qt.DeepEquals, []string{want})
	}
}

// pendingVote puts a vote straight into an election's pending buffer, past
// ingest, as sub would be stored.
func pendingVote(t *testing.T, e *Engine, id types.ElectionID, voteID string, sub *VoteSubmission) *types.Vote {
	t.Helper()
	rt, ok := e.runtime(id)
	qt.Assert(t, ok, qt.IsTrue)
	bundle, err := structuralValidator{}.Validate(rt.rules, sub)
	qt.Assert(t, err, qt.IsNil)
	slot, err := bundle.Census.SlotKey()
	qt.Assert(t, err, qt.IsNil)
	payload, err := cbor.Marshal(bundle)
	qt.Assert(t, err, qt.IsNil)
	v := &types.Vote{
		ID: types.VoteID(voteID), Address: sub.Address, Slot: slot, VoteIDKey: sub.VoteIDKey,
		Ballot: sub.Ballot, Payload: payload, SubmittedAt: time.Now(),
	}
	qt.Assert(t, e.store.AddVote(id, v), qt.IsNil)
	rt.mu.Lock()
	rt.pending = append(rt.pending, v)
	rt.mu.Unlock()
	return v
}

// TestSealDropsRefusedVote checks a vote the state refuses (its vote-ID key
// is already in the tree) is dropped with status error, the rest of its batch
// is sealed on top of an intact state, and sealing goes on.
func TestSealDropsRefusedVote(t *testing.T) {
	c := qt.New(t)
	e, s := newTestEngine(t)
	defer e.Stop()

	el, encKey := testElection(t, 0x64, time.Now().Add(time.Hour))
	el.BatchSize = 3
	c.Assert(e.CreateElection("admin", el), qt.IsNil)
	for i := range 3 {
		_, err := e.SubmitVote(el.ID, makeSub(t, encKey, i))
		c.Assert(err, qt.IsNil)
	}
	_, err := s.BatchInput(el.ID, 0)
	c.Assert(err, qt.IsNil)

	// Batch 1: voter 3, then a vote of voter 4 reusing voter 0's vote-ID key
	// (ingest refuses it as a duplicate, so it goes straight into the
	// buffer), then voter 5.
	_, err = e.SubmitVote(el.ID, makeSub(t, encKey, 3))
	c.Assert(err, qt.IsNil)
	refused := pendingVote(t, e, el.ID, "refused", makeVote(t, encKey, 4, 0xa0))
	_, err = e.SubmitVote(el.ID, makeSub(t, encKey, 5))
	c.Assert(err, qt.IsNil)

	ids, _ := batchVotes(t, s, el.ID, 1)
	c.Assert(ids, qt.DeepEquals, []string{"a3", "a5"})
	st, err := s.VoteStatus(el.ID, refused.ID)
	c.Assert(err, qt.IsNil)
	c.Assert(st, qt.Equals, types.VoteStatusError)
	c.Assert(voteStatus(t, s, el.ID, 0xa5), qt.Equals, types.VoteStatusBatched)

	// The state is the persisted one, and the next batch builds on it.
	rt, _ := e.runtime(el.ID)
	bi, err := s.BatchInput(el.ID, 1)
	c.Assert(err, qt.IsNil)
	c.Assert(rt.current().Root(), qt.Equals, bi.NewStateRoot)
	voters, _ := rt.current().Voters()
	c.Assert(voters, qt.Equals, uint64(5))
	for _, i := range []int{6, 7, 1} {
		_, err := e.SubmitVote(el.ID, makeVote(t, encKey, i, byte(0xc0+i)))
		c.Assert(err, qt.IsNil)
	}
	next, err := s.BatchInput(el.ID, 2)
	c.Assert(err, qt.IsNil)
	var req davinci.ProveRequest
	c.Assert(json.Unmarshal(next.ProveRequest, &req), qt.IsNil)
	c.Assert(req.State.OldStateRoot, qt.Equals, bi.NewStateRoot)

	// A batch the state refuses entirely seals nothing.
	pendingVote(t, e, el.ID, "refused-2", makeVote(t, encKey, 2, 0xa1))
	rt.mu.Lock()
	c.Assert(e.sealLocked(rt), qt.IsNil)
	c.Assert(len(rt.pending), qt.Equals, 0)
	c.Assert(rt.batchSeq, qt.Equals, uint64(3))
	rt.mu.Unlock()
}

// TestDecryptionKeyFinalizesInBackground checks a decryption key is checked
// at once and the finalize then runs in the background, the election's
// status telling how it ends.
func TestDecryptionKeyFinalizesInBackground(t *testing.T) {
	c := qt.New(t)
	e, s := newTestEngine(t)
	defer e.Stop()

	el, encKey, priv := testElectionWithKey(t, 0x65, time.Now().Add(time.Hour))
	c.Assert(e.CreateElection("admin", el), qt.IsNil)
	for i := range 2 {
		_, err := e.SubmitVote(el.ID, makeSub(t, encKey, i))
		c.Assert(err, qt.IsNil)
	}
	c.Assert(e.SetStatus("admin", el.ID, types.StatusEnded), qt.IsNil)
	c.Assert(s.SetElectionStatus(el.ID, types.StatusDecrypting), qt.IsNil)

	outcome := make(chan error)
	e.finalize = func(id types.ElectionID, _ *big.Int) (*types.Results, error) {
		if err := <-outcome; err != nil {
			return nil, err
		}
		return &types.Results{ElectionID: id}, nil
	}

	// A wrong key is refused at once.
	wrong := new(big.Int).Add(priv, big.NewInt(1))
	c.Assert(e.SubmitDecryptionKey("kw", el.ID, wrong), qt.ErrorIs, ErrInvalidDecryptionKey)
	c.Assert(e.SubmitDecryptionKey("kw", el.ID, big.NewInt(0)), qt.ErrorIs, ErrInvalidDecryptionKey)
	waitStatus(t, e, el.ID, types.StatusDecrypting)

	// The right key starts the finalize; a second one waits for it to end.
	c.Assert(e.SubmitDecryptionKey("kw", el.ID, priv), qt.IsNil)
	waitStatus(t, e, el.ID, types.StatusFinalizing)
	c.Assert(e.SubmitDecryptionKey("kw", el.ID, priv), qt.ErrorIs, ErrInvalidTransition)

	// A failed finalize returns to decrypting with its reason only; the
	// detail goes to the audit trail.
	detail := "finalize job fin-7 (attempt 3/3): Post \"http://10.0.0.5:8080/finalize\": connection refused"
	outcome <- fmt.Errorf("%w: %s", errFinalProofFailed, detail)
	got := waitStatus(t, e, el.ID, types.StatusDecrypting)
	c.Assert(got.FinalizeError, qt.Equals, "final proof failed")
	audit, err := s.ListAudit()
	c.Assert(err, qt.IsNil)
	c.Assert(audit[len(audit)-1].Action, qt.Equals, "finalize_failed: final proof failed: "+detail)

	// The key can be submitted again, which clears the error.
	c.Assert(e.SubmitDecryptionKey("kw", el.ID, priv), qt.IsNil)
	got = waitStatus(t, e, el.ID, types.StatusFinalizing)
	c.Assert(got.FinalizeError, qt.Equals, "")
	outcome <- nil
	waitStatus(t, e, el.ID, types.StatusResults)
	c.Assert(voteStatus(t, s, el.ID, 0xa0), qt.Equals, types.VoteStatusSettled)
	c.Assert(voteStatus(t, s, el.ID, 0xa1), qt.Equals, types.VoteStatusSettled)
	c.Assert(e.SubmitDecryptionKey("kw", el.ID, priv), qt.ErrorIs, ErrInvalidTransition)
}

// TestRestoreResetsInterruptedFinalize checks an election left finalizing by
// a stopped process is decrypting again after a restart, with the reason.
func TestRestoreResetsInterruptedFinalize(t *testing.T) {
	c := qt.New(t)
	database, err := metadb.New(db.TypeInMem, "")
	c.Assert(err, qt.IsNil)
	s := storage.New(database)
	e, err := NewEngine(s, Options{BatchSize: 2, Validator: structuralValidator{}})
	c.Assert(err, qt.IsNil)
	el, _ := testElection(t, 0x66, time.Now().Add(time.Hour))
	c.Assert(e.CreateElection("admin", el), qt.IsNil)
	c.Assert(s.SetElectionStatus(el.ID, types.StatusFinalizing), qt.IsNil)
	e.Stop()

	e2, err := NewEngine(s, Options{BatchSize: 2, Validator: structuralValidator{}})
	c.Assert(err, qt.IsNil)
	defer e2.Stop()
	got, err := e2.Election(el.ID)
	c.Assert(err, qt.IsNil)
	c.Assert(got.Status, qt.Equals, types.StatusDecrypting)
	c.Assert(got.FinalizeError, qt.Equals, "finalize interrupted")
}

// TestFinalizeSummary checks a failed finalize is summarized by the reason of
// the step that failed, without its detail.
func TestFinalizeSummary(t *testing.T) {
	wrap := func(errs ...error) error {
		err := errors.New(`Post "http://10.0.0.5:8080/fold": dial tcp: connection refused`)
		for _, e := range errs {
			err = fmt.Errorf("%w: job fold-3 (attempt 3/3): %w", e, err)
		}
		return err
	}
	for _, tc := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("%w: %w", errFinalFoldFailed, errNoWorker), "prover unavailable"},
		{wrap(errWorkerRemoved, errFinalProofFailed), "prover unavailable"},
		{wrap(context.Canceled, errFinalFoldFailed), "finalize interrupted"},
		{wrap(errFinalFoldFailed), "fold failed"},
		{fmt.Errorf("%w: no fold in the chain", errNothingToFinalize), "nothing to finalize"},
		{wrap(errDecryptionFailed), "decryption failed"},
		{wrap(errFinalProofFailed), "final proof failed"},
		{fmt.Errorf("%w: digest state root 0xab != local root 0xcd", errVerificationFailed), "final proof verification failed"},
		{errors.New("persist results: disk full"), "internal error"},
	} {
		qt.Check(t, finalizeSummary(tc.err), qt.Equals, tc.want, qt.Commentf("%v", tc.err))
	}
}

// TestEffectiveFoldEvery checks an election without a fold cadence reads as
// the engine default, whether created now or stored without one.
func TestEffectiveFoldEvery(t *testing.T) {
	c := qt.New(t)
	database, err := metadb.New(db.TypeInMem, "")
	c.Assert(err, qt.IsNil)
	s := storage.New(database)
	e, err := NewEngine(s, Options{BatchSize: 2, FoldEvery: 3, Validator: structuralValidator{}})
	c.Assert(err, qt.IsNil)
	defer e.Stop()

	el, _ := testElection(t, 0x67, time.Now().Add(time.Hour))
	el.FoldEvery = 0
	c.Assert(e.CreateElection("admin", el), qt.IsNil)
	stored, err := s.Election(el.ID)
	c.Assert(err, qt.IsNil)
	c.Assert(stored.FoldEvery, qt.Equals, 3)

	legacy := &types.Election{ID: types.ElectionID{0x68}, Status: types.StatusActive, BatchSize: 2}
	c.Assert(s.CreateElection(legacy, nil), qt.IsNil)
	got, err := e.Election(legacy.ID)
	c.Assert(err, qt.IsNil)
	c.Assert(got.FoldEvery, qt.Equals, 3)
	all, err := e.ListElections()
	c.Assert(err, qt.IsNil)
	for _, el := range all {
		c.Assert(el.FoldEvery, qt.Equals, 3)
	}
}
