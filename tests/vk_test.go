package tests

import (
	"encoding/json"
	"math/big"
	"testing"

	"github.com/consensys/gnark-crypto/ecc/bn254"
	"github.com/consensys/gnark-crypto/ecc/bn254/fp"
	"github.com/consensys/gnark-crypto/ecc/bn254/fr"
	qt "github.com/frankban/quicktest"

	"github.com/vocdoni/davinci-fold/orchestrator"
	"github.com/vocdoni/davinci-zkvm/go-sdk/tests/integration"
	"github.com/vocdoni/davinci-zkvm/go-sdk/vocdoni/circuits/ballotproof"
)

// TestBallotVKPerElection checks ingest verifies ballot proofs against the
// election's own vk: a proof valid under it is accepted, one valid only under
// another key is rejected. The second key and its proof come from the real
// ballot proof by rescaling: delta' = delta/s and C' = s*C keep
// e(C', delta') = e(C, delta), so the rescaled proof verifies under the
// rescaled key and not under the davinci-circom one, and the other way round.
func TestBallotVKPerElection(t *testing.T) {
	c := qt.New(t)

	election, err := integration.NewElection(1)
	c.Assert(err, qt.IsNil)
	batch, err := integration.GenerateBallotBatch(election.ProcessID, election.EncKey, election.Voters, 42)
	c.Assert(err, qt.IsNil)
	census, err := election.BuildCensusProofs(election.Voters)
	c.Assert(err, qt.IsNil)
	circomSub := voteSubmission(election.Voters[0], batch.Results[0], census[0])

	otherVK, otherProof := rescaleBallotProof(c, ballotproof.CircomVerificationKey, circomSub.Proof, big.NewInt(7))
	otherSub := clone(circomSub)
	otherSub.Proof = otherProof

	for _, tc := range []struct {
		name   string
		vk     json.RawMessage
		accept *orchestrator.VoteSubmission
		reject *orchestrator.VoteSubmission
	}{
		// No vk in the request: the davinci-circom key.
		{name: "default key", vk: nil, accept: circomSub, reject: otherSub},
		{name: "other key", vk: otherVK, accept: otherSub, reject: circomSub},
	} {
		c.Run(tc.name, func(c *qt.C) {
			engine, el := newIngestEngine(c, election, tc.vk)
			defer engine.Stop()
			_, err := engine.SubmitVote(el.ID, tc.reject)
			c.Assert(err, qt.ErrorIs, orchestrator.ErrInvalidBallotProof)
			c.Assert(err, qt.ErrorMatches, "invalid ballot proof: it does not verify under the election's vk")
			_, err = engine.SubmitVote(el.ID, tc.accept)
			c.Assert(err, qt.IsNil)
		})
	}
}

// rescaleBallotProof returns a snarkjs vk and proof with vk_delta_2 scaled by
// 1/s and pi_c by s.
func rescaleBallotProof(c *qt.C, vkJSON, proofJSON []byte, s *big.Int) (json.RawMessage, json.RawMessage) {
	sInv := new(big.Int).ModInverse(s, fr.Modulus())

	var vk map[string]any
	c.Assert(json.Unmarshal(vkJSON, &vk), qt.IsNil)
	var delta bn254.G2Affine
	d := vk["vk_delta_2"].([]any)
	setFp(c, &delta.X.A0, d[0].([]any)[0])
	setFp(c, &delta.X.A1, d[0].([]any)[1])
	setFp(c, &delta.Y.A0, d[1].([]any)[0])
	setFp(c, &delta.Y.A1, d[1].([]any)[1])
	delta.ScalarMultiplication(&delta, sInv)
	vk["vk_delta_2"] = []any{
		[]any{fpString(&delta.X.A0), fpString(&delta.X.A1)},
		[]any{fpString(&delta.Y.A0), fpString(&delta.Y.A1)},
		[]any{"1", "0"},
	}
	outVK, err := json.Marshal(vk)
	c.Assert(err, qt.IsNil)

	var proof map[string]any
	c.Assert(json.Unmarshal(proofJSON, &proof), qt.IsNil)
	var pc bn254.G1Affine
	p := proof["pi_c"].([]any)
	setFp(c, &pc.X, p[0])
	setFp(c, &pc.Y, p[1])
	pc.ScalarMultiplication(&pc, s)
	proof["pi_c"] = []any{fpString(&pc.X), fpString(&pc.Y), "1"}
	outProof, err := json.Marshal(proof)
	c.Assert(err, qt.IsNil)
	return outVK, outProof
}

// setFp sets e from a decimal snarkjs coordinate.
func setFp(c *qt.C, e *fp.Element, v any) {
	str, ok := v.(string)
	c.Assert(ok, qt.IsTrue)
	_, err := e.SetString(str)
	c.Assert(err, qt.IsNil)
}

// fpString renders a coordinate the way snarkjs does, in decimal.
func fpString(e *fp.Element) string { return e.BigInt(new(big.Int)).String() }
