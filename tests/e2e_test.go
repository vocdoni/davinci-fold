package tests

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"

	"github.com/vocdoni/davinci-node/circuits/ballotproof"
	bjjgnark "github.com/vocdoni/davinci-node/crypto/ecc/bjj_gnark"
	"github.com/vocdoni/davinci-node/crypto/elgamal"

	"github.com/vocdoni/davinci-fold/api"
	"github.com/vocdoni/davinci-fold/orchestrator"
	"github.com/vocdoni/davinci-fold/tests/helpers"
	davinci "github.com/vocdoni/davinci-zkvm/go-sdk"
	davinciSolidity "github.com/vocdoni/davinci-zkvm/go-sdk/solidity"
	"github.com/vocdoni/davinci-zkvm/go-sdk/tests/integration"
)

// requireWorkers skips the calling test unless at least n prover workers were
// supplied via DAVINCI_FOLD_WORKER_URLS. The scatter/gather and chaos tests
// need real GPU provers and cannot run without them.
func requireWorkers(t *testing.T, n int) {
	t.Helper()
	if len(workerURLs) < n {
		t.Skipf("needs >=%d prover workers in DAVINCI_FOLD_WORKER_URLS (have %d)", n, len(workerURLs))
	}
}

// TestScatterGatherE2E drives a full chained-mode election through davinci-fold
// against a real GPU prover: it generates real Groth16 ballots (with
// overwrites), submits them over the self-authenticating vote API, lets the
// lifecycle monitor end the election and drain the scattered batch STARKs onto
// the fold chain, hands the keywarden's decryption key in to trigger finalize,
// and finally asserts the analytic net tally and verifies the final PLONK on a
// simulated EVM.
//
// One worker is enough to exercise the whole pipeline; the scatter/gather code
// path is identical with one or many workers.
func TestScatterGatherE2E(t *testing.T) {
	requireWorkers(t, 1)
	c := qt.New(t)
	ctx := context.Background()
	admin := helpers.AdminToken()
	keywarden := helpers.KeywardenToken()

	nBatches := envInt("E2E_BATCHES", 3)
	batchSize := envInt("E2E_BATCH_SIZE", 2)
	foldEvery := envInt("E2E_FOLD_EVERY", 1)
	overwriteBatches := envInt("E2E_OVERWRITE_BATCHES", 1)
	c.Assert(overwriteBatches < nBatches, qt.IsTrue, qt.Commentf("overwrite batches must be fewer than total batches"))

	// Distinct voters: the overwrite batches re-vote the earliest voters, so
	// only (nBatches-overwriteBatches) batches introduce new census members.
	uniqueVoters := (nBatches - overwriteBatches) * batchSize
	election, err := integration.NewElection(uniqueVoters)
	c.Assert(err, qt.IsNil)

	censusRoot, ok := election.Census.Root()
	c.Assert(ok, qt.IsTrue)
	encX, encY := election.EncKey.Point()

	// Pre-generate every batch's ballots up front so the timed lifecycle window
	// only covers fast HTTP submission, not the slow rapidsnark proving.
	type genBatch struct {
		voters []*integration.Voter
		batch  *integration.BatchProveComponents
		census []davinci.CensusProof
	}
	gens := make([]genBatch, nBatches)
	for b := 0; b < nBatches; b++ {
		voterStart := b * batchSize
		if overwriteBatches > 0 && b >= nBatches-overwriteBatches {
			// Re-vote the voters of an earlier batch with a fresh seed (new
			// voteIDs) so the state treats these as overwrites.
			voterStart = (b - (nBatches - overwriteBatches)) * batchSize
		}
		voters := election.Voters[voterStart : voterStart+batchSize]
		batch, err := integration.GenerateBallotBatch(election.ProcessID, election.EncKey, voters, int64(42+100*b))
		c.Assert(err, qt.IsNil, qt.Commentf("GenerateBallotBatch batch %d", b))
		census, err := election.BuildCensusProofs(voters)
		c.Assert(err, qt.IsNil)
		gens[b] = genBatch{voters: voters, batch: batch, census: census}
	}

	// Register the GPU prover so sealed batches have somewhere to scatter to.
	c.Assert(services.Client.RegisterWorker(ctx, admin, workerURLs[0], "gpu-0"), qt.IsNil)

	// Create the election. ProcessID/EncKey/census root must match the locally
	// built genesis so davinci-fold's State and the validator agree.
	createReq := &api.ElectionCreateRequest{
		ProcessID:    "0x" + hex.EncodeToString(election.ProcessID[:]),
		BallotMode:   "0x01",
		EncX:         "0x" + encX.Text(16),
		EncY:         "0x" + encY.Text(16),
		CensusOrigin: uint64(election.CensusOrigin),
		CensusRoot:   feHex(censusRoot),
		VK:           json.RawMessage(ballotproof.CircomVerificationKey),
		BatchSize:    batchSize,
		FoldEvery:    foldEvery,
		// Generous window: stays Active through submission, then ends so the
		// monitor drains and publishes. Submission is fast (proving is async).
		EndTime: time.Now().Add(45 * time.Second),
	}
	el, err := services.Client.CreateElection(ctx, admin, createReq)
	c.Assert(err, qt.IsNil)
	t.Logf("created election %s (status %s, %d unique voters, %d batches)", el.ID, el.Status, uniqueVoters, nBatches)

	// Submit every ballot in generation order. Overwrites must arrive after the
	// original ballot, which the per-batch ordering guarantees.
	submitted := 0
	for b := range gens {
		g := gens[b]
		for i, v := range g.voters {
			res := g.batch.Results[i]
			sub := voteSubmission(v, res, g.census[i])
			_, err := services.Client.SubmitVote(ctx, el.ID, sub)
			c.Assert(err, qt.IsNil, qt.Commentf("SubmitVote batch %d voter %d", b, i))
			submitted++
		}
	}
	t.Logf("submitted %d votes; waiting for the election to end and drain", submitted)

	// Wait for the lifecycle monitor to end the election, drain every scattered
	// batch STARK onto the fold chain, and publish the encrypted results.
	deadline := time.Now().Add(25 * time.Minute)
	for {
		got, err := services.Client.GetElection(ctx, el.ID)
		c.Assert(err, qt.IsNil)
		if got.Status == "decrypting" || got.Status == "finalizing" || got.Status == "results" {
			t.Logf("election reached %s", got.Status)
			break
		}
		c.Assert(time.Now().Before(deadline), qt.IsTrue, qt.Commentf("timed out waiting for decrypting (last status %s)", got.Status))
		time.Sleep(2 * time.Second)
	}

	// The keywarden fetches the published ciphertext...
	ctResp, code, err := services.Client.EncryptedResults(ctx, keywarden, el.ID)
	c.Assert(err, qt.IsNil, qt.Commentf("encrypted-results status %d", code))
	c.Assert(len(ctResp.Ciphertext), qt.Equals, 32)

	// ...and returns the v1 decryption key (the raw ElGamal private scalar),
	// which triggers the GPU-bound finalize (final fold + PLONK). Widen the HTTP
	// timeout so the synchronous finalize call can complete.
	services.Client.SetTimeout(25 * time.Minute)
	results, err := services.Client.SubmitDecryptionKey(ctx, keywarden, el.ID, election.EncPrivKey)
	c.Assert(err, qt.IsNil)
	c.Assert(len(results.Tally), qt.Equals, 8)
	t.Logf("finalize complete; tally=%v", results.Tally)

	// The committed tally must equal the analytic net tally (last ballot per
	// voter wins). This is independent of davinci-fold's internal accumulator.
	expTally := expectedChainTally(nBatches, batchSize, overwriteBatches)
	for i := 0; i < 8; i++ {
		c.Assert(results.Tally[i], qt.Equals, expTally[i],
			qt.Commentf("tally field %d", i))
	}
	t.Logf("analytic net tally verified: %v", expTally)

	// Verify the final PLONK on a simulated EVM, exactly as it would verify
	// on-chain.
	snark, err := plonkFromResults(results)
	c.Assert(err, qt.IsNil)
	c.Assert(davinciSolidity.VerifyOnSimulated(solidityDir(), snark), qt.IsNil)
	t.Logf("final PLONK verified on simulated chain")
}

