package orchestrator

import (
	"encoding/hex"
	"encoding/json"
	"math/big"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	bjjgnark "github.com/vocdoni/davinci-node/crypto/ecc/bjj_gnark"
	"github.com/vocdoni/davinci-node/crypto/elgamal"
	"github.com/vocdoni/davinci-node/db"
	"github.com/vocdoni/davinci-node/db/metadb"

	"github.com/vocdoni/davinci-fold/storage"
	"github.com/vocdoni/davinci-fold/types"
	davinci "github.com/vocdoni/davinci-zkvm/go-sdk"
)

// fieldsPerBallot is the protocol constant for the number of field elements
// per ballot (mirrors davinci-node spec/params.FieldsPerBallot). Inlined here
// to avoid pulling the davinci-node/spec module, which davinci-fold doesn't
// otherwise need.
const fieldsPerBallot = 8

const testCensusRoot = "0x1234"

func bigHex(bi *big.Int) string { return "0x" + hex.EncodeToString(bi.Bytes()) }

// testElection builds an election config bound to a fresh ElGamal key.
func testElection(t *testing.T, id byte, endTime time.Time) (*types.Election, *bjjgnark.BJJ) {
	t.Helper()
	pub, _, err := elgamal.GenerateKey(bjjgnark.New())
	qt.Assert(t, err, qt.IsNil)
	encKey := pub.(*bjjgnark.BJJ)
	rx, ry := encKey.Point()
	return &types.Election{
		ID:        types.ElectionID{id, 0x02, 0x03},
		BatchSize: 2,
		FoldEvery: 4,
		EndTime:   endTime,
		Config: types.ElectionConfig{
			ProcessID:    "0xabcdef",
			BallotMode:   "0x01",
			EncX:         bigHex(rx),
			EncY:         bigHex(ry),
			CensusOrigin: 1,
			CensusRoot:   testCensusRoot,
			VK:           json.RawMessage(`{"protocol":"groth16"}`),
		},
	}, encKey
}

// makeSub builds a valid vote submission for voter i under encKey.
func makeSub(t *testing.T, encKey *bjjgnark.BJJ, i int) *VoteSubmission {
	t.Helper()
	var msg [fieldsPerBallot]*big.Int
	for j := range msg {
		msg[j] = big.NewInt(int64(10*i + j))
	}
	b, err := elgamal.NewBallot(encKey).Encrypt(msg, encKey, big.NewInt(int64(i+1)))
	qt.Assert(t, err, qt.IsNil)
	return &VoteSubmission{
		VoteID:       []byte{byte(0xa0 + i)},
		Address:      []byte{byte(i + 1)},
		CensusIdx:    i,
		AddressLo16:  uint64(i + 1),
		VoteIDKey:    uint64(i+1) | (uint64(1) << 63),
		Ballot:       b.Serialize(),
		Proof:        json.RawMessage(`{"pi_a":[]}`),
		PublicInputs: []string{"0"},
		Sig:          json.RawMessage(`{"r":"0"}`),
		Census:       davinci.CensusProof{Root: testCensusRoot},
	}
}

func newTestEngine(t *testing.T) (*Engine, *storage.Storage) {
	t.Helper()
	database, err := metadb.New(db.TypeInMem, "")
	qt.Assert(t, err, qt.IsNil)
	s := storage.New(database)
	e, err := NewEngine(s, Options{
		BatchSize:       2,
		BatchTimeWindow: 10 * time.Millisecond,
		// Synthetic ballots: exercise ingest/lifecycle mechanics, not crypto.
		Validator: structuralValidator{},
	})
	qt.Assert(t, err, qt.IsNil)
	return e, s
}

