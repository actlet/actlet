# Actlet

Actlet is a small agent runtime, written in Go, that runs inside an isolated container or sandbox. Each container runs one session, which an orchestrator controls. A session can fan out into concurrent subworkers. Actlet connects out to the orchestrator for its execution profile, models and remote tools, and does the actual work locally. It keeps a local journal so that a paused, stopped or crashed session can pick up where it left off. It also publishes a structured stream of what it is doing, so people can watch a session live or replay it later. A debugging TUI can attach inside the container, but normal runs are fully headless.

Actlet is deliberately not a web app, a model-provider gateway, a sandbox manager or an orchestration platform. Those concerns live outside it.

## Status

Early development: there is no real agent yet. What works today:

- `actlet run` connects to a gateway, registers its session, and runs a built-in demo task in place of agent work. It takes pause, stop and steer commands, streams activity, and recovers from its journal. See the [gateway protocol](docs/gateway-protocol.md).
- `actlet-devgw` is a development gateway to run it against, built on the orchestrator SDK in [`sdk/`](sdk).
- `actlet-cli` holds the offline tools for journals and recordings; it never runs a session.
- The session journal is the crash-recoverable record each session keeps ([format and guarantees](docs/journal-format.md)). `actlet-cli journal inspect|export` reads journals and `actlet-cli replay inspect|export` reads activity recordings.

All protocols and formats are experimental (v0).

## Running a session

```sh
make build
bin/actlet-devgw -token dev &
ACTLET_GATEWAY=127.0.0.1:7400 ACTLET_TOKEN=dev bin/actlet run -session ./demo-session
```

Type `pause drain`, `stop kill` or `steer <note>` into `actlet-devgw` to control the session. `actlet run` exits with 0 when the task completes, 3 when it pauses and 4 when it is stopped. Run it again on the same folder to resume a paused session. `actlet run -h` lists every flag and its environment variable.

A session lives in one folder:

```
task.json     the task definition and input (written from the gateway's task if missing)
inputs/       input files
workspace/    the working directory
output/       deliverables
.actlet/      the journal, a status marker and the activity spill
```

## Building and testing

Requirements: Go 1.27 or newer and GNU Make. `make setup` installs the pinned versions of `golangci-lint`, used by `make check`, and of `protoc-gen-go` and `protoc-gen-go-grpc`, used by `make generate` (which also needs `protoc`).

```sh
make setup   # download modules, install development tools
make build   # build bin/actlet, bin/actlet-cli and bin/actlet-devgw
make test    # run the tests with the race detector, and a short fuzz run
make test-docker  # recovery across container replacement (needs Docker)
make check   # gofmt, go vet, golangci-lint, go.mod tidiness
make bench   # run the benchmarks
make help    # list every target
```

Binaries are static and CGO-free, so they run on any Linux base image, or none.

```sh
make dist    # cross-build dist/actlet_linux_amd64, dist/actlet_linux_arm64, ...
make image   # build the actlet:dev container image (distroless, non-root)
```

The image runs as an unprivileged user with its working directory at `/var/lib/actlet`. To keep sessions across container replacement, mount a volume there. The container's own filesystem only survives a restart of the same container.

```sh
$ bin/actlet version
$ bin/actlet help
```
