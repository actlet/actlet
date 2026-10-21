# Gateway protocol

Actlet always runs under an orchestrator. The orchestrator serves a **gateway**; Actlet connects out to it, registers its session, takes control commands and reports what it is doing. Actlet never listens for connections itself, so it works behind NAT and in locked-down containers.

This document describes protocol version **v0**, how Actlet behaves on top of it, and what a gateway must do. The contract is in [`proto/actlet/gateway/v0/gateway.proto`](../proto/actlet/gateway/v0/gateway.proto).

> **Status: experimental.** v0 may change incompatibly. The messages are built to grow: new commands, event kinds and profile fields can be added without breaking existing peers.

## Overview

The transport is gRPC over one address. A worker uses four calls:

| Call | Kind | Purpose |
|---|---|---|
| `Register` | unary | Attach the worker to a session; receive the profile and the task |
| `Control` | bidirectional stream | Commands to the worker; acknowledgements and status from it |
| `PublishActivity` | bidirectional stream | Activity events from the worker; acknowledgements from the gateway |
| `Infer` | server stream | One model call of the session's agent; the gateway streams the answer |

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
- `profile`: the behaviour the orchestrator chooses: identity and version, limits, the default pause mode for OS signals, the default stop grace period, and the features the worker must support. Budgets, model aliases and tool grants are in the schema but not yet enforced. Actlet clamps the profile to the ceilings set by its own deployment flags, and records the effective configuration in the journal. No credentials ever pass through the profile. See [profiles](profiles.md).
- `task`: the task. **The folder comes first:** if the folder already holds a `task.json`, that is used. Otherwise the gateway's task is written to `task.json`. A task has a schema version, an ID, a kind, and a JSON definition and input whose shape depends on the kind.

Registration is retried with backoff while the gateway is unreachable. Errors that retrying can't fix stop the run: bad credentials, an unsupported protocol version, an invalid request, or a profile requiring features the worker lacks. A gateway must check `profile.required_features` against the worker's `capabilities` and refuse with `FAILED_PRECONDITION`; the worker checks too.

## Control

### Commands

The gateway sends `Command`s down the control stream. Each has:

- an **ID**, chosen by the gateway and unique within the session;
- an **origin** (the orchestrator, a `local` viewer on the box's machine, or a `remote` viewer attached through the orchestrator or to the box's port) and an **actor** (for example an operator name), both recorded;
- one **action**.

