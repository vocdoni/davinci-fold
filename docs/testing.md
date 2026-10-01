# Testing

## Unit tests

```sh
go test ./...
```

Unit tests need no provers, GPU or external network. They cover storage and restart recovery
(on the in-memory and the Pebble backends), the prover pool against a fake `/health` server
(health, bans, one job per prover at a time), the API handlers (JWT checks, error codes,
status changes, the decryption-key call, log redaction, stored worker registrations), the
election engine (ingest checks, census proofs, sealing of overwrites and of votes the state
refuses, status changes, the background finalize, restore of pending votes and of worker
registrations, retry backoff, using synthetic ballots), the scheduler against fake provers
(fold cadence and checkpoints, cancel, prover removal, a chain moved without its last fold's
proof, folds that keep failing, the reuse of prove jobs a previous run left and the claim on
their provers, canceled and finished elections leaving the scheduler) and the keywarden
client. `go test ./...` also runs the chaos tests below.
Set `LOG_LEVEL=debug` to see the service logs.

## Integration tests

The `tests` package boots the whole service in-process, on a free port with a temporary
database, and talks to it over HTTP; with `DAVINCI_FOLD_URL` set it talks to an orchestrator
running elsewhere instead (see [Live run](#live-run)). Its integration tests only run when
`RUN_INTEGRATION_TESTS` is set to anything other than `false`; otherwise they skip, which
keeps `go test ./...` fast. The chaos tests run either way.

| Test | Needs | Checks |
|---|---|---|
| `TestAPILifecycle`, `TestAPIAuthRejections`, `TestAPIElectionStatus`, `TestWorkerRegistration` | nothing | HTTP surface, token checks, status changes and prover removal. |
| `TestAdversarialIngest` | CPU | Real Groth16 ballots; every tampered submission (a non-member's, another ballot under a valid proof, a padded field, encodings the provers cannot parse) is rejected at ingest with its reason, and a clean one is accepted and stored re-encoded. |
| `TestIngestCensusWeight` | CPU | A member whose census weight differs from the one its ballot proof committed to is rejected. |
| `TestBallotVKPerElection` | CPU | Ballot proofs are checked against the election's own `vk`: a proof valid only under another key is rejected. |
| `TestScatterGatherE2E` | 1 prover | A full election with overwrites, from ballots to results, checking the tally and settled votes and verifying the final PLONK on a simulated EVM. |
| `TestFailoverE2E` | 2 provers | Three elections at once, with a crash of the orchestrator and a fold worker removed and registered again (see [Live run](#live-run)). |

```sh
# Everything that runs without provers.
RUN_INTEGRATION_TESTS=1 go test ./tests/ -v -timeout 30m

# End to end against a running davinci-zkvm prover.
RUN_INTEGRATION_TESTS=1 DAVINCI_FOLD_WORKER_URLS=http://127.0.0.1:8080 \
  go test ./tests/ -run TestScatterGatherE2E -v -timeout 60m
```

The end-to-end tests register every prover through the API. They need:

- the provers in `DAVINCI_FOLD_WORKER_URLS` (comma-separated base URLs), running the circuit
  release pinned by the Go SDK in `go.mod`, with the aggregator guest enabled;
- a davinci-zkvm checkout next to this repository (`../davinci-zkvm`), whose `solidity/`
  verifier it compiles;
- `solc` on the `PATH`, or Docker to run the `ethereum/solc` image.

Ballots are generated on the CPU before the election starts, which is the slow part for large
runs. The election closes 45 seconds after it is created. The run is sized with these
variables:

| Variable | Default | Meaning |
|---|---|---|
| `E2E_BATCHES` | `3` | Number of batches. |
| `E2E_BATCH_SIZE` | `2` | Votes per batch. |
| `E2E_FOLD_EVERY` | `1` | Batches per fold. |
| `E2E_OVERWRITE_BATCHES` | `1` | Trailing batches that re-vote earlier voters; must be below `E2E_BATCHES`. |

With larger values the test doubles as an end-to-end benchmark. Measured throughput is
recorded in davinci-zkvm's
[BENCHMARK.md](https://github.com/vocdoni/davinci-zkvm/blob/main/BENCHMARK.md).

### Live run

`scripts/e2e-live.sh` runs `TestFailoverE2E` against real provers, everything in Docker:

```sh
DAVINCI_FOLD_WORKER_URLS=http://<prover-a>:8080,http://<prover-b>:8080 scripts/e2e-live.sh
```

It builds the image from this tree (`davinci-fold:e2e`) and runs it on `127.0.0.1:$E2E_PORT`
with host networking, a fresh data volume, the test JWT secret and debug logs. The test runs
in a `golang:1.25` container with this repository and davinci-zkvm mounted read-only, Go
caches in volumes, the orchestrator's data volume, the Docker socket, and `solc` copied from
the `ethereum/solc:0.8.28` image. It reaches the orchestrator at `DAVINCI_FOLD_URL`; through
the Docker Engine API it kills its container (`DAVINCI_FOLD_CONTAINER`) and starts it again,
reads its logs, and stops it at the end to read its records (`DAVINCI_FOLD_DATADIR`). Without
`DAVINCI_FOLD_URL` the test runs in-process: the crash is a restart and the logs are not
checked.

The scenario runs elections A (4 batches of 2 votes, a fold every batch), B (3 of 3) and C (3
of 4, both folding every 2 batches); the last batch of each re-votes its first voters:

1. A's first two batches are sealed together, so they are proved on two provers; B and C
   start voting.
2. The orchestrator is killed while those batches are proved and started again; voting goes
   on.
3. Once A has folded, its fold worker is removed (`DELETE /workers/{id}`) and registered
   again a minute later; A goes on voting.
4. B ends at its end time, A and C through the status endpoint, and each decryption key is
   submitted once its election is `decrypting`.

For each election it checks the exact tally, every vote `settled`, the final PLONK verified
on a simulated EVM with its digest counting every ballot and overwrite, and, from the
orchestrator's records, that every batch was proved, imported and folded in order up to the
state root and fold count the proof attests. It also checks that one election was proved on
every prover, that A's chain moved and went on folding where it moved to, and that the
orchestrator's logs hold no decryption key, JWT or JWT secret. It logs a summary table:
batches, folds, provers, the provers each chain was folded on, chain moves and the time to
results.

| Variable | Default | Meaning |
|---|---|---|
| `DAVINCI_FOLD_WORKER_URLS` | | Two or more prover base URLs, comma-separated. |
| `E2E_ZKVM_DIR` | `../davinci-zkvm` | davinci-zkvm checkout. |
| `E2E_PORT` | `28888` | Orchestrator API port. |
| `E2E_LOG_DIR` | `~/.cache/davinci-fold-e2e` | Logs, a directory per run: `build.log`, `test.log`, `orchestrator.log`. |
| `E2E_TIMEOUT` | `90m` | `go test` timeout. |

Containers and volumes are named `davinci-fold-e2e-*`. The containers and the data volume
are removed when the run ends; the Go cache volumes are kept.

## Chaos tests

The `TestChaos*` tests run one election each, with real ballots, against fake provers
(`tests/helpers.FakeProver`, an `httptest` server). A fake prover answers every call the
orchestrator makes (prove, job status, STARK info, raw proof, import with `kind`, fold,
finalize, publics, PLONK) with deterministic proofs that record what a real proof attests,
and checks what the guests check about the chain: each fold extends a fold proof of the same
election and fold key, and its batches are of the pinned batch circuit and continue each
other's state roots. A test can hold a job kind running, make every job run for a set time,
fail the next jobs, run other guests (another program key), make batch proofs every fold
rejects, or stop a prover; the fake records when each job ran and the order of the batch
proofs imported into it. Each test breaks the orchestrator or a
prover at one point and checks that the election reaches results with the expected tally,
that every vote is `settled`, that the fold chain behind the results folds every sealed batch
once and in order, and that the stored proofs are dropped.

| Test | Breaks |
|---|---|
| `TestChaosRestartUnsealedVotes` | A restart with an accepted vote no batch holds. |
| `TestChaosRestartImportedBatches` | A restart with two batches imported but not folded (`foldEvery` 3). |
| `TestChaosRestartMidProve` | A restart while a batch is proved: the job is reused. |
| `TestChaosRestartMidDrain` | A restart while an ended election is drained, a fold running. |
| `TestChaosRestartMidFinalize` | A restart during the final proof; the key is submitted again. |
| `TestChaosFoldWorkerDeathBetweenFolds` | The fold worker stops between two folds. |
| `TestChaosFoldWorkerDeathDuringFold` | The fold worker stops during a fold. |
| `TestChaosFoldWorkerFailing` | The fold worker fails folds while it stays up. |
| `TestChaosStaleProver` | A prover runs other guests (another batch and aggregator key): its batch proofs are refused and the chain first pinned to it moves. |
| `TestChaosRejectedProof` | A batch proof every fold rejects: after three failed folds the batch is proved again on the other prover. |
| `TestChaosBatchProverDeath` | A prover stops while it proves a batch. |
| `TestChaosRemoveFoldWorker` | The fold worker is removed through the API, then a restart. |
| `TestChaosScatter` | Eight batches on three provers: the proves overlap, no prover runs two jobs at once, a prover dies mid-prove and its batch is proved on another one while the rest go on; imports and folds stay in sequence order. |
| `TestChaosRestartMidScatter` | A restart during the drain, with three batches proved at once: each waits for its own job again, and no batch is proved twice. |

They take about 15 seconds together, most of it generating the ballots once.

```sh
go test ./tests/ -run TestChaos -v
```

## Docker

The Compose `test` profile runs both suites in the builder image, without provers:

```sh
docker compose --profile test up unit-test
docker compose --profile test up integration-test
```
