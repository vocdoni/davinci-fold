package tests

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"text/tabwriter"
	"time"

	qt "github.com/frankban/quicktest"

	"github.com/vocdoni/davinci-fold/api"
	"github.com/vocdoni/davinci-fold/orchestrator"
	"github.com/vocdoni/davinci-fold/storage"
	"github.com/vocdoni/davinci-fold/tests/helpers"
	"github.com/vocdoni/davinci-fold/types"
	"github.com/vocdoni/davinci-fold/workers"
	davinci "github.com/vocdoni/davinci-zkvm/go-sdk"
	"github.com/vocdoni/davinci-zkvm/go-sdk/chain"
	davinciSolidity "github.com/vocdoni/davinci-zkvm/go-sdk/solidity"
	"github.com/vocdoni/davinci-zkvm/go-sdk/tests/integration"
	"github.com/vocdoni/davinci-zkvm/go-sdk/vocdoni/circuits/ballotproof"
	bjjgnark "github.com/vocdoni/davinci-zkvm/go-sdk/vocdoni/crypto/ecc/bjj_gnark"
	"github.com/vocdoni/davinci-zkvm/go-sdk/vocdoni/crypto/elgamal"
	spectestutil "github.com/vocdoni/davinci-zkvm/go-sdk/vocdoni/spec/testutil"
)

// e2eTimeout bounds each wait of the end-to-end tests on the provers.
const e2eTimeout = 30 * time.Minute

// requireWorkers skips the calling test unless the integration suite is
// enabled with at least n prover workers in DAVINCI_FOLD_WORKER_URLS: the
// end-to-end tests need real GPU provers.
func requireWorkers(t *testing.T, n int) {
	t.Helper()
	requireIntegration(t)
	if len(workerURLs) < n {
		t.Skipf("needs >=%d prover workers in DAVINCI_FOLD_WORKER_URLS (have %d)", n, len(workerURLs))
	}
}

// TestScatterGatherE2E drives a full chained-mode election through davinci-fold
// against real GPU provers: it generates real Groth16 ballots (with
// overwrites), submits them over the self-authenticating vote API, lets the
// lifecycle monitor end the election and drain the scattered batch STARKs onto
// the fold chain, hands the keywarden's decryption key in to trigger finalize,
// and finally asserts the analytic net tally, every vote settled, and the final
// PLONK verified on a simulated EVM. It is sized with the E2E_* variables (see
// docs/testing.md).
func TestScatterGatherE2E(t *testing.T) {
	requireWorkers(t, 1)
	e := newE2EElection(t, "e2e", envInt("E2E_BATCHES", 3), envInt("E2E_BATCH_SIZE", 2),
		envInt("E2E_OVERWRITE_BATCHES", 1), envInt("E2E_FOLD_EVERY", 1))
	registerWorkers(t)

	// Generous window: it stays active through submission, then ends, so the
	// monitor drains and publishes. Submission is fast (proving is async).
	e.create(t, time.Now().Add(45*time.Second))
	for b := range e.batches {
		e.submit(t, b)
	}
	finish(t, e)
	e.checkResults(t)
}

