package helpers

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"

	davinci "github.com/vocdoni/davinci-zkvm/go-sdk"
	"github.com/vocdoni/davinci-zkvm/go-sdk/chain"
)

// Job kinds of a FakeProver.
const (
	JobBatch    = "batch"    // prove jobs and imported batch proofs
	JobFold     = "fold"     // fold jobs and imported fold proofs
	JobFinalize = "finalize" // finalize jobs
)

// FakeProof is what a FakeProver proof.bin attests: for a batch, the state
// transition of its prove request; for a fold, the chain up to it.
type FakeProof struct {
	Kind       string               `json:"kind"`              // JobBatch or JobFold
	OldRoot    string               `json:"oldRoot"`           // batch: root before it; fold: root at the chain's genesis
	NewRoot    string               `json:"newRoot"`           // root after it
	Voters     uint64               `json:"voters"`            // fold: since genesis
	Overwrites uint64               `json:"overwrites"`        // fold: since genesis
	Steps      uint32               `json:"steps,omitempty"`   // fold steps up to this one
	Prev       string               `json:"prev,omitempty"`    // hash of the previous fold's proof
	Batches    []string             `json:"batches,omitempty"` // hashes of the batch proofs folded in this step
	FoldVK     string               `json:"foldVK,omitempty"`  // fold vk the chain binds
	Config     *davinci.ChainConfig `json:"config,omitempty"`
	VK         string               `json:"vk,omitempty"`       // batch: program vk of the guest that proved it
	Rejected   bool                 `json:"rejected,omitempty"` // batch: every fold of it fails
}

const fakeProofMagic = "FAKESTARK"

func (p *FakeProof) encode() []byte {
	b, err := json.Marshal(p)
	if err != nil {
		panic(err)
	}
	return append([]byte(fakeProofMagic), b...)
}

func decodeFakeProof(raw []byte) (*FakeProof, error) {
	body, ok := bytes.CutPrefix(raw, []byte(fakeProofMagic))
	if !ok {
		return nil, fmt.Errorf("not a fake proof")
	}
	var p FakeProof
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func proofHash(raw []byte) string {
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}

// Ledger records every proof the fake provers of a test made, by hash, and
// the fold behind every finalize, so a test can walk a fold chain that moved
// between provers.
type Ledger struct {
	mu        sync.Mutex
	proofs    map[string]*FakeProof
	finalized []string // fold proof hash of each finalize, in order
}

// NewLedger returns an empty ledger.
func NewLedger() *Ledger { return &Ledger{proofs: make(map[string]*FakeProof)} }

func (l *Ledger) add(raw []byte, p *FakeProof) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.proofs[proofHash(raw)] = p
}

// FinalChain walks back from the fold of the last finalize to the genesis
// fold, checking that each fold is the next step after the one before it,
// and returns the batch proofs folded along the chain, in order. The batches
// must continue each other's state roots from the genesis root to the final
// one.
func (l *Ledger) FinalChain() ([]*FakeProof, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.finalized) == 0 {
		return nil, fmt.Errorf("no finalize")
	}
	var folds []*FakeProof
	for h := l.finalized[len(l.finalized)-1]; h != ""; {
		f, ok := l.proofs[h]
		if !ok || f.Kind != JobFold {
			return nil, fmt.Errorf("fold %s unknown", h)
		}
		folds = append([]*FakeProof{f}, folds...)
		h = f.Prev
	}
	var batches []*FakeProof
	root := folds[0].OldRoot
	for i, f := range folds {
		if f.Steps != uint32(i+1) {
			return nil, fmt.Errorf("fold %d has step %d", i, f.Steps)
		}
		for _, bh := range f.Batches {
			b, ok := l.proofs[bh]
			if !ok || b.Kind != JobBatch {
				return nil, fmt.Errorf("batch %s unknown", bh)
			}
			if b.OldRoot != root {
				return nil, fmt.Errorf("fold %d: batch from %s does not continue %s", i, b.OldRoot, root)
			}
			root = b.NewRoot
			batches = append(batches, b)
		}
		if f.NewRoot != root {
			return nil, fmt.Errorf("fold %d ends at %s, its batches at %s", i, f.NewRoot, root)
		}
	}
	return batches, nil
}

