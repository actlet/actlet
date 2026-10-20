# Actlet

Actlet is a small agent runtime, written in Go, that runs inside an isolated container or sandbox. Each container runs one session, which an orchestrator controls. A session can fan out into concurrent subworkers. Actlet connects out to the orchestrator for its execution profile, models and remote tools, and does the actual work locally. It keeps a local journal so that a paused, stopped or crashed session can pick up where it left off. It also publishes a structured stream of what it is doing, so people can watch a session live or replay it later. A debugging TUI can attach inside the container, but normal runs are fully headless.

Actlet is deliberately not a web app, a model-provider gateway, a sandbox manager or an orchestration platform. Those concerns live outside it.

## Status

Early development. Nothing is usable yet: the repository currently holds only the build, test and CI scaffolding and a stub `actlet` binary.

## Building and testing

Requirements: Go 1.27 or newer and GNU Make. `make setup` installs the pinned version of `golangci-lint` used by `make check`.

```sh
make setup   # download modules, install golangci-lint
make build   # build bin/actlet
make test    # run the tests with the race detector
make check   # gofmt, go vet, golangci-lint, go.mod tidiness
make help    # list every target
```

```sh
$ bin/actlet version
$ bin/actlet help
```
