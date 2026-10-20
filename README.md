# Actlet

Actlet is a small agent runtime, written in Go, that runs inside an isolated container or sandbox. Each container runs one session, which an orchestrator controls. A session can fan out into concurrent subworkers. Actlet connects out to the orchestrator for its execution profile, models and remote tools, and does the actual work locally. It keeps a local journal so that a paused, stopped or crashed session can pick up where it left off. It also publishes a structured stream of what it is doing, so people can watch a session live or replay it later. A debugging TUI can attach inside the container, but normal runs are fully headless.

Actlet is deliberately not a web app, a model-provider gateway, a sandbox manager or an orchestration platform. Those concerns live outside it.

## Status

Early development. Nothing is usable yet. So far the repository holds the build, test and CI scaffolding and an experimental session journal: the crash-recoverable file Actlet will use to resume sessions ([format and guarantees](docs/journal-format.md)). `actlet journal inspect` and `actlet journal export --jsonl` read journals.

## Building and testing

Requirements: Go 1.27 or newer and GNU Make. `make setup` installs the pinned versions of `golangci-lint`, used by `make check`, and `protoc-gen-go`, used by `make generate` (which also needs `protoc`).

```sh
make setup   # download modules, install development tools
make build   # build bin/actlet
make test    # run the tests with the race detector
make check   # gofmt, go vet, golangci-lint, go.mod tidiness
make bench   # run the benchmarks
make help    # list every target
```

```sh
$ bin/actlet version
$ bin/actlet help
```
