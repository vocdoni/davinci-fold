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
   the exact prove request, the new state snapshot and the votes' `batched` status in one
   write. If the state refuses a vote (its vote-ID
   key is already in the tree, for example), the state is restored from the last snapshot, the
   vote is dropped with status `error` and the rest of the batch is sealed.
3. **Prove.** Each sealed batch is sent as a STARK job to the healthy prover with the shortest
   queue; the prover and job are stored as soon as the job is accepted. A failed job is
   resubmitted, up to three attempts, each to the least-loaded prover that has not failed it
   yet. A proof whose program key is not the batch circuit's of the pinned release (a prover
   still running another guest, as in a rolling upgrade) counts as a failed job and is never
   stored. The batch proof (`proof.bin`) is downloaded from the prover that made it and
   stored.
   Batches of one election are dispatched in sequence order, one at a time; different
   elections run in parallel.
4. **Gather.** The stored batch proof is imported into the election's fold worker with
   davinci-zkvm's `POST /jobs/import`. A fold can only reference proofs in its own worker's
   job store, which is why the gather step exists.
5. **Fold.** After every `foldEvery` imported batches the fold worker folds them into the
   chain. The first fold runs a bootstrap pass to learn the aggregator's program key, which the
   guest cannot know about itself, and the genesis fold binds it; later folds extend the
   previous one. A fold whose program key is not the release's aggregator key is a failure of
   the fold worker. After each fold, one write stores the checkpoint (fold count, batches
   folded, last fold job and the worker holding it, program keys) and the fold's proof, and
   drops the proofs of the batches it folded; the votes of those batches become `folded`.
   Every failed fold is logged with the batches it held and the chain head. When fold jobs keep
   failing (three in a row) on the same head, the proofs of the pending batches are dropped
   and the batches proved again on other provers than the ones that made them, in case one
   proof is one the aggregator rejects. If folds of the new proofs keep failing too, the
   stored proof of the last fold is reported once, as an error, and the folds go on being
   retried.
6. **Finalize.** When the election ends, the remaining batches are dispatched and folded and
   the encrypted tally is published. Once the keywarden returns the private key, davinci-fold
   checks it against the election's public key and, in the background, decrypts the tally,
   builds a Chaum-Pedersen proof for every decrypted field and sends both to the fold worker,
   which verifies them in-circuit and produces the final PLONK proof. When it is verified and
   stored, every vote becomes `settled` and the stored proofs are dropped.

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

Work the monitor drives is retried when it fails: ending an election past its end time, the
dispatch of its batches and the drain of an ended election. The wait doubles after each
consecutive failure, from one second up to five minutes. Failures are logged as warnings,
the fifth in a row once as an error. A seal that fails when the election ends does not keep it
open: the election ends, and the drain seals the votes left.

## Provers

Provers are davinci-zkvm services registered through `POST /workers/register`. Registrations
(address and name) are stored and reloaded at start.

- **Health.** Each prover is polled on `GET /health` every `--worker.pollPeriod`, with a
  5-second limit. A prover that does not answer is skipped when picking where to send work.
- **Requests.** A prover must start answering a job call within 30 seconds; the whole call,
  a proof upload or download included, may take up to 5 minutes.
- **Bans.** A failed job or request counts against the prover that ran it. After more than
  three consecutive failures it is banned for 30 minutes; a success resets the count. A job
  that runs past `--worker.jobTimeout` (30 minutes by default) counts as failed.
- **Removal.** `DELETE /workers/{id}` takes a prover out of the pool and deletes its
  registration. Its running jobs count as failed and are sent elsewhere, as when a prover
  dies, and the fold chains pinned to it move at once.
- **Fold worker.** Each election pins one fold worker, chosen as the least-loaded prover when
  its first batch is gathered, and stores the pin (`foldWorker` in the election). The fold
  chain and the imported batch proofs live in that worker's job store, so davinci-fold keeps
  the proofs it needs to rebuild them elsewhere: the last fold's and those of the batches
  proved but not folded yet. A transient failure is retried on the same worker. The chain
  moves when the fold worker is removed, banned, misses more health polls in a row than the
  ban allows failed jobs, or fails an import, a fold or the finalize after its retries:
  davinci-fold pins the least-loaded other prover, imports the last fold onto it
  (`POST /jobs/import?kind=fold`) and the pending batches (`kind=batch`), stores the new pin
  and job IDs, and goes on there. If no other prover is available, the chain is imported
  again onto the same one, or waits for a prover and is retried; nothing is lost while it
  waits. The finalize moves the chain the same way. A chain whose last fold proof is not
  stored (one folded by an older version) starts again from genesis on the new prover,
  proving its batches again.
- **Circuit release.** davinci-fold checks the final proof against the circuit release pinned
  by the davinci-zkvm Go SDK it is built with (`chain.CircuitRelease`). Provers must run that
  release, with the aggregator guest enabled; proofs from any other build are rejected at
  finalize.

## Persistence and restarts

Everything lives in one PebbleDB database under `--datadir`: election records and status,
the vote log with per-vote status, the prove request of every sealed batch together with the
prover and job that proved it and the fold worker job it was imported as, fold checkpoints,
state snapshots, the proofs of the last fold and of the batches not folded yet, final
results, prover registrations and an audit log of admin, keywarden and system actions.

The state is restored from snapshots rather than rebuilt from the vote log because each batch
draws fresh re-encryption randomness, so replaying the votes would not reproduce the same
state. A vote and its status are stored in one write, and so are a sealed batch, the snapshot
after it and its votes' status, so a crash never leaves them apart.

davinci-fold can stop at any point and resume. On start it reloads the worker registrations,
then every election that is still open:

- its state from the latest snapshot, and the accepted votes no batch holds back into the
  pending buffer, in submission order, to be sealed later (they are still refused as
  duplicates);
- its fold chain from the fold checkpoint and the stored batches. Batches imported on the
  pinned fold worker but not folded are folded in sequence order. A proved batch not imported
  there is imported from its stored proof. A batch whose prove job was running or done on a
  prover that still has it reuses that job; any other sealed batch is proved again, which is
  safe because its prove request is fixed at seal time;
- an `ended` election resumes its drain, and a `finalizing` one goes back to `decrypting` with
  `finalizeError` `finalize interrupted`: the key is never stored, so the keywarden submits it
  again.

Dispatch skips what is already done, so repeating it is harmless. A batch being proved is
reserved; a reservation older than three job timeouts plus five minutes is freed.

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
