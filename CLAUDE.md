# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```sh
go build ./cmd/davinci-fold ./cmd/test-keywarden   # build both binaries
go test ./...                                       # unit tests (no external services)
go test ./orchestrator/ -run TestEngine -v          # single test
go vet ./...
golangci-lint run                                   # CI uses v2.5; formatter is gofumpt
```

CI also enforces a clean `go mod tidy`, `go fix ./...`, `go generate ./...` (no diffs allowed) and runs `deadcode -test ./...`.

Integration tests are gated by `RUN_INTEGRATION_TESTS`:

```sh
# Full pipeline against real davinci-zkvm GPU workers:
RUN_INTEGRATION_TESTS=1 DAVINCI_FOLD_WORKER_URLS=http://127.0.0.1:8080 go test ./tests/... -v -timeout 30m

# Adversarial ingest — verifies ballots on CPU, needs no workers:
RUN_INTEGRATION_TESTS=1 go test ./tests/ -run TestAdversarialIngest -v
```

## What this is

Distributed orchestrator for the **chained/folded** DAVINCI proving pipeline. One Go node coordinates a pool of remote GPU prover workers (the davinci-zkvm Rust service). Scope is exclusively the folded approach: batches are proved STARK-only, recursively folded server-side, and finalized to one PLONK per election.

## Architecture

Data flow: votes → ingest/validate/dedup → pending buffer → seal batch → STARK prove (scattered) → import to fold worker → serial fold chain → election end → drain → publish ciphertext → keywarden key → finalize → one PLONK.

- **Scatter/gather:** batch STARKs are independent and go to the least-loaded healthy worker. The fold chain is serial and **pinned to one worker per election** (a fold references STARKs in that worker's local store); `POST /jobs/import` on the Rust service moves STARKs between workers.
- **Election lifecycle state machine** (`orchestrator/lifecycle.go`): `Active → Ended → Decrypting → Finalizing → Results`, plus administrative `Paused`/`Canceled`. A monitor goroutine sweeps all elections once per second (seals aged partial batches, ends elections, drives ended ones to results). The drain and finalize drives are single-flighted per election.
- **Crash resilience is a core invariant:** votes (in arrival order), exact per-batch prove inputs, fold checkpoints, and `chain.State` snapshots are all persisted to PebbleDB. Anything downstream must remain re-drivable after a worker death or orchestrator restart; dispatch is idempotent. State restore (not replay) is required because ballot re-encryption uses a fresh random `k`.
- **Key abstraction:** the orchestrator holds only the encryption *public* key. At election end it publishes the encrypted results ciphertext; an external keywarden (`cmd/test-keywarden` in v1, on-chain DKG later) returns the decryption key via the same two-phase handshake.
- **Trust model:** nothing a worker says is trusted. Finalize verifies a digest chain binding the fold guest's VK to a circuit-release anchor hardcoded in the binary, the genesis config commitment, and the orchestrator's own canonical state/results.

## Package map

- `cmd/davinci-fold/` — entrypoint; viper/pflag config, `DAVINCIFOLD_*` env prefix (`--api.port` → `DAVINCIFOLD_API_PORT`).
- `service/` — dependency-injection wiring between api, orchestrator, workers, storage.
- `orchestrator/` — the engine: genesis state, ingest/validate (`validate.go` checks Groth16 + ECDSA + census proof per ballot), seal, scatter/gather scheduler, fold driver, finalize.
- `workers/` — worker registry, health polling, ban/backoff, client pool. Pool starts empty; workers register at runtime via API.
- `storage/` — PebbleDB store, key layout, reservations/recovery, audit log.
- `api/` — chi router, typed `Error` model. JWT (HS256 over `--api.jwtSecret`) with `role` claim `admin` or `keywarden`; vote submission is self-authenticating and public.
- `keywarden/` — client for the publish-ciphertext / receive-key handshake.
- `log/`, `crypto/` — vendored helpers (log from davinci-node; keep parity rather than editing freely).

Proving primitives come from `github.com/vocdoni/davinci-zkvm/go-sdk` (notably its `chain` package).

## Conventions

- IDs (election, vote) are `0x`-prefixed hex; field elements are big-endian hex.
- `batchSize` and `foldEvery` have process-wide defaults but can be overridden per election in the create request.
- Every mutating API call is written to the Pebble audit namespace with subject/role/action.
