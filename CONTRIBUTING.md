# Contributing

## Setup

Install Go 1.27 or newer and GNU Make, then run:

```sh
make setup
```

## Before you commit

```sh
make check   # gofmt, go vet, golangci-lint, go mod tidy diff
make test    # tests with the race detector
make build   # bin/actlet
```

CI runs the same three targets on every push and pull request to `main`.

## Commit messages

Use [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/):

```
<type>[optional scope]: <description>

[optional body]
```

Common types: `feat`, `fix`, `build`, `ci`, `docs`, `test`, `refactor`, `chore`. Keep the header short and in the imperative mood, and use the body to explain why. One commit per coherent change.