| Action | Effect |
|---|---|
| `pause` | Stop starting new work, persist, and **end the process**. `mode` is required: `drain` lets running work finish, `interrupt` signals it to stop now. A pause without a mode is rejected |
| `stop` | Cancel the task: signal running work, kill it after `grace` (default from the profile), or at once with `kill`. Persist and **end the process**. A stopped session doesn't resume |
| `steer` | Pass an operator note to the session. It is recorded and published as activity |
| `profile_update` | Change some of the profile: the fields it names, which must be among `limits.max_workers`, `limits.max_tool_concurrency`, `limits.max_subagents`, `budgets` and `local_attach`, and budgets to reset. An update naming any other field is rejected. See [updating a running session](profiles.md#updating-a-running-session) |

The worker writes every command to its journal, together with whether it accepted it, **before** acknowledging it. A `CommandAck` therefore means the command is durable on the worker's side. If the worker can't record a command, it doesn't acknowledge it.

Commands are idempotent by ID. A worker that receives an ID it has seen before, in this run or an earlier one, acknowledges it again with `duplicate: true` and doesn't apply it a second time. A gateway can therefore always resend a command it hasn't seen acknowledged. The SDK does this automatically after a reconnect.

A command with an action the worker doesn't know is rejected rather than ignored. A newer gateway talking to an older worker learns that the action wasn't applied.

### File reads

For viewers attached through the orchestrator ([attaching from elsewhere](tui.md#attaching-from-elsewhere)), the gateway asks the worker for files on the control stream: a `FileRequest` with an ID and a path relative to `workspace/`, answered by a `FileResponse` with the same ID. A worker that announces the `attach.files` capability serves files in `inputs/`, `workspace/` and `output/` only, never through symbolic links out of them, cuts large files short (256 KiB) and reports binary files without their content. It serves a few at a time on goroutines of its own, so file reads never delay commands, and refuses more. A file read isn't a command: it isn't recorded or acknowledged, and it isn't sent again after a reconnect.

### Status

The worker opens the stream with a `ControlHello` (session, epoch, state), reports its state on a heartbeat, and sends a final status as it exits. States are `running`, `pausing`, `stopping`, and the final `paused`, `stopped`, `completed` or `failed`.

The gateway sends a `Heartbeat` down the stream when it is otherwise idle (`sdk.Options.Heartbeat`, every 15s by default), so that a proxy with a read timeout in that direction doesn't cut it. Workers ignore heartbeats.

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

Event kinds in v0: session started, state change, command received, steer note, text output and gap; for agent tasks, model turn, tool started and finished (with the usage of calls through the gateway), file change (with a diff), subagent started and finished, `compaction` of a worker's context, and the agent's `progress` reports and `feedback`; `budget_status`, the budgets' use, at the start of each run, at alerts and limits, and when they change; and `session_info`, sent once at the start of each run, which describes the task, the effective configuration, budgets, models and tools for viewers. Events are small: activity describes what happened, and larger content belongs in the session folder.

An agent's events name the worker they are about in `worker`: `root` for the session's agent, `root.1` for its first subagent, and so on (see [the agent](agent.md#subagents)). Events about the session itself have none.

### Publishing and acknowledgement

The worker opens one `PublishActivity` stream per epoch with an `ActivityHello`. The gateway answers at once with an `ActivityAck` holding the highest sequence number up to which it already holds every event of that epoch. The worker sends from there, in order and in batches, and the gateway acknowledges as it stores them.

The gateway deduplicates by sequence number. It stores an event only if it directly follows what it holds, skips events it already has, and stops at a gap so the worker resends from the acknowledgement. An event is acknowledged only once it is stored, and the worker discards it only then.

The worker keeps at most a window of unacknowledged events in flight (1024). If events are outstanding and no acknowledgement arrives within 30s, it reconnects the activity stream.

### Buffering, spilling and gaps

Emitting activity never waits for the network or the disk. Events wait in a bounded in-memory queue (`-activity-queue`). When that is full, they spill to `.actlet/activity-<epoch>.actrec` in the session folder, and are read back from there once the gateway is reachable again. The spill file is deleted when everything in it has been acknowledged.

The spill file is capped (`-activity-spill-max`). Beyond the cap, display-only events, such as text output, are dropped. A **gap** event then reports how many were dropped and over what time span. State changes, commands and gap events keep a small reserve beyond the cap. If the disk is full, or the spill file can't be written for another reason, events of any kind that find no room in memory are dropped, and a gap reports them once the gateway has caught up; emitting never fails, and the session carries on. Execution-critical state never depends on activity: it lives in the journal.

When a run ends, the worker waits briefly (`-flush-timeout`) for outstanding events to be acknowledged, and writes the rest to the spill file. The next run sends them as a **backlog** for the earlier epoch, alongside its own events.

## Inference

An agent's model calls go to the gateway: Actlet holds no provider SDKs, endpoints or credentials. `Infer` sends one call:

- `model`: an alias from the profile's catalogue, the one assigned to the worker's role (`main` or `subagent`), or one it may choose. The gateway resolves it to whatever provider and model it likes.
- `purpose`: `agent` for a worker's turn, `llm` for the agent's `llm` tool (a one-off call with no tools), `compaction` for a summary of a worker's older turns when [its context fills](agent.md#context-compaction); `worker` names the worker.
- `system`, `messages` and `tools`: a provider-neutral conversation. Messages are `user` or `assistant`, made of text, tool calls (`id`, `name`, JSON `input`) and tool results (`call_id`, `content`, `is_error`). Tools are described by name, description and JSON schema.
- `continuation`: opaque bytes from the previous answer, passed back unchanged. A gateway can keep provider state in it, such as a response ID or cache keys; the worker never looks inside.
- `intent_id`: the call's identity. The worker records it in its journal before calling, and uses it again for every retry of that call, including after a crash. The gateway treats it as an idempotency key.

The answer streams text deltas and complete tool calls in the order the model produced them, then `done`: the stop reason (`end_turn`, `tool_use`, `max_tokens`, `refusal`), token usage (with the call's `cost` and `currency` when the gateway knows them) and the next continuation.

A call carries the whole conversation, which can be large before the worker [compacts it](agent.md#context-compaction), so a gateway must accept messages of up to 64 MiB, more than gRPC's default 4 MiB; `sdk.ServerOptions` sets it. A worker whose call is refused for its size fails the session with that reason rather than retrying.

A call is never continued after an interruption. The worker discards what an interrupted attempt received and makes the call again from its last journaled state, with the same intent ID. It retries `UNAVAILABLE`, `RESOURCE_EXHAUSTED`, `ABORTED` and `DEADLINE_EXCEEDED`, and an answer that ends without `done` or goes quiet for five minutes; `NOT_FOUND` makes it register again first. Any other error fails the session with the gateway's message.

`sdk.Options.OnInference` serves calls in Go. The SDK keeps recent answers by intent ID (`InferenceCache`, 256 by default): a repeated call gets the same answer, marked `replayed`, without reaching `OnInference`, and a repeat of a call still running waits for it. The cache is in memory; a gateway with several replicas, or one that restarts, can recognise intent IDs itself. A draining gateway lets running calls finish.

Package `sdk/scripted` is a model that plays a YAML script of turns, for testing agents without a real model; `actlet-devgw -model <script>` serves it. A script can also answer `llm` calls, by their prompt, `decide` calls, by their question, and `compaction` calls with a summary, and set what each call costs.

## Bridged tools

Some of the agent's [standard tools](agent.md#standard-tools) are implemented by the orchestrator: today `decide`. The worker calls `CallTool` with the tool's name, its JSON input, the worker and an `intent_id`, recorded in its journal before the call, and gets the JSON output, whether it failed, and what it used (counted towards the budgets). A call may wait as long as the orchestrator takes, such as for a person to decide. The worker asks again with the same intent ID after a pause, a crash or a lost connection, and retries the same codes as model calls, so the gateway should answer an intent ID once.

`sdk.Options.OnDecide` answers `decide` calls in Go, with a typed question and decision; `OnTool` answers other bridged tools, and `decide` too when there is no `OnDecide`. The SDK keeps answered calls by intent ID, like model calls. A gateway that provides neither answers `UNIMPLEMENTED`, and the agent is told the tool isn't available.

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
- `sdk.MemoryStore` and `sdk.RecordingStore` (one `.actrec` file per session, readable with `actlet-cli replay inspect` and `actlet-cli replay export`);
- `OnInference` to answer agents' model calls, deduplicated by intent ID, and `OnDecide` and `OnTool` to answer [bridged tools](#bridged-tools), for which the SDK has [provider adapters](providers.md);
- `Session.UpdateProfile` to change a running session's profile, which the SDK also applies when the worker registers again;
- `Server.RegisterAttach` to serve the [attach protocol](../proto/actlet/attach/v0/attach.proto) to viewers such as `actlet-cli attach`. A viewer names its session in the `actlet-session` request metadata. Viewers are authenticated with `Authenticate`, like workers (`grpc.Method` tells their calls apart); `AuthorizeAttach` decides which sessions each may attach to, and `AttachCommands` what it may send, by default what the session's profile lets local viewers send. A viewer gets the session's earlier activity when the store is an `ActivityReader`, as both stores are, then live events; the server keeps a bounded backlog per watched session, and a slow viewer skips events rather than slowing the session's activity down. `Info` lists the viewers watching a session through this server, by the name each gives itself or, with `AttachViewerLabel`, by a label the orchestrator gives it, such as the user its token stands for; viewers attached to the box itself aren't listed there, nor are any viewers recorded.

Gateways in other languages implement the same calls from the proto file. They must:

1. accept `Register` and keep a resuming worker's session ID;
2. send commands with unique IDs, and resend those not yet acknowledged when a worker reconnects;
3. answer each `PublishActivity` hello with what they hold, store events only in sequence, skip duplicates, and acknowledge only what is stored;
4. return `NOT_FOUND` on a stream or call for a session they don't know, so the worker registers again;
5. answer a repeated `Infer` intent ID with the earlier answer where they can, and return retryable codes only for failures worth retrying.

## Trying it locally

```sh
make build
bin/actlet-devgw -token dev &                       # listens on 127.0.0.1:7400
ACTLET_GATEWAY=127.0.0.1:7400 ACTLET_TOKEN=dev \
  bin/actlet run -session ./demo-session
```

`actlet-devgw` assigns a demo task (or `-task <file>`) with a default profile (or `-profile <file>`), prints the activity it receives, records it under `devgw-data/`, and lets viewers attach with its token (`ACTLET_ATTACH_TOKEN=dev bin/actlet-cli attach -gateway 127.0.0.1:7400`). Read recordings with `actlet-cli replay inspect|export`. `make image-devgw` builds it as a container image. Type commands on its standard input: `sessions`, `pause drain`, `pause interrupt`, `stop 5s`, `stop kill`, `steer <note>`, `use <session>`. After a pause, run the same `actlet run` command again to resume.
