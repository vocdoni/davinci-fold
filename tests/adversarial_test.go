package tests

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/fxamacker/cbor/v2"
	leanimt "github.com/vocdoni/lean-imt-go"

	"github.com/vocdoni/davinci-node/db"
	"github.com/vocdoni/davinci-node/db/metadb"

	"github.com/vocdoni/davinci-fold/orchestrator"
	"github.com/vocdoni/davinci-fold/storage"
	"github.com/vocdoni/davinci-fold/types"
	davinci "github.com/vocdoni/davinci-zkvm/go-sdk"
	"github.com/vocdoni/davinci-zkvm/go-sdk/tests/integration"
	"github.com/vocdoni/davinci-zkvm/go-sdk/vocdoni/circuits/ballotproof"
	bjjgnark "github.com/vocdoni/davinci-zkvm/go-sdk/vocdoni/crypto/ecc/bjj_gnark"
	"github.com/vocdoni/davinci-zkvm/go-sdk/vocdoni/crypto/elgamal"
	spectestutil "github.com/vocdoni/davinci-zkvm/go-sdk/vocdoni/spec/testutil"
)

// TestAdversarialIngest drives real Groth16 ballots through a standalone,
// worker-less engine running the production cryptoValidator and asserts that
// every manipulated submission is rejected at ingest while the clean ballots
// are accepted. It needs no GPU: ballot generation is CPU rapidsnark and the
// validator's proof check is gnark on the CPU, so it runs whenever the
// integration suite is enabled (RUN_INTEGRATION_TESTS), with or without
// prover workers.
func TestAdversarialIngest(t *testing.T) {
	c := qt.New(t)

	// Two real voters with real ballots. Voter 0 is the "honest" baseline;
	// voter 1 is mutated across the rejection cases (each rejection happens
	// before persistence, so voter 1's vote ID stays free until the final
	// clean submission).
	election, err := integration.NewElection(2)
	c.Assert(err, qt.IsNil)
	batch, err := integration.GenerateBallotBatch(election.ProcessID, election.EncKey, election.Voters, 42)
	c.Assert(err, qt.IsNil)
	census, err := election.BuildCensusProofs(election.Voters)
	c.Assert(err, qt.IsNil)

	sub0 := voteSubmission(election.Voters[0], batch.Results[0], census[0])
	sub1 := voteSubmission(election.Voters[1], batch.Results[1], census[1])

	// Standalone ingest-only engine: production cryptoValidator (nil Validator),
	// no pool (no scheduler/dispatch), batch size large enough that nothing
	// seals, end time far in the future so the monitor never ends the election.
	engine, el := newIngestEngine(c, election, json.RawMessage(ballotproof.CircomVerificationKey))
	defer engine.Stop()

	// 1. The honest baseline ballot is accepted.
	_, err = engine.SubmitVote(el.ID, sub0)
	c.Assert(err, qt.IsNil, qt.Commentf("honest ballot must be accepted"))

	// 2. Resending the same vote ID is rejected as a duplicate.
	_, err = engine.SubmitVote(el.ID, sub0)
	assertRejected(t, err, "duplicate vote")

	// 3. Submitting to an unknown election is rejected.
	_, err = engine.SubmitVote(types.ElectionID{0xde, 0xad}, clone(sub1))
	assertRejected(t, err, "unknown election")

	// 4. Wrong address length.
	bad := clone(sub1)
	bad.Address = bad.Address[:19]
	_, err = engine.SubmitVote(el.ID, bad)
	assertRejected(t, err, "address must be")

	// 5. Malformed ballot ciphertext (truncated) fails deserialization.
	bad = clone(sub1)
	bad.Ballot = bad.Ballot[:len(bad.Ballot)-5]
	_, err = engine.SubmitVote(el.ID, bad)
	assertRejected(t, err, "malformed ballot")

	// 6. Wrong public-signal count.
	bad = clone(sub1)
	bad.PublicInputs = bad.PublicInputs[:2]
	_, err = engine.SubmitVote(el.ID, bad)
	assertRejected(t, err, "public_inputs")

	// 7. Census root does not match the election's census.
	bad = clone(sub1)
	bad.Census.Root = feHex(big.NewInt(0xdead))
	_, err = engine.SubmitVote(el.ID, bad)
	assertRejected(t, err, "census root mismatch")

	// 7b. A census value the prover cannot parse (not 32-byte hex).
	bad = clone(sub1)
	bad.Census.Root = "0xdead"
	_, err = engine.SubmitVote(el.ID, bad)
	assertRejected(t, err, "census root: want 32 bytes")

	// 7c. The right root with a path that does not lead to it: a non-member
	// cannot get a ballot into a batch the guest would then reject.
	bad = clone(sub1)
	bad.Census.Siblings = append([]string(nil), sub1.Census.Siblings...)
	bad.Census.Siblings[0] = feHex(big.NewInt(12345))
	_, err = engine.SubmitVote(el.ID, bad)
	assertRejected(t, err, "census proof does not reach the census root")

	// 7d. Another member's census proof does not authorize this address.
	bad = clone(sub1)
	bad.Census = sub0.Census
	_, err = engine.SubmitVote(el.ID, bad)
	assertRejected(t, err, "census leaf is for address")

	// 8. Address does not match the proof's public signal (identity swap,
	// with the census proof of the claimed address).
	bad = clone(sub1)
	bad.Address = append([]byte(nil), election.Voters[0].AddressBytes...)
	bad.Census = sub0.Census
	_, err = engine.SubmitVote(el.ID, bad)
	assertRejected(t, err, "public_inputs address")

	// 9. Vote-ID state-tree key does not match the vote ID.
	bad = clone(sub1)
	bad.VoteIDKey ^= 1
	_, err = engine.SubmitVote(el.ID, bad)
	assertRejected(t, err, "vote_id_key")

	// 10. A path bit above the census proof's depth: the Merkle walk ignores
	// it, but the guest rejects the proof.
	bad = clone(sub1)
	bad.Census.Index |= 1 << uint(len(bad.Census.Siblings))
	_, err = engine.SubmitVote(el.ID, bad)
	assertRejected(t, err, "path bits above depth")

	// 11. A valid signature from a different voter does not authenticate.
	bad = clone(sub1)
	bad.Sig = sub0.Sig
	_, err = engine.SubmitVote(el.ID, bad)
	assertRejected(t, err, "signature verification failed")

	// 12. A tampered Groth16 ballot proof: a coordinate off the curve fails
	// to parse, valid points in the wrong places fail verification.
	bad = clone(sub1)
	bad.Proof = editJSON(c, sub1.Proof, func(m map[string]any) { m["pi_a"].([]any)[0] = "12345" })
	_, err = engine.SubmitVote(el.ID, bad)
	assertRejected(t, err, "ballot proof: pi_a: G1 point not on the curve")
	bad.Proof = editJSON(c, sub1.Proof, func(m map[string]any) { m["pi_a"], m["pi_c"] = m["pi_c"], m["pi_a"] })
	_, err = engine.SubmitVote(el.ID, bad)
	assertRejected(t, err, "ballot proof: invalid proof")

	// 13. A proof point the prover would read as the identity.
	bad.Proof = editJSON(c, sub1.Proof, func(m map[string]any) { m["pi_c"].([]any)[2] = "0" })
	_, err = engine.SubmitVote(el.ID, bad)
	assertRejected(t, err, "ballot proof: pi_c: G1 point must be")

	// 14. A public signal the prover cannot parse as a decimal field element.
	bad = clone(sub1)
	bad.PublicInputs = []string{sub1.PublicInputs[0], sub1.PublicInputs[1], "+" + sub1.PublicInputs[2]}
	_, err = engine.SubmitVote(el.ID, bad)
	assertRejected(t, err, "public_inputs: [2]")

	// 15. A valid proof with another ballot: the inputs hash no longer
	// matches, which the guest would reject for the whole batch.
	bad = clone(sub1)
	bad.Ballot = sub0.Ballot
	_, err = engine.SubmitVote(el.ID, bad)
	assertRejected(t, err, "inputs hash does not match")

	// 16. A padded field (past the ballot mode's fields) that is not the
	// identity. The ballot proof leaves padded fields unconstrained.
	bad = clone(sub1)
	bad.Ballot = editBallot(c, sub1.Ballot, func(b *elgamal.Ballot) {
		b.Ciphertexts[davinci.NumFields-1] = b.Ciphertexts[0]
	})
	_, err = engine.SubmitVote(el.ID, bad)
	assertRejected(t, err, "field 15 must be the identity")

	// 17. A vote ID with a second encoding (a leading zero byte) of the same
	// state key.
	bad = clone(sub1)
	bad.VoteID = append([]byte{0}, sub1.VoteID...)
	_, err = engine.SubmitVote(el.ID, bad)
	assertRejected(t, err, "vote_id must be 8 bytes")

	// 18. A recovery id the guest cannot use.
	bad = clone(sub1)
	bad.Sig = editJSON(c, sub1.Sig, func(m map[string]any) { m["signature_v"] = 2 })
	_, err = engine.SubmitVote(el.ID, bad)
	assertRejected(t, err, "recovery id 2")

	// 19. The voter-1 ballot is accepted, proving the mutations above were
	// the sole cause of each rejection. It is sent in encodings the prover
	// would not parse (recovery id 27/28, r without 0x in upper case, a proof
	// without "curve" and "protocol", an upper-case census root), which
	// ingest stores re-encoded.
	odd := clone(sub1)
	var sig map[string]any
	c.Assert(json.Unmarshal(sub1.Sig, &sig), qt.IsNil)
	recid := int(sig["signature_v"].(float64))
	odd.Sig = editJSON(c, sub1.Sig, func(m map[string]any) {
		m["signature_v"] = 27 + recid
		m["signature_r"] = strings.ToUpper(strings.TrimPrefix(m["signature_r"].(string), "0x"))
	})
	odd.Proof = editJSON(c, sub1.Proof, func(m map[string]any) { delete(m, "curve"); delete(m, "protocol") })
	odd.Census.Root = strings.ToUpper(sub1.Census.Root[2:])
	v, err := engine.SubmitVote(el.ID, odd)
	c.Assert(err, qt.IsNil, qt.Commentf("clean voter-1 ballot must be accepted"))

	var stored struct {
		Proof json.RawMessage `cbor:"proof"`
		Sig   json.RawMessage `cbor:"sig"`
	}
	c.Assert(cbor.Unmarshal(v.Payload, &stored), qt.IsNil)
	var storedProof, storedSig map[string]any
	c.Assert(json.Unmarshal(stored.Proof, &storedProof), qt.IsNil)
	c.Assert(json.Unmarshal(stored.Sig, &storedSig), qt.IsNil)
	c.Assert(storedSig["signature_v"], qt.Equals, float64(recid))
	c.Assert(storedSig["signature_r"], qt.Equals, strings.ToLower(sig["signature_r"].(string)))
	c.Assert(storedSig["vote_id"], qt.Equals, float64(binary.BigEndian.Uint64(sub1.VoteID)))
	c.Assert(storedProof["curve"], qt.Equals, "bn128")
	c.Assert(storedProof["protocol"], qt.Equals, "groth16")
}

