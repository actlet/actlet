# Security

This is Actlet's threat model: who and what it trusts, what it protects, where the boundaries lie, and how each surface is defended. The last section lists what the review found, fixed or not.

Actlet is not a sandbox. Isolation, from the network, the host and other tenants, is the job of whatever runs the container: Docker, Kubernetes, gVisor, a micro-VM. Actlet makes its own surfaces safe within that, and says plainly where it relies on the container.

## Actors

| Actor | Trusted to | Not trusted to |
|---|---|---|
| **The orchestrator** (the gateway the worker connects to) | assign the task, the profile and the skills; answer model and tool calls; command the session | Nothing beyond that. Its permissions are authoritative: Actlet never widens them. |
| **The implementor** (whoever builds the image and deploys the worker) | choose the image, its tools and skills, the container's isolation, its network and volumes | |
| **The agent** (the model, through the tools) | do the task with the tools it is given | stay inside the session folder, keep secrets, or leave the box's own files alone. It runs commands as the worker's user, and what it writes to `workspace/` is attacker-controlled for everything that reads it. |
| **Viewers**: the local TUI, `actlet-cli attach`, and viewers through the orchestrator | watch the session, read its `inputs/`, `workspace/` and `output/`, send the commands the profile allows | read anything else, or send other commands |
| **Whoever can write the session folder or the recordings** | | Journals and recordings have checksums, not authentication. |

## Assets

- **Credentials**: the gateway token (`ACTLET_TOKEN`), the attach port token (`ACTLET_ATTACH_TOKEN`), and in the orchestrator the providers' API keys.
- **The session's state**: the journal in `.actlet/`, which decides what recovery does and so what runs again.
- **The session's files**: inputs, workspace and deliverables.
- **The orchestrator's budgets**: model calls cost money.
- **The host**: CPU, memory, disk and processes.

## Trust boundaries and surfaces

### The gateway connection (worker → orchestrator)

The worker connects out over gRPC, with a bearer token, and with TLS when `-tls` is set. Everything the gateway sends is trusted as the orchestrator's: task, profile, skills, commands and model answers. Untrusted input in it, such as model text and tool calls, is bounded and validated before use:

- tool inputs against their schemas;
- file paths against the session folder;
- every queue, buffer and read has a limit.

The token is never written to the journal, the recordings, the activity or the logs. Tools run without the `ACTLET_*` variables, so the agent can't read it from its environment.

On the gateway side (the SDK), every call is authenticated with `Options.Authenticate`; `StaticToken` compares in constant time. Activity is stored only in sequence: no gaps, no duplicates, nothing from another epoch. Model calls are deduplicated per session and intent ID. Fuzzing: `FuzzWorkerMessages` sends garbled messages on every call, and the gateway must not crash, must keep serving, and must keep each session's activity in order.

### The attach socket and port (viewers → box)

- The socket is in `.actlet/`, readable only by the worker's user. Anyone who can use it can do what the profile lets local viewers do.
- The attach port is off by default. When on, it listens on loopback unless `-attach-allow-remote` is set, and it needs a token, compared in constant time.
- Viewers through the orchestrator are authenticated and authorized by the SDK's `Authenticate`, `AuthorizeAttach` and `AttachCommands`.
- Commands are limited to those the profile allows: steer, pause or stop.
- Text from the session is sanitised before the TUI draws it, so the agent can't send terminal escape sequences to a viewer's terminal.
- Fuzzing: `FuzzCommand` checks that only allowed commands reach the session, and never with an empty note.

### File reads by viewers

Viewers may read files in `inputs/`, `workspace/` and `output/` only: never `.actlet/`, `task.json`, or anything outside the session folder, also not through symbolic links. Reads are capped at 256 KiB. The agent controls `workspace/`, so a check of a path and the open that follows it must not be separable: the file is opened inside its readable directory with `os.Root`, which no link swapped in after the check can lead out of. Fuzzing: `FuzzReadFile`. Test: a directory swapped for a link to `.actlet/` while a viewer reads through it.

### Skills

