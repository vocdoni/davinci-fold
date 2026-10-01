# davinci-fold

davinci-fold runs the chained proving mode of the [DAVINCI](https://davinci.vote) voting
protocol: it collects the ballots of an election, proves them in batches on a pool of
[davinci-zkvm](https://github.com/vocdoni/davinci-zkvm) GPU provers, folds the batch proofs
into one recursive chain and ends each election with a single PLONK proof of the whole tally.
It is operated by whoever runs the proving infrastructure for a set of elections.

[![Build and Test](https://github.com/vocdoni/davinci-fold/actions/workflows/main.yml/badge.svg)](https://github.com/vocdoni/davinci-fold/actions/workflows/main.yml)
[![License: AGPL-3.0](https://img.shields.io/badge/License-AGPL%203.0-blue.svg)](LICENSE)

## Overview

davinci-fold is a single Go service with an HTTP API. For every election it checks each
incoming ballot (Groth16 ballot proof, ECDSA signature, census membership), applies ballots to
the election state in batches, and sends each batch as a STARK job to an idle prover.
Batch proofs do not depend on each other, so an election's batches are proved on several
provers at once, one job per prover at a time. The proofs are then gathered in order on one
prover per election, the fold worker, which folds them into a recursive chain every few
batches.

```
ballots ─▶ verify ─▶ seal batch ─▶ STARK on any prover ─▶ import on fold worker ─▶ fold
                                                                                    │
end time ─▶ publish encrypted tally ─▶ keywarden returns key ─▶ finalize ─▶ tally + PLONK
```

When an election reaches its end time, davinci-fold folds the remaining batches and publishes
the encrypted tally. The key holder (the keywarden) fetches it and returns the decryption key,
which triggers the finalize step: davinci-fold decrypts the tally and proves each decryption,
and the fold worker checks those proofs in-circuit and wraps the whole chain into one PLONK
proof. davinci-fold checks that proof against its own state before serving the tally together
with the four values `ZiskVerifier.verifySnarkProof` takes on-chain.

Votes, sealed batches, fold checkpoints, state snapshots, the proofs a fold chain needs and
the prover registrations are stored in PebbleDB; after a restart the open elections resume
where they were, and a fold chain moves to another prover when its own one fails. The per-batch model, where every
batch is settled on Ethereum with its own proof, is implemented by
[davinci-node](https://github.com/vocdoni/davinci-node); davinci-fold follows its conventions
for configuration, storage and API.

## Quick start

You need Docker with Compose and at least one davinci-zkvm prover reachable from this host,
with the aggregator guest configured for chained mode (see the
[davinci-zkvm README](https://github.com/vocdoni/davinci-zkvm)).

```sh
git clone https://github.com/vocdoni/davinci-fold.git
cd davinci-fold
cp .env.example .env
# Set DAVINCIFOLD_API_JWTSECRET in .env to a long random string, e.g. `openssl rand -hex 32`.
docker compose --profile prod up -d
curl http://127.0.0.1:8888/info
```

The `prod` profile runs the orchestrator plus Watchtower for automatic image updates; use
`--profile dev` for the orchestrator alone. Data lives in the `run` volume.

To run from source instead (Go 1.25.5 or newer):

```sh
go build -o . ./cmd/...
./davinci-fold --api.jwtSecret="$(openssl rand -hex 32)"
```

## Usage

### Configuration

Every flag has an environment variable: prefix `DAVINCIFOLD_`, uppercase, dots replaced by
underscores (`--api.port` becomes `DAVINCIFOLD_API_PORT`). [.env.example](.env.example) lists
them all.

| Flag | Env | Default | Description |
|---|---|---|---|
| `--api.jwtSecret` | `DAVINCIFOLD_API_JWTSECRET` | | HMAC secret for admin and keywarden tokens. Required. |
| `--api.host`, `-h` | `DAVINCIFOLD_API_HOST` | `0.0.0.0` | API bind address. |
| `--api.port`, `-p` | `DAVINCIFOLD_API_PORT` | `8888` | API port. |
| `--batch.size` | `DAVINCIFOLD_BATCH_SIZE` | `64` | Seal a batch at this many votes (2 to 1024). |
| `--batch.time`, `-b` | `DAVINCIFOLD_BATCH_TIME` | `5m` | Seal a partial batch once its oldest vote is this old. |
| `--fold.every` | `DAVINCIFOLD_FOLD_EVERY` | `4` | Fold after this many batch proofs. |
| `--worker.pollPeriod` | `DAVINCIFOLD_WORKER_POLLPERIOD` | `10s` | Prover health-check interval. |
| `--worker.jobTimeout` | `DAVINCIFOLD_WORKER_JOBTIMEOUT` | `30m` | Time limit of each prove, fold and finalize job. |
| `--log.level`, `-l` | `DAVINCIFOLD_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` or `fatal`. |
| `--log.output`, `-o` | `DAVINCIFOLD_LOG_OUTPUT` | `stdout` | `stdout`, `stderr` or a file path. |
| `--log.disableAPI` | `DAVINCIFOLD_LOG_DISABLEAPI` | `false` | Do not log API requests and responses. |
| `--datadir`, `-d` | `DAVINCIFOLD_DATADIR` | `~/.davinci-fold` | Database directory. |

An election can override the batch size and fold cadence when it is created. At `debug` level
the API logs request and response bodies, except for the decryption-key call, and never the
`Authorization` header.

### Authentication

Voting and the read endpoints are public, except the encrypted tally. Creating elections and
registering provers needs an `admin` token; fetching the encrypted tally and submitting the
decryption key needs a `keywarden` token. Tokens are HS256 JWTs signed with the JWT secret,
carrying `role`, `sub` (recorded in the audit log) and `exp`, which is required. Any JWT
library can mint them; from a shell:

```sh
b64url() { openssl base64 -A | tr '+/' '-_' | tr -d '='; }
mint() { # mint <role> <subject>
  h=$(printf '{"alg":"HS256","typ":"JWT"}' | b64url)
  p=$(printf '{"role":"%s","sub":"%s","exp":%d}' "$1" "$2" $(($(date +%s) + 3600)) | b64url)
  s=$(printf '%s.%s' "$h" "$p" | openssl dgst -sha256 -hmac "$DAVINCIFOLD_API_JWTSECRET" -binary | b64url)
  echo "$h.$p.$s"
}
ADMIN_JWT=$(mint admin ops)
KEYWARDEN_JWT=$(mint keywarden keywarden-1)
```

### Running an election

1. Register each prover. Registrations are stored, so they survive a restart;
   `DELETE /workers/{id}` removes one.

   ```sh
   curl -X POST http://127.0.0.1:8888/workers/register \
     -H "Authorization: Bearer $ADMIN_JWT" -H 'Content-Type: application/json' \
     -d '{"address":"http://10.0.0.5:8080","name":"gpu-0"}'
   ```

2. Generate the election key pair. `test-keywarden` keeps the private key in the key file and
   prints the public key; the orchestrator only receives the private key after voting ends.

   ```sh
   ./test-keywarden --mode=keygen --keyfile=keywarden-key.json
   ```

3. Create the election. `vk` is the ballot-proof verification key that ingest and the provers
   check ballots against; without it the election uses the davinci-circom `v1.0.0` key. The
   census must be a lean-IMT Merkle census (`censusOrigin` 1 to 3). The other fields are
   described in [docs/api.md](docs/api.md#create-an-election).

   ```sh
   curl -LO https://raw.githubusercontent.com/vocdoni/davinci-circom/v1.0.0/artifacts/ballot_proof_vkey.json
   jq -n --arg encX "$(jq -r .encX keywarden-key.json)" --arg encY "$(jq -r .encY keywarden-key.json)" \
     --slurpfile vk ballot_proof_vkey.json \
     '{processID: "0x...", ballotMode: "0x...", encX: $encX, encY: $encY, censusOrigin: 1,
       censusRoot: "0x...", vk: $vk[0], endTime: "2026-12-01T18:00:00Z"}' > election.json
   curl -X POST http://127.0.0.1:8888/elections \
     -H "Authorization: Bearer $ADMIN_JWT" -H 'Content-Type: application/json' -d @election.json
   ```

   The election ID is the process ID in hex. Use it as `$ELECTION` below.

4. Voters submit ballots to `POST /elections/$ELECTION/votes` (no token; the ballot carries its
   own proofs). The body format is in [docs/api.md](docs/api.md#submit-a-vote).

5. At `endTime` the election moves to `ended`, the remaining batches are folded and the status
   becomes `decrypting`. To end it earlier, or if it has no `endTime`, ask for it; the same call
   takes `paused`, `active` and `canceled`:

   ```sh
   curl -X POST http://127.0.0.1:8888/elections/$ELECTION/status \
     -H "Authorization: Bearer $ADMIN_JWT" -H 'Content-Type: application/json' -d '{"status":"ended"}'
   ```

6. Release the key. davinci-fold checks it and finalizes in the background, which takes several
   minutes on the fold worker. `test-keywarden` submits the key, follows the election until it
   has results (at most `--timeout`, 30 minutes by default) and prints them.

   ```sh
   ./test-keywarden --mode=finalize --keyfile=keywarden-key.json \
     --token="$KEYWARDEN_JWT" --election="$ELECTION"
   ```

7. Fetch the tally and the PLONK proof:

   ```sh
   curl http://127.0.0.1:8888/elections/$ELECTION/results
   ```

### API

| Method | Path | Auth | Purpose |
|---|---|---|---|
| GET | `/ping`, `/info` | | Liveness; version, defaults and counts. |
| POST, GET | `/elections` | admin (POST) | Create or list elections. |
| GET | `/elections/{id}` | | Election status. |
| POST | `/elections/{id}/status` | admin | Pause, resume, end or cancel an election. |
| POST | `/elections/{id}/votes` | | Submit a ballot. |
| GET | `/elections/{id}/votes/{voteID}` | | Vote status. |
| GET | `/elections/{id}/encrypted-results` | keywarden | Encrypted tally, once `decrypting`. |
| POST | `/elections/{id}/decryption-key` | keywarden | Submit the key; finalize runs in the background. |
| GET | `/elections/{id}/results` | | Tally and PLONK proof. |
| GET | `/workers` | | Prover pool. |
| POST | `/workers/register` | admin | Add a prover. |
| DELETE | `/workers/{id}` | admin | Remove a prover. |

Request and response formats and error codes are in [docs/api.md](docs/api.md).

## Documentation

- [docs/architecture.md](docs/architecture.md): proving pipeline, election lifecycle, prover
  scheduling, persistence and the checks run at finalize.
- [docs/api.md](docs/api.md): HTTP API reference.
- [docs/testing.md](docs/testing.md): unit and integration tests, including the end-to-end run
  against real provers.
- [CONTRIBUTING.md](CONTRIBUTING.md): build, lint and code conventions.

## Development

```sh
go build ./...
go test ./...        # unit tests, no provers needed
golangci-lint run
```

The integration suite needs `RUN_INTEGRATION_TESTS=1` and, for the proving tests, running
davinci-zkvm provers; see [docs/testing.md](docs/testing.md).

## License

davinci-fold is licensed under the [GNU Affero General Public License v3.0](LICENSE).