// TestFailoverE2E runs three elections at once on two or more provers and
// breaks the orchestrator twice while they run: it is killed and started
// again while batches are proved, and an election's fold worker is removed
// from the pool, then registered again a minute later. Each election must
// reach its exact tally with every vote settled and a final PLONK that
// verifies on a simulated EVM, behind a fold chain that folds every batch in
// order across the provers. Run against an external orchestrator (see
// scripts/e2e-live.sh), the crash is a SIGKILL of its container and its logs
// must hold no decryption key nor JWT.
func TestFailoverE2E(t *testing.T) {
	requireWorkers(t, 2)
	c := qt.New(t)
	ctx := t.Context()

	// The ballots come first; the elections then only wait for HTTP calls.
	ea := newE2EElection(t, "A", 4, 2, 1, 1) // its fold worker is removed
	eb := newE2EElection(t, "B", 3, 3, 1, 2) // ends at its end time
	ec := newE2EElection(t, "C", 3, 4, 1, 2)
	els := []*e2eElection{ea, eb, ec}
	registerWorkers(t)

	start := time.Now()
	ea.create(t, time.Time{})
	eb.create(t, time.Now().Add(2*time.Minute))
	ec.create(t, time.Time{})

	// A's first two batches are sealed together while every prover is idle,
	// so they are proved on two provers. The orchestrator picks up the
	// second one on its next job poll, before B and C take the provers.
	ea.submit(t, 0)
	ea.submit(t, 1)
	time.Sleep(6 * time.Second)
	eb.submit(t, 0)
	ec.submit(t, 0)

	// Crash while those batches are proved, then go on voting.
	time.Sleep(2 * time.Second)
	crashedAt := time.Since(start)
	crashOrchestrator(t)
	t.Logf("orchestrator crashed and started again at %s", crashedAt.Round(time.Second))
	eb.submit(t, 1)
	eb.submit(t, 2)
	ec.submit(t, 1)
	c.Assert(time.Now().Before(eb.endTime), qt.IsTrue, qt.Commentf("B ended before all its votes were in"))

	// Remove A's fold worker once A has folded: its chain moves to another
	// prover and goes on there. The prover comes back a minute later.
	waitUntil(t, "A's first fold", func() bool { return ea.voteStatus(t, 0, 0) == types.VoteStatusFolded.String() })
	removed := ea.get(t).FoldWorker
	removedAt := time.Now()
	code, err := services.Client.RemoveWorker(ctx, helpers.AdminToken(), workerID(t, removed))
	c.Assert(err, qt.IsNil, qt.Commentf("status %d", code))
	t.Logf("fold worker %s of A removed at %s", removed, time.Since(start).Round(time.Second))
	var movedTo string
	waitUntil(t, "A's fold chain moved", func() bool {
		movedTo = ea.get(t).FoldWorker
		return movedTo != removed
	})
	ea.submit(t, 2)
	time.Sleep(time.Until(removedAt.Add(time.Minute)))
	registerWorker(t, removed)
	t.Logf("%s registered again at %s", removed, time.Since(start).Round(time.Second))
	ea.submit(t, 3)
	ec.submit(t, 2)

	// B ends at its end time, A and C through the status endpoint.
	ea.end(t)
	ec.end(t)
	finish(t, els...)

	digests := make([]*chain.Digest, len(els))
	for i, e := range els {
		digests[i] = e.checkResults(t)
	}
	if logs := orchestratorLogs(t); logs != nil {
		checkNoSecrets(t, logs, els)
		t.Logf("prove jobs reused after the crash: %d", bytes.Count(logs, []byte("reused the batch's prove job")))
	} else {
		t.Log("in-process orchestrator: its log is the test output, not checked for secrets")
	}

	store := orchestratorStore(t)
	chains := make([]*e2eChain, len(els))
	for i, e := range els {
		chains[i] = e.records(t, store)
	}
	logSummary(t, els, chains, digests)
	for i, e := range els {
		e.checkChain(t, store, chains[i], digests[i])
	}

	// The elections' batches were scattered: one of them was proved on every
	// prover.
	c.Assert(slices.ContainsFunc(chains, func(ch *e2eChain) bool { return len(ch.provers) == len(workerURLs) }),
		qt.IsTrue, qt.Commentf("no election proved on all %d provers", len(workerURLs)))
	// A's chain moved when its fold worker was removed, and went on folding
	// where it moved to.
	ca := chains[0]
	c.Assert(ca.moves > 0, qt.IsTrue)
	c.Assert(ca.batches[0].FoldWorker, qt.Equals, removed)
	c.Assert(ca.batches[2].FoldWorker, qt.Equals, movedTo)
}

// e2eElection is an election of the end-to-end tests with real ballots,
// made before it starts: batches of distinct voters, the last ones voting
// again for the first voters, and the results they must give.
type e2eElection struct {
	name       string
	election   *integration.Election
	id         string // hex, without 0x
	batchSize  int
	foldEvery  int
	batches    [][]*orchestrator.VoteSubmission // in submission order
	tally      []uint64                         // the last ballot of each voter, summed
	overwrites int                              // ballots that replace an earlier one

	endTime time.Time // zero: ended through the status endpoint
	created time.Time
	ended   time.Time
	done    time.Time
	keySent bool
	results *api.ResultsResponse
}