// fakeJob is a job in a FakeProver's job store.
type fakeJob struct {
	kind    string
	status  string // davinci.JobStatus*
	final   string // status once released, for held jobs
	err     string
	proof   []byte // proof.bin of batch and fold jobs
	publics []byte // finalize jobs
	snark   map[string]string
}

// FakeProver is an in-process davinci-zkvm prover in chained mode, for tests
// without a GPU. It serves what the orchestrator calls: health, prove, job
// status, stark info, raw proof, import (with kind), fold, finalize, publics
// and snark. Its proofs are deterministic FakeProof records, and it checks
// what the guests check about the chain: a fold extends a fold proof under
// the same config and fold vk, and its batches are of the pinned batch
// circuit and continue each other's state roots. A finalize commits the
// digest the orchestrator verifies, with the pinned circuit release's vks.
// Jobs are done when submitted unless held; the next jobs of a kind can be
// made to fail, the prover can run other guests (another vk) or make batch
// proofs every fold rejects, and it can disappear.
type FakeProver struct {
	URL    string
	srv    *httptest.Server
	ledger *Ledger

	mu       sync.Mutex
	next     int
	jobs     map[string]*fakeJob
	queueLen int
	hold     map[string]bool
	fail     map[string]int
	counts   map[string]int
	folds    []davinci.FoldRequest
	batchVK  string // program vk of its batch guest
	aggVK    string // program vk of its aggregator guest
	poison   int    // batch proofs still to make rejected
}

// NewFakeProver starts a fake prover recording its proofs in ledger. Close
// it with Kill.
func NewFakeProver(ledger *Ledger) *FakeProver {
	p := &FakeProver{
		ledger:  ledger,
		jobs:    make(map[string]*fakeJob),
		hold:    make(map[string]bool),
		fail:    make(map[string]int),
		counts:  make(map[string]int),
		batchVK: chain.CircuitRelease.BatchVK,
		aggVK:   chain.CircuitRelease.AggVK,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", p.health)
	mux.HandleFunc("POST /prove", p.prove)
	mux.HandleFunc("POST /jobs/import", p.importProof)
	mux.HandleFunc("POST /fold", p.fold)
	mux.HandleFunc("POST /finalize", p.finalize)
	mux.HandleFunc("GET /jobs/{id}", p.job)
	mux.HandleFunc("GET /jobs/{id}/stark", p.stark)
	mux.HandleFunc("GET /jobs/{id}/snark/raw", p.raw)
	mux.HandleFunc("GET /jobs/{id}/publics", p.publics)
	mux.HandleFunc("GET /jobs/{id}/snark", p.snark)
	p.srv = httptest.NewServer(mux)
	p.URL = p.srv.URL
	return p
}

// Kill stops the prover: from now on its address refuses connections.
func (p *FakeProver) Kill() {
	p.srv.CloseClientConnections()
	p.srv.Close()
}

// SetQueueLen sets the queue length the health endpoint reports.
func (p *FakeProver) SetQueueLen(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.queueLen = n
}

// Hold keeps the jobs of kind submitted from now on running until Release.
func (p *FakeProver) Hold(kind string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hold[kind] = true
}

// Release stops holding jobs of kind and lets the held ones finish.
func (p *FakeProver) Release(kind string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hold[kind] = false
	for _, j := range p.jobs {
		if j.kind == kind && j.status == davinci.JobStatusRunning {
			j.status = j.final
		}
	}
}

// SetVKs makes the prover run other guests: its batch proofs and fold jobs
// have these program vks, as a prover not yet upgraded to the pinned
// release.
func (p *FakeProver) SetVKs(batchVK, aggVK string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.batchVK, p.aggVK = batchVK, aggVK
}

// Poison makes the next n batch proofs of the prover ones every fold
// rejects, on any prover, although they are of the batch circuit.
func (p *FakeProver) Poison(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.poison = n
}

// FailNext makes the next n submitted jobs of kind fail.
func (p *FakeProver) FailNext(kind string, n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fail[kind] = n
}

// Running returns the number of held jobs of kind.
func (p *FakeProver) Running(kind string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, j := range p.jobs {
		if j.kind == kind && j.status == davinci.JobStatusRunning {
			n++
		}
	}
	return n
}

// Count returns how many requests of a type the prover took: "prove",
// "import:batch", "import:fold", "fold" or "finalize"; "import:foreign"
// counts the imported batch proofs of another circuit.
func (p *FakeProver) Count(what string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.counts[what]
}

// Folds returns the fold requests the prover took, in order.
func (p *FakeProver) Folds() []davinci.FoldRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]davinci.FoldRequest(nil), p.folds...)
}

