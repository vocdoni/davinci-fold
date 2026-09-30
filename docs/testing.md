# Testing

## Unit tests

```sh
go test ./...
```

Unit tests need no provers, GPU or network. They cover storage and restart recovery, the
prover pool against a fake `/health` server, the API handlers with JWT checks, the election
engine (ingest checks, census proofs, sealing of overwrites, lifecycle and restore, using
synthetic ballots) and the keywarden client. Set `LOG_LEVEL=debug` to see the service logs.

## Integration tests

The `tests` package boots the whole service in-process, on a free port with a temporary
database, and talks to it over HTTP. It only runs when `RUN_INTEGRATION_TESTS` is set to
anything other than `false`; otherwise it exits at once, which keeps `go test ./...` fast.

| Test | Needs | Checks |
|---|---|---|
| `TestAPILifecycle`, `TestAPIAuthRejections`, `TestWorkerRegistration` | nothing | HTTP surface and token checks. |
| `TestAdversarialIngest` | CPU | Real Groth16 ballots; every tampered submission (a non-member's, another ballot under a valid proof, a padded field, encodings the provers cannot parse) is rejected at ingest, and a clean one is accepted and stored re-encoded. |
| `TestIngestCensusWeight` | CPU | A member whose census weight differs from the one its ballot proof committed to is rejected. |
| `TestBallotVKPerElection` | CPU | Ballot proofs are checked against the election's own `vk`: a proof valid only under another key is rejected. |
| `TestScatterGatherE2E` | 1 prover | A full election with overwrites, from ballots to results, checking the tally and verifying the final PLONK on a simulated EVM. |
| `TestChaos*` | 1 or 2 provers | Placeholders for prover and restart failure scenarios; currently skipped. |

```sh
# Everything that runs without provers.
RUN_INTEGRATION_TESTS=1 go test ./tests/ -v -timeout 30m

# End to end against a running davinci-zkvm prover.
RUN_INTEGRATION_TESTS=1 DAVINCI_FOLD_WORKER_URLS=http://127.0.0.1:8080 \
  go test ./tests/ -run TestScatterGatherE2E -v -timeout 60m
```

The end-to-end test needs:

- one or more provers in `DAVINCI_FOLD_WORKER_URLS` (comma-separated base URLs), running the
  circuit release pinned by the Go SDK in `go.mod`, with the aggregator guest enabled;
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

## Docker

The Compose `test` profile runs both suites in the builder image, without provers:

```sh
docker compose --profile test up unit-test
docker compose --profile test up integration-test
```