// newE2EElection makes the ballots of an election of nBatches batches of
// batchSize votes, whose last overwriteBatches batches are cast again by the
// first voters. Its process ID gets a random nonce, so elections never
// collide.
func newE2EElection(t *testing.T, name string, nBatches, batchSize, overwriteBatches, foldEvery int) *e2eElection {
	t.Helper()
	c := qt.New(t)
	c.Assert(overwriteBatches < nBatches, qt.IsTrue, qt.Commentf("overwrite batches must be fewer than total batches"))
	fresh := (nBatches - overwriteBatches) * batchSize
	election, err := integration.NewElection(fresh)
	c.Assert(err, qt.IsNil)
	_, err = rand.Read(election.ProcessID[24:])
	c.Assert(err, qt.IsNil)
	census, err := election.BuildCensusProofs(election.Voters)
	c.Assert(err, qt.IsNil)

	e := &e2eElection{
		name:      name,
		election:  election,
		id:        hex.EncodeToString(election.ProcessID[:]),
		batchSize: batchSize,
		foldEvery: foldEvery,
	}
	lastSeed := make(map[int]int64) // voter -> seed of its last ballot
	for b := range nBatches {
		first := b * batchSize
		if b >= nBatches-overwriteBatches {
			first = (first - fresh) % fresh
			e.overwrites += batchSize
		}
		voters := election.Voters[first : first+batchSize]
		seedBase := int64(42 + 100*b)
		gen, err := integration.GenerateBallotBatch(election.ProcessID, election.EncKey, voters, seedBase)
		c.Assert(err, qt.IsNil, qt.Commentf("ballots of election %s, batch %d", name, b))
		subs := make([]*orchestrator.VoteSubmission, len(voters))
		for i, v := range voters {
			subs[i] = voteSubmission(v, gen.Results[i], census[first+i])
			lastSeed[first+i] = seedBase + int64(i)
		}
		e.batches = append(e.batches, subs)
	}
	e.tally = make([]uint64, davinci.NumFields)
	for _, seed := range lastSeed {
		for f, v := range ballotFields(seed) {
			e.tally[f] += uint64(v)
		}
	}
	return e
}

// votes is the number of ballots of the election.
func (e *e2eElection) votes() int { return len(e.batches) * e.batchSize }

// create creates the election, which ends at endTime unless it is zero.
func (e *e2eElection) create(t *testing.T, endTime time.Time) {
	t.Helper()
	req := electionRequest(t, e.election, e.batchSize, e.foldEvery, endTime)
	_, err := services.Client.CreateElection(t.Context(), helpers.AdminToken(), req)
	qt.Assert(t, err, qt.IsNil)
	e.created, e.endTime = time.Now(), endTime
	t.Logf("election %s: %s, %d batches of %d votes, fold every %d", e.name, e.id, len(e.batches), e.batchSize, e.foldEvery)
}

// submit submits the votes of batch b, which seal it.
func (e *e2eElection) submit(t *testing.T, b int) {
	t.Helper()
	for i, sub := range e.batches[b] {
		_, err := services.Client.SubmitVote(t.Context(), e.id, sub)
		qt.Assert(t, err, qt.IsNil, qt.Commentf("election %s, batch %d, vote %d", e.name, b, i))
	}
}

// get reads the election.
func (e *e2eElection) get(t *testing.T) *api.ElectionResponse {
	t.Helper()
	el, err := services.Client.GetElection(t.Context(), e.id)
	qt.Assert(t, err, qt.IsNil)
	return el
}

// voteStatus returns the status of vote i of batch b.
func (e *e2eElection) voteStatus(t *testing.T, b, i int) string {
	t.Helper()
	st, err := services.Client.Vote(t.Context(), e.id, hex.EncodeToString(e.batches[b][i].VoteID))
	qt.Assert(t, err, qt.IsNil)
	return st.Status
}

// end ends the election through the status endpoint.
func (e *e2eElection) end(t *testing.T) {
	t.Helper()
	_, code, err := services.Client.SetElectionStatus(t.Context(), helpers.AdminToken(), e.id, types.StatusEnded.String())
	qt.Assert(t, err, qt.IsNil, qt.Commentf("status %d", code))
	e.ended = time.Now()
}