func TestCreateAndIngestSeal(t *testing.T) {
	c := qt.New(t)
	e, s := newTestEngine(t)
	defer e.Stop()

	el, encKey := testElection(t, 0x01, time.Now().Add(time.Hour))
	c.Assert(e.CreateElection("admin", el), qt.IsNil)

	got, err := e.Election(el.ID)
	c.Assert(err, qt.IsNil)
	c.Assert(got.Status, qt.Equals, types.StatusActive)

	rt, ok := e.runtime(el.ID)
	c.Assert(ok, qt.IsTrue)
	genesisRoot := rt.state.Root()

	// Two votes hit BatchSize=2 and seal a batch.
	_, err = e.SubmitVote(el.ID, makeSub(t, encKey, 0))
	c.Assert(err, qt.IsNil)
	_, err = e.SubmitVote(el.ID, makeSub(t, encKey, 1))
	c.Assert(err, qt.IsNil)

	// Batch 0 persisted, root advanced, votes batched.
	bi, err := s.BatchInput(el.ID, 0)
	c.Assert(err, qt.IsNil)
	c.Assert(bi.NewStateRoot != genesisRoot, qt.IsTrue)
	c.Assert(len(bi.VoteIDs), qt.Equals, 2)
	c.Assert(len(bi.ProveRequest) > 0, qt.IsTrue)

	var req davinci.ProveRequest
	c.Assert(json.Unmarshal(bi.ProveRequest, &req), qt.IsNil)
	c.Assert(req.Output, qt.Equals, "stark")
	c.Assert(len(req.Proofs), qt.Equals, 2)
	c.Assert(req.State, qt.Not(qt.IsNil))
	c.Assert(req.State.NewStateRoot, qt.Equals, bi.NewStateRoot)

	st, err := s.VoteStatus(el.ID, types.VoteID([]byte{0xa0}))
	c.Assert(err, qt.IsNil)
	c.Assert(st, qt.Equals, types.VoteStatusBatched)

	// Pending buffer drained.
	rt.mu.Lock()
	c.Assert(len(rt.pending), qt.Equals, 0)
	rt.mu.Unlock()
}

func TestDuplicateAndUnknownRejected(t *testing.T) {
	c := qt.New(t)
	e, _ := newTestEngine(t)
	defer e.Stop()

	el, encKey := testElection(t, 0x02, time.Now().Add(time.Hour))
	c.Assert(e.CreateElection("admin", el), qt.IsNil)

	// Unknown election.
	_, err := e.SubmitVote(types.ElectionID{0xff}, makeSub(t, encKey, 0))
	c.Assert(err, qt.Not(qt.IsNil))

	sub := makeSub(t, encKey, 0)
	_, err = e.SubmitVote(el.ID, sub)
	c.Assert(err, qt.IsNil)
	// Same voteID rejected as duplicate.
	_, err = e.SubmitVote(el.ID, makeSub(t, encKey, 0))
	c.Assert(err, qt.Not(qt.IsNil))
}

func TestTimeWindowSeal(t *testing.T) {
	c := qt.New(t)
	e, s := newTestEngine(t)
	defer e.Stop()

	el, encKey := testElection(t, 0x03, time.Now().Add(time.Hour))
	c.Assert(e.CreateElection("admin", el), qt.IsNil)

	// One vote stays pending (below BatchSize).
	_, err := e.SubmitVote(el.ID, makeSub(t, encKey, 0))
	c.Assert(err, qt.IsNil)
	_, err = s.BatchInput(el.ID, 0)
	c.Assert(err, qt.Equals, storage.ErrNotFound)

	// After the window elapses, a tick seals the partial batch.
	time.Sleep(20 * time.Millisecond)
	e.tick()
	bi, err := s.BatchInput(el.ID, 0)
	c.Assert(err, qt.IsNil)
	c.Assert(len(bi.VoteIDs), qt.Equals, 1)
}

