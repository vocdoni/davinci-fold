package orchestrator

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/vocdoni/davinci-node/db"
	"github.com/vocdoni/davinci-node/db/metadb"

	"github.com/vocdoni/davinci-fold/storage"
	"github.com/vocdoni/davinci-fold/types"
	davinci "github.com/vocdoni/davinci-zkvm/go-sdk"
	"github.com/vocdoni/davinci-zkvm/go-sdk/vocdoni/circuits/ballotproof"
	bjjgnark "github.com/vocdoni/davinci-zkvm/go-sdk/vocdoni/crypto/ecc/bjj_gnark"
	"github.com/vocdoni/davinci-zkvm/go-sdk/vocdoni/crypto/elgamal"
	spectestutil "github.com/vocdoni/davinci-zkvm/go-sdk/vocdoni/spec/testutil"
	leanimt "github.com/vocdoni/lean-imt-go"
)

// testCensusSize is the number of voters in the test census.
const testCensusSize = 8

// testCensus is the lean-IMT census of the test voters: voter i has address
// testAddress(i) and weight 1.
var testCensus = func() *leanimt.LeanIMT[*big.Int] {
	tree, err := leanimt.New(leanimt.PoseidonHasher, leanimt.BigIntEqual, nil, nil, nil)
	if err != nil {
		panic(err)
	}
	for i := range testCensusSize {
		tree.Insert(davinci.PackAddressWeight(new(big.Int).SetBytes(testAddress(i)), big.NewInt(1)))
	}
	return tree
}()

// testAddress is the 20-byte address of test voter i.
func testAddress(i int) []byte {
	a := make([]byte, 20)
	a[0], a[19] = 0xaa, byte(i+1)
	return a
}

// fr32 encodes a field element as the 32-byte big-endian hex a census proof
// carries.
func fr32(v *big.Int) string {
	var b [32]byte
	v.FillBytes(b[:])
	return "0x" + hex.EncodeToString(b[:])
}

// testCensusRoot returns the root of the test census.
func testCensusRoot(t *testing.T) string {
	t.Helper()
	root, ok := testCensus.Root()
	qt.Assert(t, ok, qt.IsTrue)
	return fr32(root)
}

// censusProof returns test voter i's membership proof.
func censusProof(t *testing.T, i int) davinci.CensusProof {
	t.Helper()
	p, err := testCensus.GenerateProof(i)
	qt.Assert(t, err, qt.IsNil)
	sibs := make([]string, len(p.Siblings))
	for j, s := range p.Siblings {
		sibs[j] = fr32(s)
	}
	return davinci.CensusProof{Root: fr32(p.Root), Leaf: fr32(p.Leaf), Index: p.PathBits, Siblings: sibs}
}

func bigHex(bi *big.Int) string { return "0x" + hex.EncodeToString(bi.Bytes()) }

// packedBallotMode returns the hex-packed fixture BallotMode; its low byte
// declares NumFields, which the guest and host num_fields-aware accumulators
// read from the state's config leaf.
func packedBallotMode(t *testing.T) string {
	t.Helper()
	bm, err := spectestutil.FixedBallotMode().Pack()
	qt.Assert(t, err, qt.IsNil)
	return "0x" + bm.Text(16)
}

// testElection builds an election config bound to a fresh ElGamal key.
func testElection(t *testing.T, id byte, endTime time.Time) (*types.Election, *bjjgnark.BJJ) {
	t.Helper()
	el, encKey, _ := testElectionWithKey(t, id, endTime)
	return el, encKey
}

// testElectionWithKey is testElection returning the private key too.
func testElectionWithKey(t *testing.T, id byte, endTime time.Time) (*types.Election, *bjjgnark.BJJ, *big.Int) {
	t.Helper()
	pub, priv, err := elgamal.GenerateKey(bjjgnark.New())
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
			BallotMode:   packedBallotMode(t),
			EncX:         bigHex(rx),
			EncY:         bigHex(ry),
			CensusOrigin: 1,
			CensusRoot:   testCensusRoot(t),
			// Real VK: the BallotVKHash config leaf is derived from it at genesis.
			VK: ballotproof.CircomVerificationKey,
		},
	}, encKey, priv
}