// finish hands the decryption key of each election in once it is
// decrypting, as the keywarden does, and waits for all their results. A
// failed finalize fails the test.
func finish(t *testing.T, els ...*e2eElection) {
	t.Helper()
	c := qt.New(t)
	ctx := t.Context()
	keywarden := helpers.KeywardenToken()
	deadline := time.Now().Add(e2eTimeout)
	for {
		waiting := 0
		for _, e := range els {
			if e.results != nil {
				continue
			}
			waiting++
			el := e.get(t)
			if e.ended.IsZero() && el.Status != types.StatusActive.String() && el.Status != types.StatusPaused.String() {
				e.ended = e.endTime
			}
			switch el.Status {
			case types.StatusDecrypting.String():
				c.Assert(e.keySent, qt.IsFalse, qt.Commentf("election %s: finalize failed: %s", e.name, el.FinalizeError))
				ct, code, err := services.Client.EncryptedResults(ctx, keywarden, e.id)
				c.Assert(err, qt.IsNil, qt.Commentf("encrypted-results status %d", code))
				c.Assert(len(ct.Ciphertext), qt.Equals, davinci.NumFields*4)
				accepted, code, err := services.Client.SubmitDecryptionKey(ctx, keywarden, e.id, e.election.EncPrivKey)
				c.Assert(err, qt.IsNil)
				c.Assert(code, qt.Equals, http.StatusAccepted)
				c.Assert(accepted.ID, qt.Equals, e.id)
				e.keySent = true
				t.Logf("election %s decrypting, key submitted", e.name)
			case types.StatusResults.String():
				res, err := services.Client.Results(ctx, e.id)
				c.Assert(err, qt.IsNil)
				e.results, e.done = res, time.Now()
				t.Logf("election %s has results: tally %v", e.name, res.Tally)
			}
		}
		if waiting == 0 {
			return
		}
		c.Assert(time.Now().Before(deadline), qt.IsTrue, qt.Commentf("timed out waiting for the results"))
		time.Sleep(2 * time.Second)
	}
}

// checkResults checks the election's results: the tally its ballots give,
// every vote settled, and the final PLONK verified on a simulated EVM, its
// digest counting every ballot. It returns the digest.
func (e *e2eElection) checkResults(t *testing.T) *chain.Digest {
	t.Helper()
	c := qt.New(t)
	c.Assert(e.results.Tally, qt.DeepEquals, e.tally, qt.Commentf("election %s tally", e.name))
	for b := range e.batches {
		for i := range e.batches[b] {
			c.Assert(e.voteStatus(t, b, i), qt.Equals, types.VoteStatusSettled.String(),
				qt.Commentf("election %s, batch %d, vote %d", e.name, b, i))
		}
	}
	snark, err := plonkFromResults(e.results)
	c.Assert(err, qt.IsNil)
	c.Assert(davinciSolidity.VerifyOnSimulated(solidityDir(), snark), qt.IsNil, qt.Commentf("election %s", e.name))
	d, err := chain.ParseDigest(publicsU32(snark.PublicValues))
	c.Assert(err, qt.IsNil)
	c.Assert(d.Mode, qt.Equals, uint32(chain.ModeFinalize))
	c.Assert(d.TotalVoters, qt.Equals, uint32(e.votes()))
	c.Assert(d.TotalOverwrites, qt.Equals, uint32(e.overwrites))
	t.Logf("election %s: tally exact, %d votes settled, final PLONK verified on a simulated EVM", e.name, e.votes())
	return d
}

// e2eChain is an election's fold chain as the orchestrator recorded it.
type e2eChain struct {
	batches     []*types.BatchInput
	checkpoint  *types.FoldCheckpoint
	provers     []string // the provers that proved its batches
	foldWorkers []string // the provers its batches were folded on, in seq order
	moves       int      // fold-chain moves
}

// records reads the election's fold chain from the orchestrator's records.
func (e *e2eElection) records(t *testing.T, store *storage.Storage) *e2eChain {
	t.Helper()
	id := types.ElectionID(e.election.ProcessID[:])
	batches, err := store.ListBatchInputs(id)
	qt.Assert(t, err, qt.IsNil)
	cp, err := store.FoldCheckpoint(id)
	qt.Assert(t, err, qt.IsNil, qt.Commentf("election %s fold checkpoint", e.name))
	ch := &e2eChain{batches: batches, checkpoint: cp}
	for _, bi := range batches {
		if !slices.Contains(ch.provers, bi.Worker) {
			ch.provers = append(ch.provers, bi.Worker)
		}
		if n := len(ch.foldWorkers); n == 0 || ch.foldWorkers[n-1] != bi.FoldWorker {
			ch.foldWorkers = append(ch.foldWorkers, bi.FoldWorker)
		}
	}
	slices.Sort(ch.provers)
	audit, err := store.ListAudit()
	qt.Assert(t, err, qt.IsNil)
	for _, r := range audit {
		if bytes.Equal(r.ElectionID, id) && strings.HasPrefix(r.Action, "move_fold_chain:") {
			ch.moves++
		}
	}
	return ch
}

