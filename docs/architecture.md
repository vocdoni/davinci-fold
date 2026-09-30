# Architecture

davinci-fold is one Go process: an HTTP API, an orchestration engine that owns the state of
every election, a pool of remote davinci-zkvm provers and a PebbleDB store. Proof generation
happens on the provers; davinci-fold decides what to prove, where, and in which order, and
checks the final result.

## Proving pipeline

1. **Ingest.** A ballot is accepted only if its Groth16 ballot proof verifies against the
   davinci-circom verification key, its public inputs match the submitted address and vote ID,
   the voter's ECDSA signature over the vote ID recovers the submitted address, and its census
   proof targets the election's census root. The census membership proof itself is verified
   by the batch circuit. A vote ID is accepted once; a later ballot from the same voter lands
   on the same state slot and overwrites the earlier one.
2. **Seal.** Accepted votes wait in a per-election buffer. A batch is sealed when the buffer
   reaches the batch size, when its oldest vote is older than `--batch.time`, or when the
   election ends. Sealing applies the batch to the election state (ballot tree, re-encryption,
   encrypted tally) and persists the exact prove request and the new state snapshot.
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
   previous one. A checkpoint (fold count, last fold job, program keys) is persisted after each
   fold.
6. **Finalize.** When the election ends, the remaining batches are dispatched and folded and
   the encrypted tally is published. Once the keywarden returns the private key, davinci-fold
   decrypts the tally, builds a Chaum-Pedersen proof for every decrypted field and sends both
   to the fold worker, which verifies them in-circuit and produces the final PLONK proof.

## Election lifecycle

A monitor sweeps all open elections once per second, sealing batches that aged past the
batch window, ending elections past their end time and driving ended elections forward.

```
active ──endTime──▶ ended ──drain──▶ decrypting ──key──▶ finalizing ──PLONK──▶ results
                                          ▲                   │
                                          └───── on error ────┘
```

| Status | Meaning |
|---|---|
| `active` | Accepting votes; batches are sealed, proved and folded. |
| `ended` | Past `endTime`. No new votes; the last batch is sealed and the chain drained. |
| `decrypting` | The encrypted tally is published and davinci-fold waits for the key. |
| `finalizing` | A key was submitted; the final fold and PLONK are running. |
| `results` | Tally and PLONK proof are available. Final. |

If finalize fails, the election returns to `decrypting` and the keywarden can submit the key
again. The drain and the finalize each run at most once at a time per election. The data
model also defines `created`, `paused` and `canceled`; nothing sets them.

## Provers

Provers are davinci-zkvm services registered through `POST /workers/register`. The pool is
held in memory and starts empty on every start.

- **Health.** Each prover is polled on `GET /health` every `--worker.pollPeriod`. A prover
  that does not answer is skipped when picking where to send work.
- **Bans.** A failed job counts against the prover that ran it. After more than three
  consecutive failures it is banned for 30 minutes; a success resets the count. A job that
  runs past 30 minutes counts as failed.
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
Worker registrations are not stored: register the provers again after a restart.

A restart does not yet recover votes accepted but not sealed into a batch, nor batches
imported on the fold worker but not yet folded.

## Keys and the keywarden

davinci-fold holds only the election's ElGamal public key while voting is open. When the
election reaches `decrypting`, any client with a `keywarden` token can fetch the encrypted
tally and post the private key back. davinci-fold uses the key to decrypt and prove the tally
and does not store it.

`test-keywarden` is a minimal keywarden that keeps the key pair in a local file. It is meant
for testing; any service that holds the key and a `keywarden` token can take its place.

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
