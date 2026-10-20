# AGENTS.md

Rules for AI coding agents working in this repository.

## Project

Actlet is a lightweight Go agent runtime for isolated containers and sandboxes. See [README.md](README.md) for what it is and its current status.

## Commands

```sh
make build   # build bin/actlet
make test    # go test -race ./...
make check   # gofmt, go vet, golangci-lint, go mod tidy diff
make help    # all targets
```

Leave `make check` and `make test` green after every change.

## Go conventions

- Format with `gofmt`/`goimports`. Follow [Effective Go](https://go.dev/doc/effective_go) and the [Go Code Review Comments](https://go.dev/wiki/CodeReviewComments).
- Prefer the standard library. Add a dependency only when it clearly earns its place, and say why in the commit.
- Keep packages small and focused. Don't add placeholder packages or interfaces for designs that haven't been settled.
- Wrap errors with context (`fmt.Errorf("...: %w", err)`) and compare with `errors.Is`/`errors.As`.
- Bound every queue, buffer and read of untrusted input.
- Write table-driven tests next to the code they test. Keep tests fast and deterministic.
- Binaries live under `cmd/<name>/`. Keep `main` thin and testable through a `run` function.

## Git

- Work on `main`. Don't create branches unless asked.
- Use [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/) (`feat:`, `fix:`, `build:`, `ci:`, `docs:`, `test:`, `refactor:`, `chore:`).
- Commit at sensible granularity: one commit per coherent change, not one giant commit and not one per tiny edit.
- Never push. The maintainer pushes.

## Confidentiality

Some project names are confidential and have aliases. Write only the alias in code, comments, docs and commit messages, never the real name.