// checkChain checks the election's fold chain: every batch it sealed was
// proved, imported onto the fold worker of its time and folded, in seq
// order, up to the state root and fold count the final proof attests.
func (e *e2eElection) checkChain(t *testing.T, store *storage.Storage, ch *e2eChain, d *chain.Digest) {
	t.Helper()
	c := qt.New(t)
	c.Assert(len(ch.batches), qt.Equals, len(e.batches), qt.Commentf("election %s batches", e.name))
	for i, bi := range ch.batches {
		c.Assert(bi.Seq, qt.Equals, uint64(i))
		c.Assert(bi.Worker != "" && bi.JobID != "", qt.IsTrue, qt.Commentf("election %s batch %d not proved", e.name, i))
		c.Assert(bi.ImportedID != "" && bi.FoldWorker != "", qt.IsTrue, qt.Commentf("election %s batch %d not imported", e.name, i))
		c.Assert(len(bi.VoteIDs), qt.Equals, e.batchSize)
	}
	last := ch.batches[len(ch.batches)-1].NewStateRoot
	c.Assert(ch.checkpoint.BatchesFolded, qt.Equals, uint64(len(ch.batches)))
	c.Assert(ch.checkpoint.StateRoot, qt.Equals, last)
	c.Assert(ch.checkpoint.FoldCount, qt.Equals, uint64(d.StepCount))
	c.Assert(d.StateRootHex(), qt.Equals, last, qt.Commentf("election %s: the final proof's state root", e.name))
	_, err := store.FoldProof(types.ElectionID(e.election.ProcessID[:]))
	c.Assert(err, qt.ErrorIs, storage.ErrNotFound, qt.Commentf("election %s: stored proofs not dropped", e.name))
}

// logSummary logs a table of the elections: batches, folds, the provers
// that proved and folded them, fold-chain moves, and the time from creation
// and from the end to the results.
func logSummary(t *testing.T, els []*e2eElection, chains []*e2eChain, digests []*chain.Digest) {
	t.Helper()
	var sb strings.Builder
	w := tabwriter.NewWriter(&sb, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "election\tbatches\tfold every\tfolds\tprovers\tfolded on\tmoves\tresults after creation\tresults after end")
	for i, e := range els {
		_, _ = fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%s\t%s\t%d\t%s\t%s\n", e.name, len(chains[i].batches), e.foldEvery,
			digests[i].StepCount, hosts(chains[i].provers, ","), hosts(chains[i].foldWorkers, " > "), chains[i].moves,
			e.done.Sub(e.created).Round(time.Second), e.done.Sub(e.ended).Round(time.Second))
	}
	_ = w.Flush()
	t.Log("summary\n" + sb.String())
}

// hosts joins the host:port of each URL.
func hosts(urls []string, sep string) string {
	out := make([]string, len(urls))
	for i, u := range urls {
		out[i] = u
		if p, err := url.Parse(u); err == nil && p.Host != "" {
			out[i] = p.Host
		}
	}
	return strings.Join(out, sep)
}

// checkNoSecrets checks the orchestrator logged none of the elections'
// decryption keys, in hex or decimal, no JWT and not the JWT secret.
func checkNoSecrets(t *testing.T, logs []byte, els []*e2eElection) {
	t.Helper()
	c := qt.New(t)
	lower := bytes.ToLower(logs)
	for _, e := range els {
		key := e.election.EncPrivKey
		c.Assert(bytes.Contains(lower, []byte(key.Text(16))), qt.IsFalse, qt.Commentf("election %s key in hex", e.name))
		c.Assert(bytes.Contains(logs, []byte(key.String())), qt.IsFalse, qt.Commentf("election %s key in decimal", e.name))
	}
	// Every token the tests mint starts with this header.
	header, _, _ := strings.Cut(helpers.AdminToken(), ".")
	c.Assert(bytes.Contains(logs, []byte(header)), qt.IsFalse, qt.Commentf("a JWT in the logs"))
	c.Assert(bytes.Contains(logs, []byte(helpers.TestJWTSecret)), qt.IsFalse, qt.Commentf("the JWT secret in the logs"))
	t.Logf("orchestrator logs (%d bytes): no decryption key, JWT or JWT secret", len(logs))
}

