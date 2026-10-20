# Forking a session

A fork is a new session that starts from a checkpoint of another: the same task, the same conversation up to that point, and its own session ID from there on. Try another model or another instruction from turn 3, or pick up a crashed session's work under a new ID, without touching the original.

```sh
actlet-cli fork --list ./session                       # the checkpoints
actlet-cli fork ./session --checkpoint "turn 2" --to ./fork
ACTLET_GATEWAY=... ACTLET_TOKEN=... actlet run -session ./fork
```

## What a fork is

`actlet-cli fork` writes a new session folder:

- **A journal** whose first record is the fork's **lineage**: the parent's session ID, the checkpoint's journal sequence number and the agent's model turn there. The checkpoint itself follows, copied from the parent's journal, in parts if it was too large for one record.
- **The parent's `task.json`.**
- **Empty `inputs/`, `workspace/` and `output/`.** Actlet keeps no snapshots of files, so it can't give the workspace as it was at the checkpoint. With `--copy-workspace`, the parent's `inputs/` and `workspace/` are copied as they are *now*, which may include what the parent did after the checkpoint. Copying the right files, or restoring a snapshot from the environment, is the implementor's job.

The parent is only read, and may be running, finished, or killed mid-run: a journal a crash cut short is read up to the damage.

`actlet run` on the fork's folder runs it as a session of its own: run 1 of a new session ID, which it registers with the gateway along with its lineage. It restores every worker from the checkpoint and carries on, as recovery after a crash does:

- Calls the checkpoint records as running are recovered as after a crash, by their tools' policies. A model call is made again; a file write is reconciled; a shell command's outcome is unknown, and the model is told so.
- Tool calls of the latest answer that hadn't started run now.
- Nothing after the checkpoint is copied: what the parent did later, the fork does again or differently.
- Budgets are the fork's own, from its profile. Its workers' token and cost usage carries over with their state, so money and token budgets count what the conversation had used by the checkpoint; time starts afresh.

## Choosing the checkpoint

Checkpoints are written at model-turn boundaries, every `checkpoint_every_turns` turns, and when a session pauses ([durability](agent.md#durability-and-recovery)). `--list` shows each one's journal sequence number, the agent's model turn, its workers and its time.

| `--checkpoint` | Picks |
|---|---|
| `latest` (the default) | the latest checkpoint |
| a number | the checkpoint with that journal sequence number |
| `turn N` | the latest checkpoint at the agent's turn N: after its Nth answer, and any of that turn's work checkpointed since |

`--session-id` sets the fork's ID; by default it gets a new random one.

## Lineage

A fork tells where it comes from:

- `RegisterRequest.lineage`, which the SDK keeps as `Session.Lineage()`;
- `SessionInfo.lineage` in its activity, which viewers show: the TUI's Session tab says "forked from", and `actlet-cli replay view --json` has it as `info.lineage`;
- its journal's first record, `session.fork`.

`Lineage` holds `parent_session_id`, `checkpoint_seq`, `turn` and `forked_at`.

## From Go

```go
cps, err := sdk.SessionCheckpoints(parentDir)
f, err := sdk.ForkSession(parentDir, childDir, sdk.ForkOptions{Turn: 2})
// f.SessionID, f.Lineage
```

An orchestrator forks a folder it controls, then assigns the new session ID to the worker that runs it.