func TestLifecycleEndSealsAndEnds(t *testing.T) {
	c := qt.New(t)
	e, s := newTestEngine(t)
	defer e.Stop()

	// End time already in the past: the next tick seals + ends.
	el, encKey := testElection(t, 0x04, time.Now().Add(-time.Minute))
	c.Assert(e.CreateElection("admin", el), qt.IsNil)
	_, err := e.SubmitVote(el.ID, makeSub(t, encKey, 0))
	c.Assert(err, qt.IsNil)

	e.tick()

	got, err := e.Election(el.ID)
	c.Assert(err, qt.IsNil)
	c.Assert(got.Status, qt.Equals, types.StatusEnded)
	bi, err := s.BatchInput(el.ID, 0)
	c.Assert(err, qt.IsNil)
	c.Assert(len(bi.VoteIDs), qt.Equals, 1)
}

func TestFinalizeLifecycleGating(t *testing.T) {
	c := qt.New(t)
	e, s := newTestEngine(t) // ingest-only: no scheduler
	defer e.Stop()

	el, _ := testElection(t, 0x06, time.Now().Add(time.Hour))
	c.Assert(e.CreateElection("admin", el), qt.IsNil)

	// Ciphertext is not served before the election reaches Decrypting.
	_, err := e.EncryptedResults(el.ID)
	c.Assert(err, qt.Not(qt.IsNil))

	// A decryption key is rejected while the election is still Active.
	_, err = e.SubmitDecryptionKey("kw", el.ID, big.NewInt(7))
	c.Assert(err, qt.Not(qt.IsNil))

	// Once Decrypting, the 32-coord ciphertext (8 ElGamal ciphertexts) is served.
	c.Assert(s.SetElectionStatus(el.ID, types.StatusDecrypting), qt.IsNil)
	ct, err := e.EncryptedResults(el.ID)
	c.Assert(err, qt.IsNil)
	c.Assert(len(ct), qt.Equals, 32)

	// Without a scheduler the key cannot be finalized, and the status is left at
	// Decrypting so a real keywarden could retry against a worker-backed engine.
	_, err = e.SubmitDecryptionKey("kw", el.ID, big.NewInt(7))
	c.Assert(err, qt.Not(qt.IsNil))
	got, err := e.Election(el.ID)
	c.Assert(err, qt.IsNil)
	c.Assert(got.Status, qt.Equals, types.StatusDecrypting)
}

func TestRestoreAcrossEngines(t *testing.T) {
	c := qt.New(t)
	database, err := metadb.New(db.TypeInMem, "")
	c.Assert(err, qt.IsNil)
	s := storage.New(database)

	e1, err := NewEngine(s, Options{BatchSize: 2, Validator: structuralValidator{}})
	c.Assert(err, qt.IsNil)
	el, encKey := testElection(t, 0x05, time.Now().Add(time.Hour))
	c.Assert(e1.CreateElection("admin", el), qt.IsNil)
	_, err = e1.SubmitVote(el.ID, makeSub(t, encKey, 0))
	c.Assert(err, qt.IsNil)
	_, err = e1.SubmitVote(el.ID, makeSub(t, encKey, 1))
	c.Assert(err, qt.IsNil)
	rt1, _ := e1.runtime(el.ID)
	wantRoot := rt1.state.Root()
	e1.Stop()

	// A fresh engine over the same storage restores State + batch sequence.
	e2, err := NewEngine(s, Options{BatchSize: 2, Validator: structuralValidator{}})
	c.Assert(err, qt.IsNil)
	defer e2.Stop()
	rt2, ok := e2.runtime(el.ID)
	c.Assert(ok, qt.IsTrue)
	c.Assert(rt2.state.Root(), qt.Equals, wantRoot)
	c.Assert(rt2.batchSeq, qt.Equals, uint64(1))

	// Next sealed batch lands at seq 1, building on the restored root.
	_, err = e2.SubmitVote(el.ID, makeSub(t, encKey, 2))
	c.Assert(err, qt.IsNil)
	_, err = e2.SubmitVote(el.ID, makeSub(t, encKey, 3))
	c.Assert(err, qt.IsNil)
	bi, err := s.BatchInput(el.ID, 1)
	c.Assert(err, qt.IsNil)
	c.Assert(bi.NewStateRoot != wantRoot, qt.IsTrue)
}
