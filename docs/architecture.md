# Architecture

davinci-fold is one Go process: an HTTP API, an orchestration engine that owns the state of
every election, a pool of remote davinci-zkvm provers and a PebbleDB store. Proof generation
happens on the provers; davinci-fold decides what to prove, where, and in which order, and
checks the final result.

## Proving pipeline

1. **Ingest.** Ingest applies every check the batch circuit makes on what a voter sends,
   because one ballot the circuit rejects fails its whole batch and blocks the election's fold
   chain. A ballot is accepted only if its Groth16 ballot proof verifies against the
   election's verification key; its public inputs are the submitted address, the vote ID and
   the inputs hash recomputed from the election, the ballot and the census weight; the
   voter's ECDSA signature over the vote ID recovers the address; its lean-IMT census proof
   leads from a leaf that binds the address to the election's census root; and its ballot is
   well formed (identity in the fields the ballot mode does not use, curve points in the
   others). The proof, public inputs and signature are stored re-encoded as the provers parse
   them. A vote ID is accepted once. The ballot slot is derived from the voter's address, so
   a later ballot from the same voter overwrites the earlier one.
2. **Seal.** Accepted votes wait in a per-election buffer. A batch is sealed when the buffer
   holds a full batch of distinct voters, when its oldest vote is older than `--batch.time`, or
   when the election ends. The batch circuit rejects a batch that writes one slot twice, so a
   voter's second vote waits for a later batch and the latest vote still wins. Sealing applies
   the batch to the election state (ballot tree, re-encryption, encrypted tally) and persists
   the exact prove request and the new state snapshot. If the state refuses a vote (its vote-ID
   key is already in the tree, for example), the state is restored from the last snapshot, the
   vote is dropped with status `error` and the rest of the batch is sealed.
3. **Prove.** Each sealed batch is sent as a STARK job to the healthy prover with the shortest
   queue. A failed job is resubmitted, up to three attempts, each to the least-loaded prover at
   that moment. Batches of one election are dispatched in sequence order, one at a time;
   different elections run in parallel.
4. **Gather.** The batch proof is downloaded from the prover that made it and imported into
   the election's fold worker with davinci-zkvm's `POST /jobs/import`. A fold can only reference
   proofs in its own worker's job store, which is why the gather step exists.
5. **Fold.** After every `foldEvery` imported batches the fold worker folds them into the
   chain. The first fold runs a bootstrap pass to learn the aggregator's program key, which the
   guest cannot know about itself, and the genesis fold binds it; later folds extend the
   previous one. A checkpoint (fold count, batches folded, last fold job, program keys) is
   persisted after each fold, and the votes of the folded batches become `folded`.
6. **Finalize.** When the election ends, the remaining batches are dispatched and folded and
   the encrypted tally is published. Once the keywarden returns the private key, davinci-fold
   checks it against the election's public key and, in the background, decrypts the tally,
   builds a Chaum-Pedersen proof for every decrypted field and sends both to the fold worker,
   which verifies them in-circuit and produces the final PLONK proof. When it is verified and
   stored, every vote becomes `settled`.

## Election lifecycle

A monitor sweeps all open elections once per second, sealing batches that aged past the
batch window, ending elections past their end time and driving ended elections forward. The
organizer (an `admin` token) can also pause, resume, end or cancel an election through
`POST /elections/{id}/status`; ending it goes through the same code as the end time.

```
active ◀── pause / resume ──▶ paused

active or paused ──end time or end──▶ ended ──drain──▶ decrypting ──key──▶ finalizing ──PLONK──▶ results
active or paused ──cancel──▶ canceled                      ▲                   │
                                                           └──── on error ─────┘
```

| Status | Meaning |
|---|---|
| `active` | Accepting votes; batches are sealed, proved and folded. |
| `paused` | Not accepting votes; the accepted ones are still sealed, proved and folded. Its end time still ends it. |
| `ended` | Past `endTime`, or ended by the organizer. No new votes; the last batch is sealed and the chain drained. |
| `decrypting` | The encrypted tally is published and davinci-fold waits for the key. |
| `finalizing` | A key was accepted; the final fold and PLONK are running. |
| `results` | Tally and PLONK proof are available. Final. |
| `canceled` | Canceled by the organizer. Nothing is sealed, proved or folded any more; the votes are `error`. Final. |

