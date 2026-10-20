# Actlet

Actlet is a small agent runtime, written in Go, that runs inside an isolated container or sandbox. Each container runs one session, which an orchestrator controls. A session can fan out into concurrent subworkers. Actlet connects out to the orchestrator for its execution profile, models and remote tools, and does the actual work locally. It keeps a local journal so that a paused, stopped or crashed session can pick up where it left off. It also publishes a structured stream of what it is doing, so people can watch a session live or replay it later. A debugging TUI can attach inside the container, but normal runs are fully headless.

Actlet is deliberately not a web app, a model-provider gateway, a sandbox manager or an orchestration platform. Those concerns live outside it.

## Status

Early development. What works today:

- `actlet run` connects to a gateway, registers its session, and runs its task. It takes pause, stop and steer commands, streams activity, and recovers from its journal. See the [gateway protocol](docs/gateway-protocol.md).
- [The agent](docs/agent.md) runs `agent` tasks: it calls the model through the gateway and works with local tools (files, search, shell, Python), runs tool calls and subagents in parallel, and takes operators' steer notes. It journals every call, so that recovery never repeats a side effect. A built-in `demo` task needs no model.
- `actlet-devgw` is a development gateway to run it against, built on the orchestrator SDK in [`sdk/`](sdk). It can answer model calls from a script.
- A terminal viewer [shows a session](docs/tui.md) as it runs (`actlet attach`, or `actlet run -tui`) or from a recording (`actlet-cli replay play`).
- `actlet-cli` holds the offline tools for journals and recordings; it never runs a session.
- The session journal is the crash-recoverable record each session keeps ([format and guarantees](docs/journal-format.md)). `actlet-cli journal inspect|export` reads journals and `actlet-cli replay inspect|export` reads activity recordings.

All protocols and formats are experimental (v0).

## Running a session

```sh
make build
bin/actlet-devgw -token dev &
ACTLET_GATEWAY=127.0.0.1:7400 ACTLET_TOKEN=dev bin/actlet run -session ./demo-session
```

This runs the demo task; to run the agent with a scripted model, see [trying the agent](docs/agent.md#trying-it). Type `pause drain`, `stop kill` or `steer <note>` into `actlet-devgw` to control the session. Give it a [profile](docs/profiles.md) with `-profile <file>`. `actlet run` exits with 0 when the task completes, 3 when it pauses and 4 when it is stopped. Run it again on the same folder to resume a paused session. `actlet run -h` lists every flag and its environment variable.

A session lives in one folder:

```
task.json     the task definition and input (written from the gateway's task if missing)
inputs/       input files
workspace/    the working directory
output/       deliverables
.actlet/      the journal, a status marker and the activity spill
```

## Try the TUI

The demo runs a scripted session of about two minutes, with no real model: an agent hands three regions of a sales review to subagents, runs shell commands and Python (one script fails and is fixed), edits a report, and gets an operator's note half-way. Every part of the viewer has something to show.

```sh
make demo-tui      # run the demo with the viewer in the foreground
```

In a second terminal, while it runs:

```sh
make demo-attach   # attach another viewer to the same box
```

Select a tool call with `↑` `↓` (or click it) and press `enter` to expand it; switch tabs with `tab` or a click; press `i` to send the agent a note, `?` for the keys and `q` to quit. Quitting detaches: in `make demo-tui` the session carries on until it ends or you press Ctrl-C, which pauses it. `make demo-attach` on its own starts a demo in the background and stops it when you quit. Replay the demo afterwards with the command it prints. See [watching a session](docs/tui.md).

## Building and testing

Requirements: Go 1.27 or newer and GNU Make. `make setup` installs the pinned versions of `golangci-lint`, used by `make check`, and of `protoc-gen-go` and `protoc-gen-go-grpc`, used by `make generate` (which also needs `protoc`).

```sh
make setup   # download modules, install development tools
make build   # build bin/actlet, bin/actlet-cli and bin/actlet-devgw
make test    # run the tests with the race detector, and a short fuzz run
make test-docker  # recovery across container replacement (needs Docker)
make test-k8s     # the gateway path through Kubernetes, in a throwaway kind cluster
make check   # gofmt, go vet, golangci-lint, go.mod tidiness
make bench   # run the benchmarks
make help    # list every target
```

Binaries are static and CGO-free and run natively on Linux, macOS and Windows, on amd64 and arm64.

```sh
make dist         # cross-build actlet, actlet-cli and actlet-devgw for all six platforms into dist/
make image        # build the actlet:dev container image (distroless, non-root)
make image-devgw  # build the actlet-devgw:dev development gateway image
```

For Kubernetes, see [running on Kubernetes](docs/kubernetes.md): what was tested and the recommended settings.

The image runs as an unprivileged user with its working directory at `/var/lib/actlet`. To keep sessions across container replacement, mount a volume there. The container's own filesystem only survives a restart of the same container.

```sh
$ bin/actlet version
$ bin/actlet help
```