// voteSubmission maps a generated ballot to the self-authenticating vote body
// davinci-fold's ingest API expects. The ballot is rebuilt from its raw RTE
// ciphertexts and serialized the same way the seal path deserializes it.
func voteSubmission(v *integration.Voter, res *integration.BallotResult, census davinci.CensusProof) *orchestrator.VoteSubmission {
	ballot := elgamal.NewBallot(bjjgnark.New())
	for i := 0; i < 8; i++ {
		ballot.Ciphertexts[i] = &elgamal.Ciphertext{
			C1: bjjgnark.New().SetPoint(res.RawBallot.C1X[i], res.RawBallot.C1Y[i]),
			C2: bjjgnark.New().SetPoint(res.RawBallot.C2X[i], res.RawBallot.C2Y[i]),
		}
	}
	voteIDBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(voteIDBytes, res.VoteID)
	return &orchestrator.VoteSubmission{
		VoteID:       voteIDBytes,
		Address:      v.AddressBytes,
		CensusIdx:    v.CensusIdx,
		AddressLo16:  res.AddressLo16,
		VoteIDKey:    res.VoteID,
		Ballot:       ballot.Serialize(),
		Proof:        res.ProofJSON,
		PublicInputs: res.PublicInputs,
		Sig:          res.SigJSON,
		Census:       census,
	}
}