If finalize fails, the election returns to `decrypting` with a short reason in
`finalizeError` (the details go to the log and the audit trail), and the keywarden can submit
the key again. A finalize cut short by a restart ends the same
way. The drain and the finalize each run at most once at a time per election. `created` is the
zero value of the status type; no election is in it.

## Provers

Provers are davinci-zkvm services registered through `POST /workers/register`. The pool is
held in memory and starts empty on every start.

- **Health.** Each prover is polled on `GET /health` every `--worker.pollPeriod`. A prover
  that does not answer is skipped when picking where to send work.
- **Bans.** A failed job counts against the prover that ran it. After more than three
  consecutive failures it is banned for 30 minutes; a success resets the count. A job that
  runs past 30 minutes counts as failed.
- **Removal.** `DELETE /workers/{id}` takes a prover out of the pool. Its running jobs count as
  failed and are sent elsewhere, as when a prover dies.
- **Fold worker.** Each election pins one fold worker, chosen as the least-loaded prover when
  its first batch is gathered. The fold chain and the imported batch proofs live in that
  worker's job store, so it must stay up, with its store intact, until the election has
  results.
- **Circuit release.** davinci-fold checks the final proof against the circuit release pinned
  by the davinci-zkvm Go SDK it is built with (`chain.CircuitRelease`). Provers must run that
  release, with the aggregator guest enabled; proofs from any other build are rejected at
  finalize.

## Persistence and restarts

Everything lives in one PebbleDB database under `--datadir`: election records and status,
the vote log with per-vote status, the prove request of every sealed batch together with the
prover and job that proved it, fold checkpoints, state snapshots, final results and an audit
log of admin, keywarden and system actions.

The state is restored from snapshots rather than rebuilt from the vote log because each batch
draws fresh re-encryption randomness, so replaying the votes would not reproduce the same
state. On start davinci-fold loads every election that is still open, restores its state
from the latest snapshot and its fold checkpoint, and resumes dispatching sealed batches that
were not yet imported. Dispatch skips batches already imported, so repeating it is harmless.
A batch being dispatched is reserved; a reservation older than three job timeouts plus five
minutes is freed. Worker registrations are not stored: register the provers again after a
restart.

A restart does not yet recover votes accepted but not sealed into a batch, nor batches
imported on the fold worker but not yet folded.

## Keys and the keywarden

davinci-fold holds only the election's ElGamal public key while voting is open. When the
election reaches `decrypting`, any client with a `keywarden` token can fetch the encrypted
tally and post the private key back. davinci-fold uses the key to decrypt and prove the tally
and does not store it. The API never logs the request or response of that call, nor any
`Authorization` header.

`test-keywarden` is a minimal keywarden that keeps the key pair in a local file, submits the
key and waits for the results. It is meant for testing; any service that holds the key and a
`keywarden` token can take its place.

## Checks at finalize

The provers are not trusted. Before storing results, davinci-fold compares the public outputs
of the final proof with its own state:

- the proof comes from a finalize step and its state root equals the local state root;
- the fold count, the voter count and the overwrite count match the local ones;
- the aggregator key the proof commits to equals the proof's own program key, and the batch
  key equals the one learned from the first batch proof;
- both keys match the pinned circuit release;
- the decrypted tally equals the locally decrypted tally;
- the election's configuration commitment, `sha256(config ‖ batch key ‖ aggregator key)`, is
  recomputed from the creation parameters and equals the one in the proof.

Each fold re-verifies the batch STARKs it consumes inside the circuit, so a forged or
corrupted proof fails to fold; importing a proof from another prover adds no trust.

## Code layout

| Path | Contents |
|---|---|
| `cmd/davinci-fold` | Entry point and configuration (flags and `DAVINCIFOLD_*` variables). |
| `cmd/test-keywarden` | Local keywarden for testing. |
| `api` | HTTP router, JWT authentication, handlers and error codes. |
| `orchestrator` | Election engine: ingest and validation, sealing, lifecycle, scheduling, folding, finalize. |
| `workers` | Prover registry, health polling and bans. |
| `storage` | PebbleDB store, key layout, reservations and audit log. |
| `keywarden` | Client side of the encrypted-tally and decryption-key calls. |
| `service` | Wiring between the components and their start/stop. |
| `types` | Election, vote, batch and results records. |
| `log`, `crypto` | Helpers copied from davinci-node. |
| `tests` | Integration tests and their harness. |

Proving primitives (state, folding requests, digests, the prover client) come from the
davinci-zkvm Go SDK, mostly its `chain` package.
