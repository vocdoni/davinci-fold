package tests

import (
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"

	"github.com/vocdoni/davinci-node/db"
	"github.com/vocdoni/davinci-node/db/metadb"

	"github.com/vocdoni/davinci-fold/orchestrator"
	"github.com/vocdoni/davinci-fold/storage"
	"github.com/vocdoni/davinci-fold/types"
	"github.com/vocdoni/davinci-zkvm/go-sdk/tests/integration"
	"github.com/vocdoni/davinci-zkvm/go-sdk/vocdoni/circuits/ballotproof"
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
	database, err := metadb.New(db.TypeInMem, "")
	c.Assert(err, qt.IsNil)
	store := storage.New(database)
	engine, err := orchestrator.NewEngine(store, orchestrator.Options{
		BatchSize:       1000,
		BatchTimeWindow: time.Hour,
	})
	c.Assert(err, qt.IsNil)
	defer engine.Stop()

	censusRoot, ok := election.Census.Root()
	c.Assert(ok, qt.IsTrue)
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
			VK:           json.RawMessage(ballotproof.CircomVerificationKey),
		},
	}
	c.Assert(engine.CreateElection("admin", el), qt.IsNil)

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
	bad.Census.Root = "0xdead"
	_, err = engine.SubmitVote(el.ID, bad)
	assertRejected(t, err, "census root mismatch")

	// 8. Address does not match the proof's public signal (identity swap).
	bad = clone(sub1)
	bad.Address = append([]byte(nil), election.Voters[0].AddressBytes...)
	_, err = engine.SubmitVote(el.ID, bad)
	assertRejected(t, err, "public_inputs address")

	// 9. Vote-ID state-tree key does not match the vote ID.
	bad = clone(sub1)
	bad.VoteIDKey ^= 1
	_, err = engine.SubmitVote(el.ID, bad)
	assertRejected(t, err, "vote_id_key")

	// 10. Address-low-16 state-tree key does not match the address.
	bad = clone(sub1)
	bad.AddressLo16 ^= 1
	_, err = engine.SubmitVote(el.ID, bad)
	assertRejected(t, err, "address_lo16")

	// 11. A valid signature from a different voter does not authenticate.
	bad = clone(sub1)
	bad.Sig = sub0.Sig
	_, err = engine.SubmitVote(el.ID, bad)
	assertRejected(t, err, "signature verification failed")

	// 12. A tampered Groth16 ballot proof fails verification.
	bad = clone(sub1)
	bad.Proof = tamperProof(c, sub1.Proof)
	_, err = engine.SubmitVote(el.ID, bad)
	assertRejected(t, err, "ballot proof")

	// 13. The untouched voter-1 ballot is accepted, proving the mutations above
	// were the sole cause of each rejection.
	_, err = engine.SubmitVote(el.ID, sub1)
	c.Assert(err, qt.IsNil, qt.Commentf("clean voter-1 ballot must be accepted"))
}

// clone returns a shallow copy of a submission. Mutating the copy's fields by
// assignment (not by writing through shared slices) leaves the original intact.
func clone(s *orchestrator.VoteSubmission) *orchestrator.VoteSubmission {
	cp := *s
	return &cp
}

// tamperProof corrupts the first coordinate of a snarkjs Groth16 proof while
// keeping the JSON structurally valid, so the proof is rejected by the
// verifier rather than by the decoder.
func tamperProof(c *qt.C, raw json.RawMessage) json.RawMessage {
	var m map[string]any
	c.Assert(json.Unmarshal(raw, &m), qt.IsNil)
	a, ok := m["pi_a"].([]any)
	c.Assert(ok, qt.IsTrue)
	c.Assert(len(a) > 0, qt.IsTrue)
	a[0] = "12345"
	out, err := json.Marshal(m)
	c.Assert(err, qt.IsNil)
	return out
}

// assertRejected fails unless err is non-nil and mentions want.
func assertRejected(t *testing.T, err error, want string) {
	t.Helper()
	qt.Assert(t, err, qt.IsNotNil)
	qt.Assert(t, strings.Contains(err.Error(), want), qt.IsTrue,
		qt.Commentf("want error containing %q, got %v", want, err))
}
