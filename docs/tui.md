# Watching a session

Actlet runs headless; a terminal viewer is something you add when you want to look. The same viewer shows three things:

- a box running somewhere else on the machine: `actlet attach -session <dir>`;
- the run in your own terminal: `actlet run -tui` runs the session and shows it;
- a recording: `actlet-cli replay play <file.actrec>` plays a gateway's recording back, offline.

Whichever it shows, the viewer is only a viewer. Quitting it (`q`) detaches: the session carries on. If a viewer reads too slowly, the box skips events for it and says how many, rather than wait for it or buffer without bound; a viewer that crashes or is killed doesn't affect the run either.

Try it with the demo, a scripted session of about two minutes with subagents, tool calls, a failing script and an operator's note:

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
| Session | state, the box's folder and output sizes, memory, CPU, the gateway connection, token use and cost per worker, budgets |
| Task | `task.json` and the worker tree, with each subagent's task and result |
| Profile | the effective configuration, the models and their context windows, budgets, what local viewers may do |
| Tools | the tools and their recovery policies, the profile's grants, the tools manifest |
| Events | every event as one line, for debugging |

Figures Actlet doesn't have show as `n/a`: cost needs a gateway that reports it with each model call, context percentages need a context window in the profile's model catalogue, and the box's figures come only from a running box. Budgets aren't enforced yet; the viewer shows how much of each has been used.

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

A note typed into the viewer goes to the agent like any steer note: it reaches the agent at its next turn boundary, exactly once, and it is recorded with origin `local` and the viewer's user name. Pausing and stopping are the orchestrator's business, so a local viewer may only send notes unless the profile allows more:

```yaml
local_attach:
  commands: [steer, pause]   # steer, pause and stop; [none] allows nothing
  # disabled: true           # no local attach at all
```

A refused command shows in the status line, and the session isn't touched.

## How it attaches

A running box listens on a Unix socket in its session folder, `.actlet/attach.sock`, readable only by its own user. When the folder's path is too long for a socket, the socket goes in the temporary directory instead and `.actlet/attach.addr` names it. Windows 10 and later have Unix sockets too, so it works the same there. `-no-attach` (or `ACTLET_NO_ATTACH=true`) turns it off. Once the session ends the socket goes away; a viewer still open on the same machine then reads files from the session folder itself, by the same rules.

The socket serves the attach protocol, [`proto/actlet/attach/v0`](../proto/actlet/attach/v0/attach.proto), over gRPC: `Info` describes the session and the box's figures, `Watch` streams the run's activity from a position (what the box still holds, then live events), and `Command` sends a note or, when allowed, a pause or stop. The box keeps the last few thousand events, plus the run's start, state changes and subagent starts and ends, so a late viewer still sees the run's shape; a viewer told it missed events shows that in the Session tab. Remote viewers will use the same protocol.

`actlet attach -dump` and `actlet-cli replay play -dump` print a screen as text and exit, without a terminal: handy for scripts and bug reports. `-tab` picks the tab and `-width` and `-height` the size.

## Untrusted text

Everything the viewer shows from a session (model text, tool output, file contents, diffs) is untrusted. Before it is shown, terminal control sequences are removed: colour codes, cursor movement, screen clearing, window titles, clipboard writes and other escape sequences, as well as carriage returns and bidirectional overrides. A file or a command's output can't change the terminal or hide what it says.