// addJob registers a submitted job of kind, failed with failure if it is
// not empty, held or failing as the controls say. The caller holds p.mu.
func (p *FakeProver) addJob(j *fakeJob, failure string) string {
	p.next++
	id := fmt.Sprintf("%s-%d", j.kind, p.next)
	j.final = davinci.JobStatusDone
	if p.fail[j.kind] > 0 {
		p.fail[j.kind]--
		failure = "injected failure"
	}
	if failure != "" {
		j.final, j.err = davinci.JobStatusFailed, failure
	}
	j.status = j.final
	if p.hold[j.kind] {
		j.status = davinci.JobStatusRunning
	}
	p.jobs[id] = j
	return id
}

// doneJob returns a done job of one of kinds, or an error a real prover
// answers with 400.
func (p *FakeProver) doneJob(id string, kinds ...string) (*fakeJob, error) {
	j, ok := p.jobs[id]
	switch {
	case !ok:
		return nil, fmt.Errorf("job %s not found", id)
	case j.status != davinci.JobStatusDone:
		return nil, fmt.Errorf("job %s is not done (status: %s)", id, j.status)
	case !slices.Contains(kinds, j.kind):
		return nil, fmt.Errorf("job %s has kind %s, expected %v", id, j.kind, kinds)
	}
	return j, nil
}

func (p *FakeProver) health(w http.ResponseWriter, _ *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	writeJSON(w, http.StatusOK, davinci.HealthResponse{Status: "ok", Version: "fake", QueueLen: p.queueLen})
}