// registerWorkers registers every prover in DAVINCI_FOLD_WORKER_URLS and
// waits until the pool finds them healthy.
func registerWorkers(t *testing.T) {
	t.Helper()
	for _, u := range workerURLs {
		registerWorker(t, u)
	}
}

// registerWorker registers the prover at address and waits until the pool
// finds it healthy.
func registerWorker(t *testing.T, address string) {
	t.Helper()
	name := "gpu-" + strconv.Itoa(slices.Index(workerURLs, address))
	qt.Assert(t, services.Client.RegisterWorker(t.Context(), helpers.AdminToken(), address, name), qt.IsNil)
	waitUntil(t, address+" healthy", func() bool {
		w := worker(t, address)
		return w != nil && w.Healthy
	})
}

// worker returns the pool's view of the prover at address, nil if it is not
// in the pool.
func worker(t *testing.T, address string) *workers.WorkerInfo {
	t.Helper()
	list, err := services.Client.ListWorkers(t.Context())
	qt.Assert(t, err, qt.IsNil)
	i := slices.IndexFunc(list.Workers, func(w *workers.WorkerInfo) bool { return w.Address == address })
	if i < 0 {
		return nil
	}
	return list.Workers[i]
}

// workerID returns the ID of the prover at address in the pool.
func workerID(t *testing.T, address string) string {
	t.Helper()
	w := worker(t, address)
	qt.Assert(t, w, qt.IsNotNil, qt.Commentf("worker %s", address))
	return w.ID
}

// waitUntil polls cond every two seconds until it holds, failing the test
// after e2eTimeout.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(e2eTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Second)
	}
}

// electionRequest is the create-election body of a generated election,
// whose genesis matches the one the ballots were made for.
func electionRequest(t *testing.T, e *integration.Election, batchSize, foldEvery int, endTime time.Time) *api.ElectionCreateRequest {
	t.Helper()
	root, ok := e.Census.Root()
	qt.Assert(t, ok, qt.IsTrue)
	bm, err := spectestutil.FixedBallotMode().Pack()
	qt.Assert(t, err, qt.IsNil)
	encX, encY := e.EncKey.Point()
	return &api.ElectionCreateRequest{
		ProcessID:    "0x" + hex.EncodeToString(e.ProcessID[:]),
		BallotMode:   "0x" + bm.Text(16),
		EncX:         "0x" + encX.Text(16),
		EncY:         "0x" + encY.Text(16),
		CensusOrigin: uint64(e.CensusOrigin),
		CensusRoot:   feHex(root),
		VK:           json.RawMessage(ballotproof.CircomVerificationKey),
		BatchSize:    batchSize,
		FoldEvery:    foldEvery,
		EndTime:      endTime,
	}
}

// voteSubmission maps a generated ballot to the self-authenticating vote body
// davinci-fold's ingest API expects. The ballot is rebuilt from its raw RTE
// ciphertexts and serialized the same way the seal path deserializes it.
func voteSubmission(v *integration.Voter, res *integration.BallotResult, census davinci.CensusProof) *orchestrator.VoteSubmission {
	ballot := elgamal.NewBallot(bjjgnark.New())
	for i := 0; i < davinci.NumFields; i++ {
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

// publicsU32 is the u32 view of the publics the on-chain verifier hashes,
// where each one is an 8-byte little-endian word: the view a fold or
// finalize job serves at /publics.
func publicsU32(values []byte) []byte {
	out := make([]byte, 0, len(values)/2)
	for i := 0; i+8 <= len(values); i += 8 {
		out = append(out, values[i:i+4]...)
	}
	return out
}

// solidityDir resolves the davinci-zkvm solidity verifier sources (sibling repo
// of davinci-fold) for VerifyOnSimulated.
func solidityDir() string {
	_, thisFile, _, _ := runtime.Caller(0)
	// <parent>/davinci-fold/tests/e2e_test.go -> <parent>/davinci-zkvm/solidity
	return filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", "davinci-zkvm", "solidity"))
}

// ballotFields is the plaintext of the deterministic ballot made with seed,
// as BallotProofForTestDeterministic draws it.
func ballotFields(seed int64) [davinci.NumFields]int64 {
	var fields [davinci.NumFields]int64
	stored := map[int64]bool{}
	for f := int64(0); f < spectestutil.BallotNumFields; f++ {
		for attempt := int64(0); ; attempt++ {
			val := (seed + f*1000 + attempt) % 16
			if !stored[val] {
				fields[f] = val
				stored[val] = true
				break
			}
		}
	}
	return fields
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
