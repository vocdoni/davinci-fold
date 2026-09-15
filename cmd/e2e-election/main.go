// cmd/run-election drives a full end-to-end election against a live
// davinci-fold deployment. It generates real Groth16 ballot proofs locally
// (via rapidsnark/CGO), submits them over the HTTP API, and hands the
// decryption key to trigger the GPU-bound finalize job.
package main

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"log"
	"math/big"
	"os"
	"time"

	jwtv4 "github.com/golang-jwt/jwt/v4"
	"github.com/vocdoni/davinci-node/circuits/ballotproof"
	bjjgnark "github.com/vocdoni/davinci-node/crypto/ecc/bjj_gnark"
	"github.com/vocdoni/davinci-node/crypto/elgamal"
	"github.com/vocdoni/davinci-fold/api"
	"github.com/vocdoni/davinci-fold/orchestrator"
	"github.com/vocdoni/davinci-fold/tests/helpers"
	davinci "github.com/vocdoni/davinci-zkvm/go-sdk"
	"github.com/vocdoni/davinci-zkvm/go-sdk/tests/integration"
)

const (
	defaultServer    = "http://localhost:8888"
	defaultWorker    = "http://10.200.0.26:8080"
	defaultJWTSecret = "5947d03df7aa42690efa2037a78781841767bc15f848b3060396b4baada02ad1"
	batchSize        = 2
	foldEvery        = 1
)

func main() {
	serverURL := envOr("DAVINCI_SERVER", defaultServer)
	workerURL := envOr("DAVINCI_WORKER", defaultWorker)
	jwtSecret := envOr("DAVINCI_JWT_SECRET", defaultJWTSecret)

	ctx := context.Background()
	client := helpers.NewClient(serverURL)

	// Mint admin + keywarden JWTs from the live server secret.
	adminTok, err := mintToken(jwtSecret, api.RoleAdmin, "run-election-admin")
	if err != nil {
		log.Fatalf("mint admin token: %v", err)
	}
	kwTok, err := mintToken(jwtSecret, api.RoleKeywarden, "run-election-kw")
	if err != nil {
		log.Fatalf("mint keywarden token: %v", err)
	}

	// Wait until the server is ready.
	log.Println("waiting for davinci-fold API...")
	if err := client.WaitReady(ctx, 15*time.Second); err != nil {
		log.Fatalf("server not ready: %v", err)
	}
	log.Printf("server at %s is ready", serverURL)

	// Register the GPU worker (idempotent — the API accepts re-registration).
	log.Printf("registering worker %s ...", workerURL)
	if err := client.RegisterWorker(ctx, adminTok, workerURL, "gpu-0"); err != nil {
		log.Printf("warning: register worker: %v (continuing — might already be registered)", err)
	} else {
		log.Println("worker registered")
	}

	// Build election keys, census, and voters.
	log.Println("building election (2 voters)...")
	election, err := integration.NewElection(batchSize)
	if err != nil {
		log.Fatalf("NewElection: %v", err)
	}

	censusRoot, ok := election.Census.Root()
	if !ok {
		log.Fatal("census has no root")
	}
	encX, encY := election.EncKey.Point()

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
		EndTime:      time.Now().Add(90 * time.Second),
	}

	el, err := client.CreateElection(ctx, adminTok, createReq)
	if err != nil {
		log.Fatalf("CreateElection: %v", err)
	}
	log.Printf("created election %s (status=%s)", el.ID, el.Status)

	// Generate Groth16 ballot proofs for all voters. This is the slow CPU step.
	log.Printf("generating ballot proofs for %d voters (this may take ~1-2 min)...", batchSize)
	t0 := time.Now()
	batch, err := integration.GenerateBallotBatch(election.ProcessID, election.EncKey, election.Voters, 42)
	if err != nil {
		log.Fatalf("GenerateBallotBatch: %v", err)
	}
	log.Printf("ballot proofs generated in %s", time.Since(t0).Round(time.Second))

	census, err := election.BuildCensusProofs(election.Voters)
	if err != nil {
		log.Fatalf("BuildCensusProofs: %v", err)
	}

	// Submit votes.
	for i, v := range election.Voters {
		res := batch.Results[i]
		sub := toVoteSubmission(v, res, census[i])
		receipt, err := client.SubmitVote(ctx, el.ID, sub)
		if err != nil {
			log.Fatalf("SubmitVote voter %d: %v", i, err)
		}
		log.Printf("vote %d submitted (receipt=%s)", i, receipt.VoteID)
	}

	// Poll until the election reaches "decrypting" (all batches proved + folded).
	log.Println("waiting for election to reach 'decrypting' state...")
	deadline := time.Now().Add(25 * time.Minute)
	for {
		got, err := client.GetElection(ctx, el.ID)
		if err != nil {
			log.Fatalf("GetElection: %v", err)
		}
		log.Printf("  status: %s", got.Status)
		if got.Status == "decrypting" || got.Status == "finalizing" || got.Status == "results" {
			break
		}
		if time.Now().After(deadline) {
			log.Fatalf("timed out after 25 min; last status: %s", got.Status)
		}
		time.Sleep(5 * time.Second)
	}

	// Keywarden: fetch encrypted results (just to confirm the API is happy).
	ct, code, err := client.EncryptedResults(ctx, kwTok, el.ID)
	if err != nil {
		log.Fatalf("EncryptedResults (status %d): %v", code, err)
	}
	log.Printf("encrypted results fetched (%d ciphertext entries)", len(ct.Ciphertext))

	// Submit the decryption key to trigger the GPU-bound finalize.
	log.Println("submitting decryption key (triggers GPU finalize)...")
	client.SetTimeout(25 * time.Minute)
	results, err := client.SubmitDecryptionKey(ctx, kwTok, el.ID, election.EncPrivKey)
	if err != nil {
		log.Fatalf("SubmitDecryptionKey: %v", err)
	}

	log.Printf("finalize complete!")
	log.Printf("tally: %v", results.Tally)
	log.Printf("proofBytes (len=%d)", len(results.ProofBytes))

	os.Exit(0)
}

// mintToken signs a JWT with the given HMAC-SHA256 secret, role, and subject.
func mintToken(secret, role, subject string) (string, error) {
	tok := jwtv4.NewWithClaims(jwtv4.SigningMethodHS256, jwtv4.MapClaims{
		"role": role,
		"sub":  subject,
		"exp":  time.Now().Add(2 * time.Hour).Unix(),
	})
	return tok.SignedString([]byte(secret))
}

// toVoteSubmission maps a generated ballot to the orchestrator.VoteSubmission
// format expected by the davinci-fold /elections/{id}/votes endpoint.
func toVoteSubmission(v *integration.Voter, res *integration.BallotResult, c davinci.CensusProof) *orchestrator.VoteSubmission {
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
		Census:       c,
	}
}

// feHex returns a *big.Int as a 0x-prefixed 32-byte big-endian hex string.
func feHex(v *big.Int) string {
	var buf [32]byte
	v.FillBytes(buf[:])
	return "0x" + hex.EncodeToString(buf[:])
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
