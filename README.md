# davinci-fold

Distributed orchestrator for the **chained/folded** DAVINCI proving pipeline.

davinci-fold is the "brain": one Go node that coordinates a pool of remote
RTX 5090 prover workers (the [davinci-zkvm](https://github.com/vocdoni/davinci-zkvm)
Rust service). It ingests votes over an authenticated API, validates and
de-duplicates them, owns the canonical election state, decides when batches
seal and when an election ends, and schedules proving across the pool —
surviving worker death and its own restarts.

Scope is **exclusively the chained/folded approach**: batches are proved
STARK-only, recursively folded server-side, and finalized to one PLONK per
election.

## Contents

- [Architecture](#architecture)
- [Election lifecycle](#election-lifecycle)
- [Layout](#layout)
- [Build & run](#build--run)
- [Configuration](#configuration)
- [Authentication](#authentication)
- [HTTP API](#http-api)
- [End-to-end walkthrough](#end-to-end-walkthrough)
- [Workers](#workers)
- [Resilience & recovery](#resilience--recovery)
- [Keywarden](#keywarden)
- [Verifiability](#verifiability)
- [Test](#test)

## Architecture

- **Scatter/gather.** Independent batch STARKs are scattered across the whole
  worker pool; their proof blobs are gathered onto one per-election *fold
  worker* that runs the serial fold chain and finalize.
- **Key-abstracted finalize.** The orchestrator holds only the voter
  **encryption public key**. At election end it publishes the encrypted results
  ciphertext; an external **keywarden** returns a decryption key. The private
  key is never disclosed to the orchestrator. v1 ships a local test keywarden;
  a future on-chain DKG slots into the same two-phase handshake.
- **Crash-resilient.** Votes (ordered), exact per-batch prove inputs and fold
  checkpoints are persisted to PebbleDB, so proving is always re-drivable. A
  worker dying costs proving time, not correctness.

### Proving pipeline

```
votes ──ingest+validate──▶ pending buffer ──seal──▶ batch
                                                      │
                                       ApplyBatch + persist prove inputs
                                                      │
                                              SubmitProve (stark)
                                  scattered across least-loaded healthy workers
                                                      │
                        FetchStarkInfo (learn batch_vk) + pull raw proof.bin
                                                      │
                              POST /jobs/import ──▶ pinned fold worker
                                                      │
                                  every foldEvery imports: SubmitFold
                                  (serial fold chain, checkpointed)
                                                      │
                                election Ended ──▶ drain ──▶ publish ciphertext
                                                      │
                              keywarden key ──▶ SubmitFinalize ──▶ one PLONK
```

A batch STARK is independent and provable on any worker; only the fold chain is
pinned to a single worker, because a fold references STARKs that live in that
worker's local store. The `POST /jobs/import` endpoint on the Rust service is
what lets a STARK proved on worker A be folded on worker B.

## Election lifecycle

The orchestrator owns a persisted state machine. A monitor goroutine sweeps all
loaded elections once per second: it seals partial batches that have aged past
the batch time window, ends elections past their end time, and drives ended
elections toward published results.

```
Active ──endTime──▶ Ended ──drain──▶ Decrypting ──key──▶ Finalizing ──PLONK──▶ Results
                                                              │
                                                       (on error, back to Decrypting)
```

| State | Meaning |
|---|---|
| `Active` | Accepting votes. Batches seal by size or time window; STARKs scatter to the pool; folds run on cadence. |
| `Ended` | End time passed. No new votes. The final partial batch is sealed and the monitor drains all remaining batches and pending folds onto the fold chain. |
| `Decrypting` | The encrypted results ciphertext is published. The orchestrator is waiting for the keywarden to submit a decryption key. |
| `Finalizing` | A decryption key was accepted. The fold worker is decrypting results, producing the final PLONK, and the orchestrator is verifying the digest. |
| `Results` | Final tally and PLONK are available. Terminal. |

`Paused` and `Canceled` are administrative states; a `Canceled` election is
terminal and is not resumed on restart. Election creation persists directly as
`Active`.

Each transition is journaled to PebbleDB, so a restart resumes mid-flight. The
two drives that could otherwise double-run are single-flighted per election: the
`Ended → Decrypting` drain and the `Decrypting → Finalizing → Results` finalize
each hold a per-election guard, so the once-per-second monitor and a second
decryption-key submission cannot start a duplicate run.

## Layout

| Path | Notes |
|---|---|
| `cmd/davinci-fold/` | Entrypoint + viper/pflag config (`DAVINCIFOLD_` env prefix). |
| `cmd/test-keywarden/` | v1 local keywarden. |
| `api/` | chi router, typed `Error` model, JWT-authenticated admin/keywarden routes. |
| `service/` | Dependency-injection wiring and service lifecycles. |
| `storage/` | Pebble-backed store + reservation/recovery. |
| `orchestrator/` | Scatter/gather scheduler + fold-chain driver. |
| `workers/` | Worker registry, health poll, ban/backoff, client pool. |
| `keywarden/` | Client for the publish-ciphertext / receive-key handshake. |
| `types/` | Election, Vote, Status, request/response types. |

Proving primitives are reused from the davinci-zkvm
[`go-sdk`](https://github.com/vocdoni/davinci-zkvm/tree/main/go-sdk) and its
`chain` package; infrastructure parity (`log`, `db`) is reused from
[davinci-node](https://github.com/vocdoni/davinci-node).

## Build & run

```sh
go build ./cmd/davinci-fold
go build ./cmd/test-keywarden

./davinci-fold --api.jwtSecret=<secret> --datadir=$HOME/.davinci-fold
```

The orchestrator listens on `0.0.0.0:8888` by default. It starts with an empty
worker pool and no elections; register workers and create elections over the
API (see the [walkthrough](#end-to-end-walkthrough)).

## Configuration

Configuration is via flags or `DAVINCIFOLD_*` environment variables. A dotted
flag maps to an underscored, prefixed env var: `--api.port` →
`DAVINCIFOLD_API_PORT`, `--api.jwtSecret` → `DAVINCIFOLD_API_JWTSECRET`.

| Flag | Short | Env | Default | Notes |
|---|---|---|---|---|
| `--api.host` | `-h` | `DAVINCIFOLD_API_HOST` | `0.0.0.0` | API bind address. |
| `--api.port` | `-p` | `DAVINCIFOLD_API_PORT` | `8888` | API port. |
| `--api.jwtSecret` | | `DAVINCIFOLD_API_JWTSECRET` | — | HMAC secret for admin/keywarden JWTs. **Required.** |
| `--batch.size` | | `DAVINCIFOLD_BATCH_SIZE` | `64` | Seal a batch once this many votes accumulate. Range `2`–`128` (circuit `MAX_BATCH_SIZE`). |
| `--batch.time` | `-b` | `DAVINCIFOLD_BATCH_TIME` | `5m` | Seal a partial batch once its oldest vote is older than this, so low-traffic elections still progress. |
| `--fold.every` | | `DAVINCIFOLD_FOLD_EVERY` | `4` | Fold after this many imported batch STARKs. Minimum `1`. |
| `--worker.pollPeriod` | | `DAVINCIFOLD_WORKER_POLLPERIOD` | `10s` | Health-poll interval per prover worker. |
| `--log.level` | `-l` | `DAVINCIFOLD_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`, `fatal`. |
| `--log.output` | `-o` | `DAVINCIFOLD_LOG_OUTPUT` | `stdout` | `stdout`, `stderr` or a file path. |
| `--log.disableAPI` | | `DAVINCIFOLD_LOG_DISABLEAPI` | `false` | Disable the API logging middleware. |
| `--datadir` | `-d` | `DAVINCIFOLD_DATADIR` | `~/.davinci-fold` | Database and storage directory. |

`batchSize` and `foldEvery` can also be set per election in the create request,
overriding these process-wide defaults for that election.

## Authentication

Admin and keywarden routes carry a signed JWT
(`Authorization: Bearer <token>`), HMAC-SHA256 over `--api.jwtSecret`. Vote
submission is self-authenticating and public — no token.

A token is a standard HS256 JWT with two claims that the server checks plus an
expiry:

| Claim | Value |
|---|---|
| `role` | `admin` or `keywarden` |
| `sub` | a subject string, recorded in the audit log |
| `exp` | unix expiry |

Any JWT library can mint one. In Go:

```go
tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
    "role": "admin",
    "sub":  "ops",
    "exp":  time.Now().Add(time.Hour).Unix(),
})
signed, _ := tok.SignedString([]byte(jwtSecret))
```

The `admin` role creates elections and registers workers; the `keywarden` role
fetches the encrypted results and submits the decryption key. Every mutating
call is written to a Pebble audit namespace with its subject, role and action.

## HTTP API

| Method | Path | Auth | Purpose |
|---|---|---|---|
| GET | `/ping` | — | Liveness probe. |
| GET | `/info` | — | Version, batch/fold config, worker and election counts. |
| POST | `/elections` | admin | Create an election. |
| GET | `/elections` | — | List elections. |
| GET | `/elections/{id}` | — | Election status and config. |
| POST | `/elections/{id}/votes` | self | Submit a ballot (Groth16 proof + ECDSA + census). |
| GET | `/elections/{id}/votes/{voteID}` | — | Vote record and pipeline status. |
| GET | `/elections/{id}/encrypted-results` | keywarden | Results ciphertext, once the election is Decrypting. |
| POST | `/elections/{id}/decryption-key` | keywarden | Submit the decryption key; triggers finalize. |
| GET | `/elections/{id}/results` | — | Final tally + PLONK, once the election reaches Results. |
| GET | `/workers` | — | Worker pool stats. |
| POST | `/workers/register` | admin | Register a prover worker. |

Election IDs and vote IDs in paths are `0x`-prefixed hex.

## End-to-end walkthrough

This drives one election from creation to final tally. It assumes the
orchestrator is running on `127.0.0.1:8888` and a davinci-zkvm prover worker is
running on `127.0.0.1:8080`. `$ADMIN_JWT` and `$KEYWARDEN_JWT` are tokens minted
as described under [Authentication](#authentication).

**1. Register a prover worker** (admin):

```sh
curl -X POST http://127.0.0.1:8888/workers/register \
  -H "Authorization: Bearer $ADMIN_JWT" \
  -H 'Content-Type: application/json' \
  -d '{"address":"http://127.0.0.1:8080","name":"gpu-0"}'
```

**2. Generate the election encryption keypair** with the keywarden. This prints
the public coordinates `encX`/`encY` and writes the private scalar to the
keyfile — the orchestrator never sees it.

```sh
./test-keywarden --mode=keygen --keyfile=keywarden-key.json
# encX=0x...
# encY=0x...
```

**3. Create the election** (admin). Field-element values are big-endian hex;
`vk` is the Groth16 ballot verification key the orchestrator checks each ballot
against. `endTime` is when the monitor moves the election to Ended.

```sh
curl -X POST http://127.0.0.1:8888/elections \
  -H "Authorization: Bearer $ADMIN_JWT" \
  -H 'Content-Type: application/json' \
  -d '{
    "processID":    "0x...",
    "ballotMode":   "0x01",
    "encX":         "0x...",
    "encY":         "0x...",
    "censusOrigin": 1,
    "censusRoot":   "0x...",
    "vk":           { "...": "..." },
    "batchSize":    64,
    "foldEvery":    4,
    "endTime":      "2026-06-15T18:00:00Z"
  }'
```

The election ID is derived from `processID`. Use it as `{id}` below.

**4. Submit votes** (self-authenticating, no token). Each ballot carries its
Groth16 proof, ECDSA signature and census proof; the orchestrator verifies all
three and de-duplicates by vote ID and address before buffering.

```sh
curl -X POST http://127.0.0.1:8888/elections/<id>/votes \
  -H 'Content-Type: application/json' \
  -d @vote.json
```

A resubmitted vote ID is rejected as a duplicate; a new ballot for an existing
address is applied as an overwrite (the net accumulator subtracts the old
ballot). Batches seal at `batchSize` or after `batchTime`, and their STARKs are
scattered to the pool immediately.

**5. The election ends automatically** at `endTime`. The monitor seals the
final batch, drains the fold chain, publishes the encrypted results ciphertext,
and moves the election to `Decrypting`. No call is needed.

**6. Finalize** with the keywarden (keywarden token). It fetches the published
ciphertext and returns the decryption key, which triggers decrypt + final PLONK
+ digest verification on the fold worker.

```sh
./test-keywarden --mode=finalize \
  --keyfile=keywarden-key.json \
  --orchestrator=http://127.0.0.1:8888 \
  --token=$KEYWARDEN_JWT \
  --election=<id>
```

**7. Fetch the results** — final tally plus the four Solidity-ready PLONK fields
for on-chain verification:

```sh
curl http://127.0.0.1:8888/elections/<id>/results
```

## Workers

The pool starts empty. Workers are registered at runtime with
`POST /workers/register` (admin); there is no startup worker list. Each entry is
a base URL for a davinci-zkvm prover service.

- **Health.** Every worker is polled on `--worker.pollPeriod`. Missed or
  timed-out polls mark a worker unhealthy and apply ban/backoff before it is
  retried.
- **Scatter.** Sealed batch STARKs go to the least-loaded healthy worker by
  reported queue length, so proving spreads across the pool.
- **Fold pinning.** Each election pins one fold worker that gathers the imported
  batch blobs and runs the serial fold chain and finalize. Batch proving on
  other workers overlaps with folding on the pinned one.

`GET /workers` reports the pool; `GET /info` reports the live worker and
election counts.

## Resilience & recovery

The orchestrator never loses votes and can always re-drive proving, because the
durable state is enough to recompute everything downstream:

- Votes are persisted in arrival order.
- Exact per-batch prove inputs (the request that produced each batch STARK) are
  persisted, so any batch can be re-proved.
- Fold checkpoints (`foldCount`, last fold job, state root) are persisted.
- `chain.State` snapshots are persisted; restore is required because
  re-encryption uses a fresh random `k` per ballot, so a replay is not
  byte-reproducible.

What this buys:

- **A batch prove fails or its worker dies.** The prove is retried on another
  healthy worker. Because the exact prove inputs are persisted, the batch stays
  re-drivable across an orchestrator restart too — correctness is unaffected,
  the cost is proving time.
- **A worker goes unhealthy.** Health polling bans it after repeated failures
  and routes new work to healthy workers; an election's pinned fold worker is
  re-pinned to a healthy one when its current fold worker is banned or
  unreachable.
- **The orchestrator restarts.** It restores each live election's `chain.State`
  from its latest snapshot, recomputes the next batch sequence from persisted
  batches, restores the fold checkpoint (`aggVK`, `batchVK`, last fold job, fold
  count) so the chain resumes rather than re-folding from genesis, reloads the
  lifecycle state, and resumes proving any persisted-but-undispatched batches.
  Dispatch is idempotent — already-imported batches are skipped — and finalized
  or canceled elections are skipped entirely.

## Keywarden

The orchestrator is key-abstracted: it holds only the encryption public key and
never the private scalar. The handshake is two-phase — the orchestrator
publishes the encrypted results ciphertext at election end, and an external
keywarden returns a decryption key.

v1 ships `cmd/test-keywarden`, which owns the keypair locally:

- `--mode=keygen` generates a BabyJubJub ElGamal keypair, writes
  `keywarden-key.json` (`encX`, `encY`, `priv`, all `0x` big-endian hex), and
  prints the public coordinates for election creation.
- `--mode=finalize` reads the keyfile, fetches the election's encrypted results
  over `GET /elections/{id}/encrypted-results`, and submits the private scalar
  to `POST /elections/{id}/decryption-key`, triggering finalize.

This stands in for a future on-chain DKG: the ciphertext goes on-chain, DKG
participants return decryption shares, and the key is reconstructed. The same
publish/return handshake is built to accept that variant — only the keywarden
implementation changes, not the orchestrator.

## Verifiability

The final PLONK is independently checkable by anyone holding only the
orchestrator binary and on-chain election parameters; nothing the workers say is
trusted. The digest verification at finalize binds, in one chain:

- the fold guest's self-reference (`digest.FoldVK == proof.ProgramVK`),
- the orchestrator's hardcoded circuit-release anchor
  (`proof.ProgramVK == CircuitRelease.AggVK`), pinned in the binary rather than
  supplied by a worker,
- the genesis configuration commitment
  (`sha256(config frame ‖ batch_vk ‖ fold_vk)`),
- and equality of the final state root, decrypted results and voter/overwrite
  counts against the orchestrator's own canonical state.

Tally soundness and per-ballot validity are enforced in-circuit: the fold guest
re-verifies every batch STARK, so a forged or imported-garbage blob fails to
fold rather than corrupting the result. Importing a STARK adds no trust.

## Test

Unit tests need no external services:

```sh
go test ./...
```

The integration suite is gated by `RUN_INTEGRATION_TESTS` and drives real
ballots through the full ingest → scatter/gather → fold → finalize pipeline.
Proving tests (the scatter/gather E2E) need a running davinci-zkvm worker pool,
passed as a comma-separated list in `DAVINCI_FOLD_WORKER_URLS`; the adversarial
ingest test verifies ballots on CPU and runs without workers.

```sh
# Full integration suite against a real worker pool.
RUN_INTEGRATION_TESTS=1 DAVINCI_FOLD_WORKER_URLS=http://127.0.0.1:8080 \
  go test ./tests/... -v -timeout 30m

# Adversarial ingest only (no GPU, no workers).
RUN_INTEGRATION_TESTS=1 go test ./tests/ -run TestAdversarialIngest -v
```

The E2E test is parameterized via `E2E_BATCHES`, `E2E_BATCH_SIZE`,
`E2E_FOLD_EVERY` and `E2E_OVERWRITE_BATCHES` and doubles as a benchmark:
1024 votes (batch 128, fold every 4) run submission-to-verified-results in
14m39s (1.17 votes/s) on a single RTX 5090 worker. Full numbers live in
[davinci-zkvm's BENCHMARK.md](https://github.com/vocdoni/davinci-zkvm/blob/main/BENCHMARK.md).
