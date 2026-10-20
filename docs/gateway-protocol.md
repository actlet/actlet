# Gateway protocol

Actlet always runs under an orchestrator. The orchestrator serves a **gateway**; Actlet connects out to it, registers its session, takes control commands and reports what it is doing. Actlet never listens for connections itself, so it works behind NAT and in locked-down containers.

This document describes protocol version **v0**, how Actlet behaves on top of it, and what a gateway must do. The contract is in [`proto/actlet/gateway/v0/gateway.proto`](../proto/actlet/gateway/v0/gateway.proto).

> **Status: experimental.** v0 may change incompatibly. The messages are built to grow: new commands, event kinds and profile fields can be added without breaking existing peers.

## Overview

The transport is gRPC over one address. A worker uses three calls:

| Call | Kind | Purpose |
|---|---|---|
| `Register` | unary | Attach the worker to a session; receive the profile and the task |
| `Control` | bidirectional stream | Commands to the worker; acknowledgements and status from it |
| `PublishActivity` | bidirectional stream | Activity events from the worker; acknowledgements from the gateway |

Control and activity are separate streams with separate loops on the worker. A gateway that consumes activity slowly delays only activity acknowledgements, never commands.

Every call carries `authorization: Bearer <token>`. TLS is optional (`-tls`, `-tls-ca`). Both sides use keepalives, so dead connections are noticed even when idle; `sdk.ServerOptions()` gives a gateway matching settings.

## Sessions, runs and epochs

A **session** is one task, carried out in one **session folder**:

```
task.json     the task: definition and input
inputs/       input files placed by the orchestrator
workspace/    the agent's working directory
output/       deliverables the orchestrator collects
.actlet/      the journal, a status marker, the activity spill
```

A session may take several **runs**: each start of `actlet run` on the folder is one. Runs are numbered by **epoch**, from 1. The journal in `.actlet/` is the authoritative record of the session. A new run recovers from it, so resuming after a pause and recovering after a crash are the same thing.

Keeping the folder is the deployer's job: to resume in a new container, mount the folder from storage that outlives the container. See [the journal format](journal-format.md#storage-and-deployment).

## Registration

The worker calls `Register` before anything else:

- `session_id`: empty for a new session, the session's ID when resuming.
- `epoch`: this run's number.
- `worker_id`, `capabilities`, `actlet_version`: for the gateway's information.

The gateway answers with:

- `session_id`: the session to use. A gateway must keep the requested ID when resuming. Actlet refuses to continue a folder under a different session.
- `profile`: the behaviour the orchestrator chooses. In v0 that is identity and version, limits, the default pause mode for OS signals, and the default stop grace period. Actlet clamps the profile to the ceilings set by its own deployment flags, and records the effective configuration in the journal. No credentials ever pass through the profile.
- `task`: the task. **The folder comes first:** if the folder already holds a `task.json`, that is used. Otherwise the gateway's task is written to `task.json`. A task has a schema version, an ID, a kind, and a JSON definition and input whose shape depends on the kind.

Registration is retried with backoff while the gateway is unreachable. Errors that retrying can't fix stop the run: bad credentials, an unsupported protocol version, an invalid request.

## Control

### Commands

The gateway sends `Command`s down the control stream. Each has:

- an **ID**, chosen by the gateway and unique within the session;
- an **origin** (orchestrator or local) and an **actor** (for example an operator name), both recorded;
- one **action**.

| Action | Effect |
|---|---|
| `pause` | Stop starting new work, persist, and **end the process**. `mode` is required: `drain` lets running work finish, `interrupt` signals it to stop now. A pause without a mode is rejected |
| `stop` | Cancel the task: signal running work, kill it after `grace` (default from the profile), or at once with `kill`. Persist and **end the process**. A stopped session doesn't resume |
| `steer` | Pass an operator note to the session. It is recorded and published as activity |

The worker writes every command to its journal, together with whether it accepted it, **before** acknowledging it. A `CommandAck` therefore means the command is durable on the worker's side. If the worker can't record a command, it doesn't acknowledge it.

Commands are idempotent by ID. A worker that receives an ID it has seen before, in this run or an earlier one, acknowledges it again with `duplicate: true` and doesn't apply it a second time. A gateway can therefore always resend a command it hasn't seen acknowledged. The SDK does this automatically after a reconnect.

A command with an action the worker doesn't know is rejected rather than ignored. A newer gateway talking to an older worker learns that the action wasn't applied.

### Status

The worker opens the stream with a `ControlHello` (session, epoch, state), reports its state on a heartbeat, and sends a final status as it exits. States are `running`, `pausing`, `stopping`, and the final `paused`, `stopped`, `completed` or `failed`.

## Ending a run

Pausing and stopping end the process. There is no paused process waiting for a resume command. Exit codes tell the orchestrator why:

| Exit code | Meaning | Next step |
|---:|---|---|
| 0 | the task completed | collect `output/` |
| 1 | the run failed | look at the logs; the session can be run again |
| 2 | usage error | fix the command line |
| 3 | paused | start `actlet run` on the folder again to resume |
| 4 | stopped | nothing; a new run exits 4 straight away |

