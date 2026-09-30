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
| `exp` | Expiry as a Unix timestamp. |

The README shows how to mint a token from a shell.

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

`foldEvery` reads `0` when the election uses the process default.

### `GET /elections`

`{"elections": [ ... ]}`, each entry shaped as above.

### `GET /elections/{id}`

One election. `status` is one of `active`, `ended`, `decrypting`, `finalizing` or `results`
(see [architecture.md](architecture.md#election-lifecycle)).

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

Returns `{"voteID": "<hex>", "status": "accepted"}`. A rejected vote returns error `40009`
with the reason in the message: failed verification (ballot proof, signature or census proof),
a repeated vote ID, an election that is not `active` or another vote from the same address
still being processed.

### `GET /elections/{id}/votes/{voteID}`

`{"voteID": "<hex>", "status": "pending", "seq": 12}`. `status` is `pending` until the vote is
sealed into a batch, then `batched`; `seq` is its position in the election's vote log.

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

Runs the final fold, decryption and PLONK proof, verifies the result and moves the election
to `results`. The call returns when finalize is done, which takes minutes, with the same body
as `GET /results`. If the client disconnects earlier, finalize still completes. On failure the
election goes back to `decrypting` and the key can be submitted again.

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

`address` is the base URL of a davinci-zkvm prover. Registering an existing address returns it
unchanged. Returns the prover as listed below.

### `GET /workers`

```json
{"workers": [{"address": "http://10.0.0.5:8080", "name": "gpu-0", "healthy": true,
  "queueLen": 0, "banned": false, "successCount": 42, "failedCount": 1}]}
```

## Errors

Errors carry an HTTP status and a JSON body such as
`{"error": "election not found: ...", "code": 40006}`.

| Code | HTTP | Meaning |
|---|---|---|
| 40002 | 400 | Malformed JSON body. |
| 40003 | 400 | Malformed parameter (bad hex, invalid election configuration). |
| 40004 | 403 | Missing token or role not allowed on this route. |
| 40005 | 401 | Invalid or expired token. |
| 40006 | 404 | Election not found. |
| 40007 | 409 | Election already exists. |
| 40009 | 400 | Vote rejected. |
| 40013 | 404 | Vote not found. |
| 40014 | 409 | Results not ready, or finalize failed. |
| 40016 | 400 | Malformed prover registration. |
| 50001 | 500 | Response encoding failed. |
| 50002 | 500 | Internal error. |

Codes are never reused; codes missing from this table are reserved.