Image skills are as trusted as the image. Profile skills are as trusted as the orchestrator. A skill's instructions and files reach the model as text, so a skill can steer the agent. Which skills to give is the implementor's and the orchestrator's choice.

What Actlet enforces for skills:

- **Names and paths:** skill names are checked, and file paths must be relative and stay inside the skill's folder.
- **Size:** profile skills are capped at 2 MiB in all.
- **Writing and reading:** they are written to `.actlet/skills/`, which tools can't write, and read with `os.Root`, so no link leads out.

### Recordings and journals

`actlet-cli` reads journals and recordings someone gives it, possibly damaged or hostile:

- The reader checks framing, lengths and checksums, and bounds every allocation and decompression.
- It reports corruption with its offset rather than guessing.
- Fuzzing: `FuzzScan` and `FuzzOpen`.

Checksums detect accidents, not attacks, and compression isn't encryption. A forged journal is a forged session. Protect the session volume and the recordings as you would the data in them.

### The tools' environment

The agent's tools run as the worker's user, in the worker's container:

- **Network and filesystem:** the shell and Python tools can use the network and read anything that user can. Actlet doesn't block it; the container's isolation decides.
- **What tools can't write:** `inputs/`, `.actlet/`, `task.json` and skill folders, also not through links.
- **Bounds:** tool output is capped, and commands run in process groups with timeouts.
- **Processes:** nothing a tool starts outlives the run. On Linux, daemons are caught too, and as PID 1 the worker reaps every orphan in the container ([the agent](agent.md#processes-the-tools-start)).

### Resources

Every queue, buffer and decoded message is bounded. A slow viewer skips events rather than slowing the session, and a full spill disk drops display events with a gap marker. Budgets limit time, money, tokens and disk, as the orchestrator sets them. CPU and memory limits are the container's.

## Checks

- `make check` runs `govulncheck` (`make vulncheck`), which reports known vulnerabilities in code the build actually reaches. It needs the network: `VULNCHECK=0` skips it offline, and it is skipped when govulncheck isn't installed (`make setup` installs it).
- `make fuzz` runs every fuzz target for `FUZZTIME` each, and `make test` for 5 seconds each.

## Findings

| # | Finding | Status |
|---|---|---|
| 1 | Five HTTP/2 vulnerabilities in `golang.org/x/net` v0.57.0 that the code reaches through its gRPC servers and the provider adapters' HTTP client: GO-2026-6617, GO-2026-6612, GO-2026-6611, GO-2026-6610, GO-2026-6603 | **Fixed**: upgraded to v0.60.0; `govulncheck` now reports none |
| 2 | A viewer's file read checked the resolved path, then opened it by name again: a directory swapped for a link in between, which the agent can do in `workspace/`, led the read out, such as into `.actlet/` | **Fixed**: opened inside the readable directory with `os.Root`; a test swaps a directory while reading |
| 3 | Background processes and daemons started by tools outlived the run, and as PID 1 the worker left zombies | **Fixed**: process groups are killed on every exit path; on Linux a child subreaper catches daemons, and a tini-like init reaps as PID 1 |
| 4 | A replacement container started while the old one still held the journal failed at once, and could have recovered from a journal still being written | **Fixed**: the run waits for the lock (`-lock-wait`) before it reads the journal |
| 5 | The SDK authenticates a worker but doesn't tie its credential to a session: a worker with a valid token may make calls for any session whose ID it knows | **Listed.** Session IDs are random 128-bit values. An orchestrator that gives each worker its own credential should check, in `Authenticate`, that the credential matches the session named in the call. |
| 6 | Journals and recordings are checksummed, not signed | **Listed**, by design: protect the volumes |
| 7 | The worker connects without TLS unless `-tls` is set, and the token then crosses the network in the clear | **Listed**: use `-tls` whenever the gateway isn't on a trusted network |
| 8 | On macOS and Windows, a daemon a tool starts in a new session escapes the run's cleanup | **Listed**: production runs on Linux, where it is caught |
| 9 | Skill instructions and tool output reach the model unfiltered and can steer it (prompt injection) | **Listed**: the implementor chooses skills and the container's isolation; the orchestrator can review decisions with `decide` |