// feHex returns a *big.Int as a 0x-prefixed 32-byte big-endian hex string,
// matching the integration package's census-root encoding (bigIntToFr32) so
// the orchestrator's census-root binding check passes.
func feHex(v *big.Int) string {
	var buf [32]byte
	v.FillBytes(buf[:])
	return "0x" + hex.EncodeToString(buf[:])
}

// plonkFromResults reconstructs a davinci.PlonkSnark from the four Solidity-ready
// hex fields of the results response.
func plonkFromResults(r *api.ResultsResponse) (*davinci.PlonkSnark, error) {
	pvk, err := hex.DecodeString(strings.TrimPrefix(r.ProgramVK, "0x"))
	if err != nil {
		return nil, err
	}
	rc, err := hex.DecodeString(strings.TrimPrefix(r.RootCVadcopFinal, "0x"))
	if err != nil {
		return nil, err
	}
	pub, err := hex.DecodeString(strings.TrimPrefix(r.PublicValues, "0x"))
	if err != nil {
		return nil, err
	}
	pb, err := hex.DecodeString(strings.TrimPrefix(r.ProofBytes, "0x"))
	if err != nil {
		return nil, err
	}
	snark := &davinci.PlonkSnark{PublicValues: pub, ProofBytes: pb}
	copy(snark.ProgramVK[:], pvk)
	copy(snark.RootCVadcopFinal[:], rc)
	return snark, nil
}

// solidityDir resolves the davinci-zkvm solidity verifier sources (sibling repo
// of davinci-fold) for VerifyOnSimulated.
func solidityDir() string {
	_, thisFile, _, _ := runtime.Caller(0)
	// .../davinci-fold/tests/e2e_test.go → /home/p4u → davinci-zkvm/solidity
	return filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", "davinci-zkvm", "solidity"))
}

// expectedChainTally analytically computes the net vote tally: the last
// overwriteBatches batches re-vote earlier voters and only the last ballot per
// voter counts. It mirrors BallotProofForTestDeterministic's field formula with
// the seed scheme (seedBase = 42+100*b, voter i in a batch uses seedBase+i).
func expectedChainTally(nBatches, batchSize, overwriteBatches int) [8]uint64 {
	lastFields := make(map[int][8]int64)
	for b := 0; b < nBatches; b++ {
		voterStart := b * batchSize
		if overwriteBatches > 0 && b >= nBatches-overwriteBatches {
			voterStart = (b - (nBatches - overwriteBatches)) * batchSize
		}
		seedBase := int64(42 + 100*b)
		for i := 0; i < batchSize; i++ {
			seed := seedBase + int64(i)
			var fields [8]int64
			stored := map[int64]bool{}
			for f := int64(0); f < 6; f++ {
				for attempt := int64(0); ; attempt++ {
					val := (seed + f*1000 + attempt) % 16
					if !stored[val] {
						fields[f] = val
						stored[val] = true
						break
					}
				}
			}
			lastFields[voterStart+i] = fields
		}
	}
	var totals [8]uint64
	for _, fields := range lastFields {
		for f := 0; f < 8; f++ {
			totals[f] += uint64(fields[f])
		}
	}
	return totals
}

// envInt reads an integer environment variable with a default.
func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