func (p *FakeProver) prove(w http.ResponseWriter, r *http.Request) {
	var req davinci.ProveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.State == nil || req.Output != "stark" {
		http.Error(w, "want a stark prove request with a state block", http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	proof := &FakeProof{
		Kind:       JobBatch,
		OldRoot:    req.State.OldStateRoot,
		NewRoot:    req.State.NewStateRoot,
		Voters:     req.State.VotersCount,
		Overwrites: req.State.OverwrittenCount,
		VK:         p.batchVK,
		Rejected:   p.poison > 0,
	}
	if p.poison > 0 {
		p.poison--
	}
	raw := proof.encode()
	p.ledger.add(raw, proof)
	p.counts["prove"]++
	id := p.addJob(&fakeJob{kind: JobBatch, proof: raw}, "")
	writeJSON(w, http.StatusAccepted, davinci.ProveResponse{JobID: id, Status: "queued"})
}

func (p *FakeProver) importProof(w http.ResponseWriter, r *http.Request) {
	kind := JobBatch
	if q := r.URL.Query(); q.Has("kind") {
		kind = q.Get("kind")
		if kind != JobBatch && kind != JobFold {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "kind must be batch or fold"})
			return
		}
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	proof, err := decodeFakeProof(raw)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	p.ledger.add(raw, proof)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.counts["import:"+kind]++
	if proof.Kind == JobBatch && proof.VK != chain.CircuitRelease.BatchVK {
		p.counts["import:foreign"]++
	}
	p.next++
	id := fmt.Sprintf("import-%d", p.next)
	p.jobs[id] = &fakeJob{kind: kind, status: davinci.JobStatusDone, proof: raw}
	writeJSON(w, http.StatusOK, davinci.ProveResponse{JobID: id})
}

func (p *FakeProver) fold(w http.ResponseWriter, r *http.Request) {
	var req davinci.FoldRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.BatchJobs) == 0 {
		http.Error(w, "want a fold request with batch jobs", http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	batches := make([]*FakeProof, len(req.BatchJobs))
	hashes := make([]string, len(req.BatchJobs))
	for i, id := range req.BatchJobs {
		j, err := p.doneJob(id, JobBatch)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		batches[i], _ = decodeFakeProof(j.proof)
		hashes[i] = proofHash(j.proof)
	}
	out := &FakeProof{Kind: JobFold, Config: &req.Config, Batches: hashes, Steps: 1, FoldVK: req.FoldVK}
	failure := ""
	if req.PrevFoldJob == "" {
		out.OldRoot = batches[0].OldRoot
		out.NewRoot = out.OldRoot
	} else {
		j, err := p.doneJob(req.PrevFoldJob, JobFold)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		prev, err := decodeFakeProof(j.proof)
		switch {
		case err != nil || prev.Kind != JobFold:
			failure = "previous proof is not a fold proof"
		case prev.Config == nil || *prev.Config != req.Config:
			failure = "previous fold is of another election"
		case req.FoldVK != "" && req.FoldVK != prev.FoldVK:
			failure = "fold vk differs from the previous fold's"
		default:
			out.OldRoot, out.NewRoot = prev.OldRoot, prev.NewRoot
			out.Voters, out.Overwrites = prev.Voters, prev.Overwrites
			out.Steps, out.Prev, out.FoldVK = prev.Steps+1, proofHash(j.proof), prev.FoldVK
		}
	}
	for _, b := range batches {
		if b == nil || b.Kind != JobBatch {
			failure = "not a batch proof"
			break
		}
		if b.VK != chain.CircuitRelease.BatchVK {
			failure = "batch proof of another circuit"
			break
		}
		if b.Rejected {
			failure = "batch proof rejected"
			break
		}
		if b.OldRoot != out.NewRoot {
			failure = "batch does not continue the chain"
			break
		}
		out.NewRoot = b.NewRoot
		out.Voters += b.Voters
		out.Overwrites += b.Overwrites
	}
	raw := out.encode()
	if failure == "" {
		p.ledger.add(raw, out)
	}
	p.counts["fold"]++
	p.folds = append(p.folds, req)
	id := p.addJob(&fakeJob{kind: JobFold, proof: raw}, failure)
	writeJSON(w, http.StatusAccepted, davinci.ProveResponse{JobID: id, Status: "queued"})
}

func (p *FakeProver) finalize(w http.ResponseWriter, r *http.Request) {
	var req davinci.FinalizeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	j, err := p.doneJob(req.FoldJob, JobFold)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	job := &fakeJob{kind: JobFinalize}
	failure := ""
	fold, err := decodeFakeProof(j.proof)
	switch {
	case err != nil || fold.Kind != JobFold:
		failure = "not a fold proof"
	case fold.Config == nil || *fold.Config != req.Config:
		failure = "fold is of another election"
	case req.FoldVK != "" && req.FoldVK != fold.FoldVK:
		failure = "fold vk differs from the fold's"
	default:
		job.publics, job.snark, err = finalizeOutput(fold, &req)
		if err != nil {
			failure = err.Error()
		}
	}
	if failure == "" {
		p.ledger.mu.Lock()
		p.ledger.finalized = append(p.ledger.finalized, proofHash(j.proof))
		p.ledger.mu.Unlock()
	}
	p.counts["finalize"]++
	id := p.addJob(job, failure)
	writeJSON(w, http.StatusAccepted, davinci.ProveResponse{JobID: id, Status: "queued"})
}

// finalizeOutput builds the publics (the aggregator digest, 64 LE u32) and
// the PLONK payload of a finalize of fold.
func finalizeOutput(fold *FakeProof, req *davinci.FinalizeRequest) ([]byte, map[string]string, error) {
	batchWords, err := chain.VKWords(chain.CircuitRelease.BatchVK)
	if err != nil {
		return nil, nil, err
	}
	foldWords, err := chain.VKWords(fold.FoldVK)
	if err != nil {
		return nil, nil, fmt.Errorf("fold vk: %w", err)
	}
	commitment, err := chain.CanonicalConfigCommitment(&req.Config, batchWords, foldWords)
	if err != nil {
		return nil, nil, err
	}
	root, err := hex.DecodeString(strings.TrimPrefix(fold.NewRoot, "0x"))
	if err != nil || len(root) != 32 {
		return nil, nil, fmt.Errorf("state root %q", fold.NewRoot)
	}
	words := make([]uint32, 64)
	words[0] = binary.LittleEndian.Uint32([]byte("DAG1"))
	words[1], words[2] = chain.ModeFinalize, fold.Steps
	words[3], words[4] = uint32(fold.Voters), uint32(fold.Overwrites)
	put := func(off int, b []byte) {
		for i := 0; i < len(b)/4; i++ {
			words[off+i] = binary.LittleEndian.Uint32(b[i*4:])
		}
	}
	put(5, commitment[:])
	put(13, root)
	putVK := func(off int, vk [4]uint64) {
		for i, v := range vk {
			words[off+2*i], words[off+2*i+1] = uint32(v), uint32(v>>32)
		}
	}
	putVK(21, batchWords)
	putVK(29, foldWords)
	for i := 0; i < davinci.NumFields && i < len(req.Results.Results); i++ {
		words[37+i] = uint32(req.Results.Results[i])
	}
	publics := make([]byte, 0, 4*len(words))
	values := make([]byte, 0, 8*len(words))
	for _, w := range words {
		publics = binary.LittleEndian.AppendUint32(publics, w)
		values = binary.LittleEndian.AppendUint64(values, uint64(w))
	}
	seed := sha256.Sum256(publics)
	proofBytes := make([]byte, 0, 24*32)
	for i := 0; len(proofBytes) < 24*32; i++ {
		h := sha256.Sum256(append(seed[:], byte(i)))
		proofBytes = append(proofBytes, h[:]...)
	}
	return publics, map[string]string{
		"program_vk":          fold.FoldVK,
		"root_c_vadcop_final": "0x" + hex.EncodeToString(seed[:]),
		"public_values":       "0x" + hex.EncodeToString(values),
		"proof_bytes":         "0x" + hex.EncodeToString(proofBytes[:24*32]),
	}, nil
}

func (p *FakeProver) job(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	j, ok := p.jobs[r.PathValue("id")]
	if !ok {
		http.Error(w, "job not found", http.StatusNotFound)
		return
	}
	resp := davinci.JobResponse{JobID: r.PathValue("id"), Status: j.status}
	if j.status == davinci.JobStatusFailed {
		resp.Error = &j.err
	}
	writeJSON(w, http.StatusOK, resp)
}

func (p *FakeProver) stark(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	j, err := p.doneJob(r.PathValue("id"), JobBatch, JobFold)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	vk := p.aggVK
	if j.kind == JobBatch {
		proof, _ := decodeFakeProof(j.proof)
		vk = proof.VK
	}
	writeJSON(w, http.StatusOK, davinci.StarkInfo{ProgramVK: vk, ZiskVK: "0x" + strings.Repeat("00", 32)})
}

func (p *FakeProver) raw(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	j, err := p.doneJob(r.PathValue("id"), JobBatch, JobFold)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	_, _ = w.Write(j.proof)
}

func (p *FakeProver) publics(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	j, err := p.doneJob(r.PathValue("id"), JobFinalize)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	_, _ = w.Write(j.publics)
}

func (p *FakeProver) snark(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	j, err := p.doneJob(r.PathValue("id"), JobFinalize)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, j.snark)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
