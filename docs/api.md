# HTTP API

The API listens on `--api.host:--api.port` (default `0.0.0.0:8888`) and speaks JSON. Election
and vote IDs in paths are hex, with or without a `0x` prefix. Request bodies are limited to
1 MiB.

## Authentication

Routes marked `admin` or `keywarden` need `Authorization: Bearer <token>`, where the token is
an HS256 JWT signed with `--api.jwtSecret` and carrying these claims:

| Claim | Value |
|---|---|
| `role` | `admin` or `keywarden` |
| `sub` | Caller name, recorded in the audit log. |
| `exp` | Expiry as a Unix timestamp. Required: a token without it is rejected. |

A missing or malformed header, a bad signature, an expired token or one without `exp` gets
`401` (`40005`); a valid token whose role the route does not allow gets `403` (`40004`). The
README shows how to mint a token from a shell.

## Service

### `GET /ping`

Returns `200` with an empty body while the service is up.

### `GET /info`

```json
{"version": "v0.1.0", "batchSize": 64, "foldEvery": 4, "workers": 2, "elections": 5}
```

`batchSize` and `foldEvery` are the process defaults.

## Elections

### Create an election

`POST /elections` (admin)

| Field | Type | Description |
|---|---|---|
| `processID` | hex | Protocol process ID, at most 31 bytes. The election ID is its bytes. |
| `ballotMode` | hex | Packed ballot mode (`spec.BallotMode.Pack()` in the davinci-zkvm Go SDK); its low byte is the number of fields, 1 to 16. |
| `encX`, `encY` | hex | ElGamal public key, reduced twisted Edwards coordinates. |
| `censusOrigin` | number | Census origin: `1`, `2` or `3`, a lean-IMT Merkle census. Other origins, `4` (CSP) included, are rejected. |
| `censusRoot` | hex | Census root every vote's census proof must reach, a BN254 field element. |
| `vk` | object | Optional. Ballot-proof verification key (snarkjs JSON, three public signals) that ingest and the provers check ballot proofs against. Defaults to the davinci-circom `v1.0.0` key. Stored re-encoded in canonical snarkjs form. |
| `endTime` | RFC 3339 | When the election closes. Without it the election never ends. |
| `batchSize` | number | Optional. Overrides `--batch.size`, at most 1024. |
| `foldEvery` | number | Optional. Overrides `--fold.every`. |

Hex values are big-endian with an optional `0x` prefix.

Returns the election:

```json
{"id": "a1b2c3", "status": "active", "batchSize": 64, "foldEvery": 4,
 "endTime": "2026-12-01T18:00:00Z", "createdAt": "2026-11-30T09:00:00Z"}
```