// TestIngestCensusWeight checks the ballot proof is bound to the weight the
// census leaf carries: a member whose leaf weight differs from the one the
// proof committed to is rejected, as the guest would reject its batch.
func TestIngestCensusWeight(t *testing.T) {
	c := qt.New(t)
	election, err := integration.NewElection(2)
	c.Assert(err, qt.IsNil)
	batch, err := integration.GenerateBallotBatch(election.ProcessID, election.EncKey, election.Voters, 42)
	c.Assert(err, qt.IsNil)

	// The same voters with weight 43; the proofs committed to 42.
	tree, err := leanimt.New(leanimt.PoseidonHasher, leanimt.BigIntEqual, nil, nil, nil)
	c.Assert(err, qt.IsNil)
	for _, v := range election.Voters {
		tree.Insert(davinci.PackAddressWeight(v.AddressBigInt, big.NewInt(43)))
	}
	root, _ := tree.Root()
	p, err := tree.GenerateProof(1)
	c.Assert(err, qt.IsNil)
	census := davinci.CensusProof{Root: feHex(root), Leaf: feHex(p.Leaf), Index: p.PathBits}
	for _, s := range p.Siblings {
		census.Siblings = append(census.Siblings, feHex(s))
	}

	engine, el := newIngestEngineWithRoot(c, election, nil, root)
	defer engine.Stop()
	_, err = engine.SubmitVote(el.ID, voteSubmission(election.Voters[1], batch.Results[1], census))
	assertRejected(t, err, "inputs hash does not match")
}

