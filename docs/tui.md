# Watching a session

Actlet runs headless; a terminal viewer is something you add when you want to look. The same viewer shows:

- a box running somewhere else on the machine: `actlet attach -session <dir>`;
- the run in your own terminal: `actlet run -tui` runs the session and shows it;
- a session running anywhere, through its orchestrator or straight from the box: `actlet-cli attach` ([attaching from elsewhere](#attaching-from-elsewhere));
- a recording: `actlet-cli replay play <file.actrec>` plays a gateway's recording back, offline.

Whichever it shows, the viewer is only a viewer. Quitting it (`q`) detaches: the session carries on. If a viewer reads too slowly, the box skips events for it and says how many, rather than wait for it or buffer without bound; a viewer that crashes or is killed doesn't affect the run either.

Try it with the demo, a scripted session of about two minutes with subagents, tool calls, a failing script, an operator's note and a budget alert:

```sh
make demo-tui      # the session with the viewer in the foreground
make demo-attach   # in a second terminal: another viewer on the same box
```

## The screen

```
 actlet  q3-sales-review                              Chat  Session  Task  Profile  Tools  Events
 ▌ Agent · turn 3 · large · 6.9k in / 610 out                            │ WORKERS
 ▌ Three regions with different stories. I'll give each region to a      │● root large 3 turns
 ▌ subagent so they can work in parallel.                                 │●  root.1 fast 1 turn
   ⠋ Subagent root.1  Region west: summarise the west's Q3 sales…  running│
     ┊ ▌ Subagent root.1 · turn 1 · fast · 2.1k in / 90 out              │ FILES
     ┊ ▌ Reading the west's rows.                                         │ workspace/totals.py +9 −0
     ┊   ✓ Grep ^west  in ../inputs/sales.csv                         24ms│
     ┊     │ ../inputs/sales.csv:8:west,2026-07,widget,150,4500           │
 ────────────────────────────────────────────────────────────────────────────────────────────────
  › press i to send the agent a note
  ⠋ running · large · ctx 3% · 0.08 EUR · 22s · local box                                  ? keys
```

The **Chat** tab is the conversation, one model turn at a time: who speaks (the agent, a subagent, the operator, or Actlet itself, each in its own colour), what the model said, then each tool call as one block with its result. A block shows the tool, its key arguments, how it went (✓, ✗, a spinner while it runs, ? when the outcome is unknown after a crash) and how long it took, then the start of its output or its diff. A subagent is shown inside the `Subagent` call that started it, with its own turns, and its final answer under it. The sidebar lists the workers and the files changed so far, by their path in the session folder, with the lines added and removed by all their changes together; it hides when the terminal is narrow.

The **status line** keeps to what matters at a glance: the session's state, the model, how full its context is, the cost so far, the time, and what the viewer is connected to. The rest is in the other tabs:

| Tab | Shows |
|---|---|
| Session | state, the agent's latest progress report, the box's folder and output sizes, memory, CPU, the gateway connection, token use and cost per worker, each budget's use and whether it is past its alert or used up, and the agent's feedback |
| Task | `task.json` and the worker tree, with each subagent's task and result |
| Profile | the effective configuration, the models and their context windows, budgets, what local viewers may do |
| Tools | the tools and their recovery policies, the profile's grants, the tools manifest |
| Events | every event as one line, for debugging |

Figures Actlet doesn't have show as `n/a`: cost needs a gateway that reports it with each model call, context percentages need a context window in the profile's model catalogue, and the box's figures come only from a running box seen directly, not through the orchestrator. A budget that is used up also shows in the status line.

## Keys and mouse

| Key | |
|---|---|
| `↑` `↓` | select the previous or next tool call (other tabs: scroll) |
| `enter` | expand or collapse the selected call |
| `e` | expand or collapse every call |
| `s` | collapse or show subagents' turns |
| `[` `]` | show one worker at a time |
| `t` | show one tool's calls at a time |
| `tab`, `1`–`6` | switch tabs |
| `pgup` `pgdn`, `end` | scroll, and back to following the end |
| `i` | type a note for the agent; `enter` sends it |
| `P`, `X` | pause or stop the session, if the profile allows it |
| `?` | show the keys |
| `q` | quit; the session carries on |

The mouse works too: click a tab, click a tool call to select it and again to expand it, and scroll with the wheel. Playing a recording adds `space` (play and pause), `←` `→` (seek), `+` `-` (speed) and `f` (follow a recording that is still being written).

## Notes, and what a viewer may do

A note typed into the viewer goes to the agent like any steer note: it reaches the agent at its next turn boundary, exactly once, and it is recorded with the viewer's user name and origin `local`, or `remote` for a viewer attached from elsewhere. Pausing and stopping are the orchestrator's business, so a viewer may only send notes unless the profile allows more:

```yaml
local_attach:
  commands: [steer, pause]   # steer, pause and stop; [none] allows nothing
  # disabled: true           # no local attach at all
```

A refused command shows in the status line, and the session isn't touched. The same setting governs viewers attached to the box's port, and, unless the orchestrator decides otherwise, viewers attached through it.

## How it attaches

A running box listens on a Unix socket in its session folder, `.actlet/attach.sock`, readable only by its own user. When the folder's path is too long for a socket, the socket goes in the temporary directory instead and `.actlet/attach.addr` names it. Windows 10 and later have Unix sockets too, so it works the same there. `-no-attach` (or `ACTLET_NO_ATTACH=true`) turns it off. Once the session ends the socket goes away; a viewer still open on the same machine then reads files from the session folder itself, by the same rules.

The socket serves the attach protocol, [`proto/actlet/attach/v0`](../proto/actlet/attach/v0/attach.proto), over gRPC: `Info` describes the session and the box's figures, `Watch` streams the run's activity from a position (what the box still holds, then live events), and `Command` sends a note or, when allowed, a pause or stop. The box keeps the last few thousand events, plus the run's start, state changes and subagent starts and ends, so a late viewer still sees the run's shape; a viewer told it missed events shows that in the Session tab. Viewers elsewhere use the same protocol.

`actlet attach -dump` and `actlet-cli replay play -dump` print a screen as text and exit, without a terminal: handy for scripts and bug reports. `-tab` picks the tab and `-width` and `-height` the size.

## Attaching from elsewhere

`actlet-cli attach` opens the same viewer on a session that runs elsewhere, over the network, through the attach protocol rather than a shell. The status line says what it is connected to.

**Through the orchestrator**, the usual way:

```sh
ACTLET_ATTACH_TOKEN=<token> actlet-cli attach -gateway orchestrator.example:7400 [-session <id>]
```

The orchestrator relays the session's activity (what it has recorded, then live events), the commands the viewer may send, and file reads, which it asks the box for over the box's own connection. Without `-session` it lists the sessions to pick from. The status line reads `orchestrator <address> · session <id>`. Which token lets a viewer in, and which sessions it may see, is the orchestrator's decision ([implementing a gateway](gateway-protocol.md#implementing-a-gateway)). `actlet-devgw` accepts its `-token`:

```sh
ACTLET_ATTACH_TOKEN=dev bin/actlet-cli attach -gateway 127.0.0.1:7400
```

**Directly to a box**, when the deployment opens the box's attach port. It is off unless `actlet run` is given an address, and then needs a token:

```sh
ACTLET_ATTACH_TOKEN=<token> actlet run -attach-listen 127.0.0.1:7600 ...
```

The port listens on loopback only, so nothing outside the box's network namespace reaches it. In Kubernetes, forward it to your machine and attach to the forwarded port:

```sh
kubectl port-forward pod/<pod> 7600:7600
ACTLET_ATTACH_TOKEN=<token> actlet-cli attach -direct 127.0.0.1:7600
```

The status line reads `box <address>`. The port speaks the same protocol as the session folder's socket, and shows the box's own figures.

Security notes:

- Every remote call needs the token; one without it, or with the wrong one, is refused.
- Pass the token in `ACTLET_ATTACH_TOKEN` rather than `-token`, which other users can see in the process list. Actlet removes every `ACTLET_` variable from the tools' environment, so the agent never sees the token, and no token is ever recorded in the activity or the journal.
- `-attach-allow-remote` (`ACTLET_ATTACH_ALLOW_REMOTE=true`) lets the port listen on another address, with a warning. Anyone who can reach it and has the token can then watch the session and read its files. The connection is plaintext: keep it on a trusted network, or tunnel it.
- Through the orchestrator, `-tls` (and `-tls-ca`) connect with TLS.
- A remote viewer reads only `inputs/`, `workspace/` and `output/`, as a local one does, and the box answers a few file reads at a time.
- Remote viewers are bounded like local ones: a slow viewer skips events, and neither a slow nor a crashed viewer affects the run.

## Untrusted text

Everything the viewer shows from a session (model text, tool output, file contents, diffs) is untrusted. Before it is shown, terminal control sequences are removed: colour codes, cursor movement, screen clearing, window titles, clipboard writes and other escape sequences, as well as carriage returns and bidirectional overrides. A file or a command's output can't change the terminal or hide what it says.