`batchSize` and `foldEvery` are the values the election uses, the process defaults when the
request did not set them. Once its first batch is gathered, the election also shows
`foldWorker`, the address of the prover holding its fold chain; it changes when the chain moves
to another prover (see [architecture.md](architecture.md#provers)).

### `GET /elections`

`{"elections": [ ... ]}`, each entry shaped as above.

### `GET /elections/{id}`

One election. `status` is one of `active`, `paused`, `ended`, `decrypting`, `finalizing`,
`results` or `canceled` (see [architecture.md](architecture.md#election-lifecycle)). After a
failed finalize the election is `decrypting` again and `finalizeError` says why, in one of
`prover unavailable`, `finalize interrupted`, `nothing to finalize`, `fold failed`,
`decryption failed`, `final proof failed`, `final proof verification failed` or
`internal error`. The details are in the service log.

### Change an election's status

`POST /elections/{id}/status` (admin)

```json
{"status": "ended"}
```

| From | To |
|---|---|
| `active` | `paused`, `ended` or `canceled` |
| `paused` | `active`, `ended` or `canceled` |

`paused` stops taking votes; the accepted ones are still sealed and proved. `ended` does what
the end time does: the pending votes are sealed and the election then waits for the decryption
key. `canceled` stops all work for the election: nothing more is sealed, proved or folded, and
its votes read `error`; the election stays readable. Any other change returns `409` (`40017`),
another status name `400` (`40003`). Returns the election.

## Votes

### Submit a vote

`POST /elections/{id}/votes`

No token: the ballot authenticates itself. Byte fields are base64, as Go encodes `[]byte`.

| Field | Type | Description |
|---|---|---|
| `vote_id` | base64 | Vote identifier: `vote_id_key` as 8 big-endian bytes. |
| `vote_id_key` | number | The vote ID as an unsigned 64-bit integer, bit 63 set. |
| `address` | base64 | Voter Ethereum address, 20 bytes. |
| `ballot` | base64 | Encrypted ballot, `elgamal.Ballot.Serialize()` from the davinci-zkvm Go SDK. |
| `proof` | object | snarkjs Groth16 proof of the ballot: decimal coordinates, affine points (`"1"`). |
| `public_inputs` | string[3] | Decimal public signals: address, vote ID, inputs hash. |
| `sig` | object | `{"signature_r": hex, "signature_s": hex, "signature_v": recovery id}` |
| `census` | object | `{"root": hex, "leaf": hex, "index": number, "siblings": [hex]}` |

Ingest checks everything the batch circuit checks about a vote, so an accepted vote cannot
make its batch fail. The inputs hash must be the Poseidon hash the ballot proof commits to,
recomputed from the election (process ID, ballot mode, key), `address`, the vote ID, the
submitted ballot and the weight the census leaf carries. Ballot fields past the ballot mode's
field count must be the identity ciphertext, the others points on BabyJubJub. The proof,
public inputs and signature are stored re-encoded as the provers parse them.

`sig` is an Ethereum `personal_sign` (EIP-191) signature by `address` over the vote ID
left-padded to 32 bytes; `r` and `s` are hex, `s` at most n/2, and the recovery id is 0 or 1
(27 or 28 are accepted). `census` is a lean-IMT membership proof that must reach the
election's `censusRoot`: `root`, `leaf` and `siblings` are 32-byte big-endian hex field
elements, `leaf` is `address << 88 | weight` for the submitted `address`, and `index` holds the
path bits, with no bits set above the proof depth, which is at most 61. The voter's ballot slot
is derived from the address, so a later vote from the same address overwrites the earlier one.

Returns `{"voteID": "<hex>", "status": "accepted"}`. A rejected vote gets the error of its
reason, with the detail in the message:

| Code | HTTP | Reason |
|---|---|---|
| 40006 | 404 | No such election. |
| 40008 | 400 | The election is not `active`. |
| 40009 | 400 | The ballot proof or its public inputs do not verify or do not match the vote. |
| 40010 | 400 | The signature is malformed or not by `address`. |
| 40011 | 400 | The census proof is malformed, does not reach `censusRoot` or is not for `address`. |
| 40012 | 409 | The vote ID was already accepted, or a vote with this vote ID or address is being processed. |
| 40002 | 400 | Anything else about the body: JSON, `address`, `vote_id` or `ballot` encoding. |

### `GET /elections/{id}/votes/{voteID}`

`{"voteID": "<hex>", "status": "pending", "seq": 12}`. `seq` is the vote's position in the
election's vote log. `status` moves forward only:

| Status | Meaning |
|---|---|
| `pending` | Accepted, waiting to be sealed into a batch. |
| `batched` | Sealed into a batch, which is being proved. |
| `folded` | Its batch proof is folded into the election's chain. |
| `settled` | The election's final proof is verified and its results stored. |
| `error` | The election state refused the vote when it was sealed, or the election was canceled. |

## Results

### `GET /elections/{id}/encrypted-results` (keywarden)

Available once the election is `decrypting`:

```json
{"election_id": "a1b2c3", "ciphertext": ["<hex>", "..."]}
```

`ciphertext` holds one ElGamal ciphertext per ballot field (16 fields), each as four
32-byte little-endian twisted Edwards coordinates (C1.x, C1.y, C2.x, C2.y), 64 strings in all.

### `POST /elections/{id}/decryption-key` (keywarden)

```json
{"key": "0x<private scalar, big-endian hex>"}
```

The key must be the private key of the election's `encX`, `encY`: a malformed key gets `400`
(`40003`), another key `400` (`40018`), and the election is unchanged. A valid key moves the
election to `finalizing` and the call returns `202` with the election. The final fold,
decryption and PLONK proof then run in the background, which takes minutes; follow them with
`GET /elections/{id}`. The election ends at `results`, or goes back to `decrypting` with
`finalizeError` set, when the key can be submitted again. A key submitted while the election
is not `decrypting`, `finalizing` included, gets `409` (`40017`). Neither the request nor the
response is logged.

### `GET /elections/{id}/results`

```json
{
  "electionID": "a1b2c3",
  "tally": [3, 0, 7, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0],
  "programVK": "0x...",
  "rootCVadcopFinal": "0x...",
  "publicValues": "0x...",
  "proofBytes": "0x..."
}
```

`tally` has one sum per ballot field. The last four fields are the arguments of
`ZiskVerifier.verifySnarkProof` in the davinci-zkvm Solidity verifier.

## Provers

### `POST /workers/register` (admin)

```json
{"address": "http://10.0.0.5:8080", "name": "gpu-0"}
```

`address` is the base URL of a davinci-zkvm prover, `http` or `https` (`400`, `40016`
otherwise). Registering an existing address returns it unchanged, with `name` filled in if it
had none. The registration is stored, so the prover is back in the pool after a restart.
Returns the prover as listed below.

### `GET /workers`

```json
{"workers": [{"id": "5f1c0e2a9b3d4c7e", "address": "http://10.0.0.5:8080", "name": "gpu-0",
  "healthy": true, "queueLen": 0, "banned": false, "successCount": 42, "failedCount": 1}]}
```

`id` is derived from `address`, so it stays the same when the prover is registered again.

### `DELETE /workers/{id}` (admin)

Removes a prover from the pool and deletes its registration. Its running jobs count as failed
and go to other provers, as when a prover dies, and the fold chain of every open election
pinned to it moves to another prover at once. Returns `200`, or `404` (`40015`) for an unknown
`id`.

## Errors

Errors carry an HTTP status and a JSON body such as
`{"error": "election not found: ...", "code": 40006}`.

| Code | HTTP | Meaning |
|---|---|---|
| 40002 | 400 | Malformed JSON body, or a malformed vote. |
| 40003 | 400 | Malformed parameter (bad hex, invalid election configuration, unknown status). |
| 40004 | 403 | The token's role is not allowed on this route. |
| 40005 | 401 | Missing, invalid or expired token, or a token without `exp`. |
| 40006 | 404 | Election not found. |
| 40007 | 409 | Election already exists. |
| 40008 | 400 | Election not accepting votes. |
| 40009 | 400 | Invalid ballot proof. |
| 40010 | 400 | Invalid signature. |
| 40011 | 400 | Invalid census proof. |
| 40012 | 409 | Vote already submitted or being processed. |
| 40013 | 404 | Vote not found. |
| 40014 | 409 | Results not ready. |
| 40015 | 404 | Prover not found. |
| 40016 | 400 | Malformed prover registration. |
| 40017 | 409 | The election's status does not allow this change. |
| 40018 | 400 | The decryption key does not match the election's key. |
| 50001 | 500 | Response encoding failed. |
| 50002 | 500 | Internal error. |

Codes are never reused; codes missing from this table are reserved.