// makeSub builds a valid vote submission for voter i under encKey.
func makeSub(t *testing.T, encKey *bjjgnark.BJJ, i int) *VoteSubmission {
	t.Helper()
	return makeVote(t, encKey, i, byte(0xa0+i))
}

// testVoteID is the vote-ID state key ending in id.
func testVoteID(id byte) uint64 { return uint64(id) | davinci.VoteIDMin }

// testVoteIDBytes is testVoteID as the 8-byte vote_id a submission carries.
func testVoteIDBytes(id byte) []byte { return binary.BigEndian.AppendUint64(nil, testVoteID(id)) }

// makeVote builds a valid vote of voter i with the vote ID ending in voteID
// under encKey: the fixture ballot mode's active fields encrypted, the rest
// the identity.
func makeVote(t *testing.T, encKey *bjjgnark.BJJ, i int, voteID byte) *VoteSubmission {
	t.Helper()
	var msg [davinci.NumFields]*big.Int
	for j := range msg {
		msg[j] = big.NewInt(int64(10*i + j))
	}
	b, err := elgamal.NewBallot(encKey).Encrypt(msg, encKey, big.NewInt(int64(voteID)))
	qt.Assert(t, err, qt.IsNil)
	for j := int(spectestutil.FixedBallotMode().NumFields); j < davinci.NumFields; j++ {
		b.Ciphertexts[j] = elgamal.NewCiphertext(encKey)
	}
	return &VoteSubmission{
		VoteID:       testVoteIDBytes(voteID),
		Address:      testAddress(i),
		VoteIDKey:    testVoteID(voteID),
		Ballot:       b.Serialize(),
		Proof:        json.RawMessage(`{"pi_a":[]}`),
		PublicInputs: []string{"0"},
		Sig:          json.RawMessage(`{"r":"0"}`),
		Census:       censusProof(t, i),
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

	st, err := s.VoteStatus(el.ID, types.VoteID(testVoteIDBytes(0xa0)))
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

	el, _, priv := testElectionWithKey(t, 0x06, time.Now().Add(time.Hour))
	c.Assert(e.CreateElection("admin", el), qt.IsNil)

	// Ciphertext is not served before the election reaches Decrypting.
	_, err := e.EncryptedResults(el.ID)
	c.Assert(err, qt.Not(qt.IsNil))

	// A decryption key is rejected while the election is still Active.
	c.Assert(e.SubmitDecryptionKey("kw", el.ID, priv), qt.ErrorIs, ErrInvalidTransition)

	// Once Decrypting, the NumFields ElGamal ciphertexts (4 coords each) are served.
	c.Assert(s.SetElectionStatus(el.ID, types.StatusDecrypting), qt.IsNil)
	ct, err := e.EncryptedResults(el.ID)
	c.Assert(err, qt.IsNil)
	c.Assert(len(ct), qt.Equals, davinci.NumFields*4)

	// Without a scheduler the key cannot be finalized, and the status is left at
	// Decrypting so a real keywarden could retry against a worker-backed engine.
	c.Assert(e.SubmitDecryptionKey("kw", el.ID, priv), qt.ErrorMatches, ".*no worker pool.*")
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

func TestNextBatch(t *testing.T) {
	c := qt.New(t)
	vote := func(id byte, slot uint64) *types.Vote { return &types.Vote{ID: types.VoteID{id}, Slot: slot} }
	ids := func(vs []*types.Vote) []byte {
		out := []byte{}
		for _, v := range vs {
			out = append(out, v.ID[0])
		}
		return out
	}
	// Votes 0, 1 and 3 share slot 1.
	pending := []*types.Vote{vote(0, 1), vote(1, 1), vote(2, 2), vote(3, 1), vote(4, 3)}
	for _, want := range [][2][]byte{
		{{0, 2}, {1, 3, 4}},
		{{1, 4}, {3}},
		{{3}, {}},
	} {
		batch, rest := nextBatch(pending, 2)
		c.Assert(ids(batch), qt.DeepEquals, want[0])
		c.Assert(ids(rest), qt.DeepEquals, want[1])
		pending = rest
	}
}

// batchVotes returns the last byte of each vote ID and the overwrite count of
// a sealed batch.
func batchVotes(t *testing.T, s *storage.Storage, id types.ElectionID, seq uint64) ([]string, uint64) {
	t.Helper()
	bi, err := s.BatchInput(id, seq)
	qt.Assert(t, err, qt.IsNil)
	var req davinci.ProveRequest
	qt.Assert(t, json.Unmarshal(bi.ProveRequest, &req), qt.IsNil)
	ids := make([]string, len(bi.VoteIDs))
	for i, v := range bi.VoteIDs {
		ids[i] = hex.EncodeToString(v[len(v)-1:])
	}
	return ids, req.State.OverwrittenCount
}

// TestOverwriteSealsInNextBatch checks two votes of one address never share a
// batch: with a batch size of 2 they are sealed as two batches, the later
// vote second, overwriting the first.
func TestOverwriteSealsInNextBatch(t *testing.T) {
	c := qt.New(t)
	e, s := newTestEngine(t)
	defer e.Stop()

	el, encKey := testElection(t, 0x07, time.Now().Add(time.Hour))
	c.Assert(e.CreateElection("admin", el), qt.IsNil)
	_, err := e.SubmitVote(el.ID, makeVote(t, encKey, 0, 0xa0))
	c.Assert(err, qt.IsNil)
	_, err = e.SubmitVote(el.ID, makeVote(t, encKey, 0, 0xb0))
	c.Assert(err, qt.IsNil)

	// Two votes on one slot do not fill a batch of two.
	_, err = s.BatchInput(el.ID, 0)
	c.Assert(err, qt.Equals, storage.ErrNotFound)

	rt, _ := e.runtime(el.ID)
	c.Assert(e.SetStatus("admin", el.ID, types.StatusEnded), qt.IsNil)

	ids, overwrites := batchVotes(t, s, el.ID, 0)
	c.Assert(ids, qt.DeepEquals, []string{"a0"})
	c.Assert(overwrites, qt.Equals, uint64(0))
	ids, overwrites = batchVotes(t, s, el.ID, 1)
	c.Assert(ids, qt.DeepEquals, []string{"b0"})
	c.Assert(overwrites, qt.Equals, uint64(1))
	voters, overwrites := rt.state.Voters()
	c.Assert(voters, qt.Equals, uint64(2))
	c.Assert(overwrites, qt.Equals, uint64(1))
}

// TestOverwriteDoesNotHoldBatch checks a deferred overwrite lets other voters
// fill the batch and is sealed in a later one.
func TestOverwriteDoesNotHoldBatch(t *testing.T) {
	c := qt.New(t)
	e, s := newTestEngine(t)
	defer e.Stop()

	el, encKey := testElection(t, 0x08, time.Now().Add(time.Hour))
	c.Assert(e.CreateElection("admin", el), qt.IsNil)
	for _, sub := range []*VoteSubmission{
		makeVote(t, encKey, 0, 0xa0),
		makeVote(t, encKey, 0, 0xb0),
		makeVote(t, encKey, 1, 0xa1),
	} {
		_, err := e.SubmitVote(el.ID, sub)
		c.Assert(err, qt.IsNil)
	}

	ids, _ := batchVotes(t, s, el.ID, 0)
	c.Assert(ids, qt.DeepEquals, []string{"a0", "a1"})
	_, err := s.BatchInput(el.ID, 1)
	c.Assert(err, qt.Equals, storage.ErrNotFound)

	// The overwrite goes out with the next time-window seal.
	time.Sleep(20 * time.Millisecond)
	e.tick()
	ids, overwrites := batchVotes(t, s, el.ID, 1)
	c.Assert(ids, qt.DeepEquals, []string{"b0"})
	c.Assert(overwrites, qt.Equals, uint64(1))
}

// TestCreateElectionVK checks the election's ballot vk: the davinci-circom key
// when the request has none, and a key ingest cannot use is refused.
func TestCreateElectionVK(t *testing.T) {
	c := qt.New(t)
	e, s := newTestEngine(t)
	defer e.Stop()

	el, _ := testElection(t, 0x09, time.Now().Add(time.Hour))
	el.Config.VK = nil
	c.Assert(e.CreateElection("admin", el), qt.IsNil)
	got, err := s.Election(el.ID)
	c.Assert(err, qt.IsNil)
	c.Assert(got.Config.VK, qt.DeepEquals, []byte(canonicalCircomVK(t)))

	// A JSON null is no vk either.
	el, _ = testElection(t, 0x0c, time.Now().Add(time.Hour))
	el.Config.VK = []byte("null")
	c.Assert(e.CreateElection("admin", el), qt.IsNil)
	got, err = s.Election(el.ID)
	c.Assert(err, qt.IsNil)
	c.Assert(got.Config.VK, qt.DeepEquals, []byte(canonicalCircomVK(t)))

	el, _ = testElection(t, 0x0a, time.Now().Add(time.Hour))
	el.Config.VK = []byte(`{"vk_alpha_1": ["1"]}`)
	c.Assert(e.CreateElection("admin", el), qt.ErrorMatches, "invalid election config: vk: .*")

	// A key for a circuit with another public-input count.
	var vk map[string]any
	c.Assert(json.Unmarshal(ballotproof.CircomVerificationKey, &vk), qt.IsNil)
	vk["IC"] = vk["IC"].([]any)[:3]
	el, _ = testElection(t, 0x0b, time.Now().Add(time.Hour))
	el.Config.VK, err = json.Marshal(vk)
	c.Assert(err, qt.IsNil)
	c.Assert(e.CreateElection("admin", el), qt.ErrorMatches, "invalid election config: vk: want 3 public inputs, got 2")
}

// TestCreateElectionBatchSize checks an election without a batch size takes
// the engine default, and one outside 2..MaxBatchSize is refused.
func TestCreateElectionBatchSize(t *testing.T) {
	e, s := newTestEngine(t)
	defer e.Stop()
	for i, tc := range []struct {
		size    int
		want    int
		wantErr string
	}{
		{size: 0, want: 2},
		{size: 1, wantErr: "batch size 1 is below the minimum 2"},
		{size: 3, want: 3},
		{size: davinci.MaxBatchSize, want: davinci.MaxBatchSize},
		{size: davinci.MaxBatchSize + 1, wantErr: "exceeds circuit maximum"},
	} {
		el, _ := testElection(t, byte(0x70+i), time.Now().Add(time.Hour))
		el.BatchSize = tc.size
		err := e.CreateElection("admin", el)
		assertErr(t, err, tc.wantErr)
		if tc.wantErr != "" {
			_, err := s.Election(el.ID)
			qt.Assert(t, err, qt.ErrorIs, storage.ErrNotFound)
			continue
		}
		got, err := s.Election(el.ID)
		qt.Assert(t, err, qt.IsNil)
		qt.Assert(t, got.BatchSize, qt.Equals, tc.want)
	}
}

// canonicalCircomVK is the davinci-circom key as elections store it.
func canonicalCircomVK(t *testing.T) json.RawMessage {
	t.Helper()
	_, vk, err := parseBallotVK(ballotproof.CircomVerificationKey)
	qt.Assert(t, err, qt.IsNil)
	return vk
}