// editJSON returns raw with edit applied to it as a JSON object.
func editJSON(c *qt.C, raw json.RawMessage, edit func(m map[string]any)) json.RawMessage {
	var m map[string]any
	c.Assert(json.Unmarshal(raw, &m), qt.IsNil)
	edit(m)
	out, err := json.Marshal(m)
	c.Assert(err, qt.IsNil)
	return out
}

// editBallot returns a serialized ballot with edit applied to it.
func editBallot(c *qt.C, raw []byte, edit func(b *elgamal.Ballot)) []byte {
	b := elgamal.NewBallot(bjjgnark.New())
	c.Assert(b.Deserialize(raw), qt.IsNil)
	edit(b)
	return b.Serialize()
}

// newIngestEngine builds a worker-less engine with the production validator
// and creates election's process in it with the given ballot vk.
func newIngestEngine(c *qt.C, election *integration.Election, vk json.RawMessage) (*orchestrator.Engine, *types.Election) {
	censusRoot, ok := election.Census.Root()
	c.Assert(ok, qt.IsTrue)
	return newIngestEngineWithRoot(c, election, vk, censusRoot)
}

// newIngestEngineWithRoot is newIngestEngine with another census root.
func newIngestEngineWithRoot(c *qt.C, election *integration.Election, vk json.RawMessage, censusRoot *big.Int) (*orchestrator.Engine, *types.Election) {
	database, err := metadb.New(db.TypeInMem, "")
	c.Assert(err, qt.IsNil)
	engine, err := orchestrator.NewEngine(storage.New(database), orchestrator.Options{
		BatchSize:       1000,
		BatchTimeWindow: time.Hour,
	})
	c.Assert(err, qt.IsNil)

	encX, encY := election.EncKey.Point()
	bm, err := spectestutil.FixedBallotMode().Pack()
	c.Assert(err, qt.IsNil)
	el := &types.Election{
		ID:        types.ElectionID(election.ProcessID[:]),
		BatchSize: 1000,
		FoldEvery: 1,
		EndTime:   time.Now().Add(time.Hour),
		Config: types.ElectionConfig{
			ProcessID:    "0x" + hex.EncodeToString(election.ProcessID[:]),
			BallotMode:   "0x" + bm.Text(16),
			EncX:         "0x" + encX.Text(16),
			EncY:         "0x" + encY.Text(16),
			CensusOrigin: uint64(election.CensusOrigin),
			CensusRoot:   feHex(censusRoot),
			VK:           vk,
		},
	}
	c.Assert(engine.CreateElection("admin", el), qt.IsNil)
	return engine, el
}

// clone returns a shallow copy of a submission. Mutating the copy's fields by
// assignment (not by writing through shared slices) leaves the original intact.
func clone(s *orchestrator.VoteSubmission) *orchestrator.VoteSubmission {
	cp := *s
	return &cp
}

// assertRejected fails unless err is non-nil and mentions want.
func assertRejected(t *testing.T, err error, want string) {
	t.Helper()
	qt.Assert(t, err, qt.IsNotNil)
	qt.Assert(t, strings.Contains(err.Error(), want), qt.IsTrue,
		qt.Commentf("want error containing %q, got %v", want, err))
}
