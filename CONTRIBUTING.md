# Contributing

Issues and pull requests are welcome at
[github.com/vocdoni/davinci-fold](https://github.com/vocdoni/davinci-fold). For anything
larger than a small fix, open an issue first so the approach can be agreed on.

## Build and check

You need the Go version in `go.mod` (1.25.5 or newer) and
[golangci-lint](https://golangci-lint.run) v2.

```sh
go build -o . ./cmd/...   # davinci-fold and test-keywarden
go vet ./...
go test ./...
golangci-lint run         # includes the gofumpt formatter
```

CI also fails when any of these leaves a diff, so run them before pushing:

```sh
go mod tidy
go fix ./...
go generate ./...
```

and reports unreachable code with
`go run golang.org/x/tools/cmd/deadcode@v0.41.0 -test ./...`.
[docs/testing.md](docs/testing.md) covers the integration suite.

## Code

The package layout is described in [docs/architecture.md](docs/architecture.md#code-layout).
A few rules keep the service correct across crashes and restarts:

- Persist before dispatching. Votes and batch prove requests are stored before any work is
  sent to a prover, and fold checkpoints and state snapshots as soon as they change, so every
  step can be driven again after a crash. New pipeline steps must keep dispatch idempotent.
- The state cannot be rebuilt by replaying votes, because re-encryption draws fresh randomness
  per batch. Anything that changes `chain.State` must persist a new snapshot.
- Provers are not trusted. Values learned from a prover are checked at finalize against local
  state and the pinned circuit release; keep new prover outputs inside those checks.
- Every mutating admin or keywarden call writes an audit record with its subject and role.
- The API follows davinci-node: chi router, typed `api.Error` values and error codes that are
  never changed or reused. Add new codes after the last one.
- `log` and `crypto` are copied from davinci-node. Keep them in step with upstream rather than
  changing them here.

Proving types and the prover client come from the davinci-zkvm Go SDK. Its version pins the
circuit release davinci-fold accepts, so bumping it means the provers must run the matching
davinci-zkvm release.

Comments explain why, not what. Exported identifiers get Go doc comments.

## Commits

Commit messages follow [Conventional Commits](https://www.conventionalcommits.org):
`feat(orchestrator): ...`, `fix(api): ...`, `docs: ...`, with a lowercase summary.

## License

By contributing you agree that your contributions are licensed under the
[GNU Affero General Public License v3.0](LICENSE).