The folder's `.actlet/status` file holds the same state as text, for tools that look at the folder rather than the process.

**SIGTERM** (for example `docker stop`) pauses in the profile's default mode, so a container that is stopped or moved can always be resumed. The pause must finish within `-signal-deadline` (default 8s). Keep the deadline inside the container runtime's stop timeout. When it runs out, running work is killed and the session is still marked paused. A second signal kills running work at once. On Windows, console close and shutdown events count as SIGTERM, and Ctrl-C counts as an interrupt.

## Activity

### Events and sequence numbers

Activity events are numbered per epoch: sequence numbers start at 1 in every run and have no gaps. An event's ID is `<epoch>.<seq>`, unique within the session. Because each run is a new epoch, a restarted worker never reuses an ID the gateway has already seen.

Event kinds in v0: session started, state change, command received, steer note, text output, and gap. Events are small: activity describes what happened, and larger content belongs in the session folder.

### Publishing and acknowledgement

The worker opens one `PublishActivity` stream per epoch with an `ActivityHello`. The gateway answers at once with an `ActivityAck` holding the highest sequence number up to which it already holds every event of that epoch. The worker sends from there, in order and in batches, and the gateway acknowledges as it stores them.

The gateway deduplicates by sequence number. It stores an event only if it directly follows what it holds, skips events it already has, and stops at a gap so the worker resends from the acknowledgement. An event is acknowledged only once it is stored, and the worker discards it only then.

The worker keeps at most a window of unacknowledged events in flight (1024). If events are outstanding and no acknowledgement arrives within 30s, it reconnects the activity stream.

### Buffering, spilling and gaps

Emitting activity never waits for the network or the disk. Events wait in a bounded in-memory queue (`-activity-queue`). When that is full, they spill to `.actlet/activity-<epoch>.actrec` in the session folder, and are read back from there once the gateway is reachable again. The spill file is deleted when everything in it has been acknowledged.

The spill file is capped (`-activity-spill-max`). Beyond the cap, display-only events, such as text output, are dropped. A **gap** event then reports how many were dropped and over what time span. State changes, commands and gap events keep a small reserve beyond the cap. Execution-critical state never depends on activity: it lives in the journal.

When a run ends, the worker waits briefly (`-flush-timeout`) for outstanding events to be acknowledged, and writes the rest to the spill file. The next run sends them as a **backlog** for the earlier epoch, alongside its own events.

## Reconnecting

- Every stream reconnects on its own, with exponential backoff and full jitter, between 200ms and 10s. The transport's own reconnect backoff is held to the same bound.
- If a gateway replies that it doesn't know the session, for example after a restart without state, the worker registers again with the same session ID and epoch, then reopens its streams.
- Work continues while the gateway is unreachable. Activity piles up in memory and the spill file, commands simply don't arrive, and nothing waits on the connection.

### What can be uncertain

- **A command applied but not acknowledged.** The worker recorded and applied it, but the acknowledgement was lost. The gateway resends it; the worker answers `duplicate: true`.
- **Events after the last acknowledgement.** The gateway may or may not hold them; the worker resends them and the gateway skips what it has.
- **A gateway that loses its own state.** A gateway that acknowledges events must keep them, because the worker forgets them. `sdk.RecordingStore` keeps recordings on disk and reads them back on restart, so a restarted gateway still deduplicates correctly.

## Implementing a gateway

The Go package [`sdk`](../sdk) implements the gateway side for embedding in an orchestrator:

- `sdk.NewServer(sdk.Options{...})` with an `Assign` function that decides each session's profile and task, an optional `Authenticate`, and an `ActivityStore`;
- `Session.Pause`, `Stop`, `Steer` and `Send` to issue commands and wait for their acknowledgements, with queuing and resending handled;
- `sdk.MemoryStore` and `sdk.RecordingStore` (one `.actrec` file per session, readable with `actlet-cli replay inspect` and `actlet-cli replay export`).

Gateways in other languages implement the same three calls from the proto file. They must:

1. accept `Register` and keep a resuming worker's session ID;
2. send commands with unique IDs, and resend those not yet acknowledged when a worker reconnects;
3. answer each `PublishActivity` hello with what they hold, store events only in sequence, skip duplicates, and acknowledge only what is stored;
4. return `NOT_FOUND` on a stream for a session they don't know, so the worker registers again.

## Trying it locally

```sh
make build
bin/actlet-devgw -token dev &                       # listens on 127.0.0.1:7400
ACTLET_GATEWAY=127.0.0.1:7400 ACTLET_TOKEN=dev \
  bin/actlet run -session ./demo-session
```

`actlet-devgw` assigns a demo task, prints the activity it receives and records it under `devgw-data/`. Type commands on its standard input: `sessions`, `pause drain`, `pause interrupt`, `stop 5s`, `stop kill`, `steer <note>`, `use <session>`. After a pause, run the same `actlet run` command again to resume.
