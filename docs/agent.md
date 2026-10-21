# The agent

A task of kind `agent` runs Actlet's agent loop: the worker builds a prompt, calls the model through the gateway, runs the tools the model asks for, and repeats until the model ends the task. The agent can hand subtasks to **subagents** that work in parallel. Actlet holds no provider SDKs or credentials; every model call goes through the gateway's [`Infer` call](gateway-protocol.md#inference).

## The task

```json
{
  "schema": "v0",
  "id": "sum-numbers",
  "kind": "agent",
  "definition": {"max_turns": 20},
  "input": "Add up the numbers in inputs/numbers.txt and write a short report to output/report.md."
}
```

- `input` is what the agent is asked to do: a string, or any JSON value, which the model is shown as JSON.
- `definition.max_turns` bounds the model calls (default 100); `definition.max_output_tokens` bounds each answer (default: the gateway's choice). Unknown fields are errors.
- Files the agent works on go in `inputs/` before the run. The agent can't change them; its deliverables go to `output/`.

The task ends when the model answers without calling a tool. If `output/` is still empty then, the model's final answer is written to `output/result.md`. `actlet run` exits 0.

## The prompt

The system prompt has layers:

1. **Actlet's base prompt**: how the box works: the session folder, the tools, `outcome_unknown`, and how to finish. Its version (`actlet-base-v0.1`) is recorded in the journal. A profile's `base_prompt` replaces it.
2. **The implementor's instructions**: the profile's `instructions`.
3. **What is installed**: the image's tools manifest (`actlet run -tools-manifest`, by default `/etc/actlet/tools.md` if it exists), the profile's `tools_manifest`, and notes such as a missing `bash`.
4. **Skills**: the name and description of each [skill](#skills).

The first user message is the task's input, followed by the list of input files.

The model is the alias the profile assigns to the `main` role (`models.roles.main`), or `main` if none is set.

## Tools

| Tool | What it does | Recovery |
|---|---|---|
| `read_file` | Numbered lines of a text file, with `offset` and `limit` | run again |
| `write_file` | Create or overwrite a file; missing directories are created | reconcile |
| `edit_file` | Replace an exact, unique piece of text (or every occurrence with `replace_all`) | reconcile |
| `glob` | Find files by pattern: `**` crosses directories, a pattern without `/` matches names | run again |
| `grep` | Search contents with a regular expression; prints `path:line:text` | run again |
| `shell` | Run a command with `bash` (or `sh` if there is no `bash`) in `workspace/` | never |
| `python` | Run a script, given as code or a path, with structured exceptions and tracebacks | never |

- Relative paths are relative to `workspace/`. `inputs/`, `.actlet/` and `task.json` can't be written, also not through symbolic links.
- Searches use `ripgrep` when it is installed and a built-in walker otherwise.
- `shell` and `python` run in a process group of their own, with a timeout (default 2 minutes, at most 10) and output capped at 32 KiB per stream, keeping its start and end. Their environment is the worker's without `ACTLET_*` variables, which can hold the gateway token.
- `python` is offered only when `python3` or `python` is installed. An uncaught exception comes back with its type, message and the traceback's files, lines and code, with hints for a missing module or file.
- Every file change is published as a `file_change` activity event with a unified diff.

### Processes the tools start

Nothing a tool starts outlives the run, however it ends: completed, failed, paused, stopped, or by a signal. A command may leave processes running in the background for later calls, such as a server it tests against, but when `actlet run` ends it kills every process group the tools started. On Linux it is also a *child subreaper*: descendants that leave their process group and session, as daemons do with `setsid`, are adopted by it rather than by the system, killed at the end too, and reaped as they exit. On Windows, which has no process groups, each command runs in a job object of its own, which everything it starts joins too, so stopping a command stops its children, and they can't escape it. On macOS, such daemons can escape.

As a container's first process, PID 1, `actlet` runs itself again as its child, as `tini` does: it reaps every orphan in the container, passes `SIGTERM`, `SIGINT`, `SIGHUP`, `SIGQUIT`, `SIGUSR1`, `SIGUSR2` and `SIGWINCH` on to the child, and once the child exits, kills whatever is left and exits with the child's code. `ACTLET_NO_INIT=true` turns this off, as for a container that has an init process already (`docker run --init`). `make test-linux` tests both in a Linux container.

A profile's `tools` list restricts the tools to those named, with settings: `shell` takes `shell`, `timeout`, `max_timeout` and `output_cap`, and `python` takes `interpreter`. Without a list, all the local tools are offered.

### Standard tools

Actlet also has standard tools, offered only when the profile's `tools` list names them:

| Tool | What it does | Recovery |
|---|---|---|
| `report_progress` | Tells the people watching how the task goes: a message and, if the agent can tell, a percentage. Published as a `progress` event | run again |
| `send_feedback` | Sends the orchestrator a `problem`, `suggestion`, `question` or `other` note about the task or its setup. Published as a `feedback` event | run again |
| `budget_status` | Tells the agent how much of each budget is used | run again |
| `decide` | Asks for a decision: a question, options and context, which the orchestrator puts to a person or a system that decides. Its answer is the call's result | ask again |
| `llm` | Calls another model for side work, such as summarising, with a prompt and no tools: the profile's `llm` role alias (or its `processing` one), or one the agent picks from `models.selectable`. Not offered when there is neither | ask again |

`decide` and `llm` go through the gateway: `decide` with the `CallTool` call, which the orchestrator implements, and `llm` as a model call with purpose `llm`. Both carry the tool call's intent ID. A `decide` call can wait a long time: a pause or stop ends the wait without an outcome, and the next run asks again with the same intent ID, also after a crash, so the orchestrator answers it once. What both use counts towards the session's [budgets](profiles.md#budgets), and both wait while a budget holds work back.

### Skills

Skills are Claude-style skill folders: a `SKILL.md`, whose front matter gives the skill's `name` and `description` and whose body its instructions, and optional scripts and resources. They come from the image, as folders in `actlet run -skills` (by default `/etc/actlet/skills`), and from the profile's `skills`, which the worker writes to `.actlet/skills/` in the session folder at the start of each run. A profile's skill replaces the image's of the same name.

The prompt lists each skill's description, and the `skill` tool, offered whenever there are skills, loads one: its instructions, its folder and its files, or with `file` one of those files, never from outside its folder. The agent runs a skill's scripts with the `shell` or `python` tools. Skill folders can't be written. Calls of `skill` are journaled and published like any other tool's, and run again after a crash.

### Calls in parallel

When the model calls several tools in one answer, they run together, up to the profile's `limits.max_tool_concurrency` at once. Calls that would change the same files wait for each other:

- `write_file` and `edit_file` write their file;
- `shell` and `python` may write anything, unless the call lists what it writes in `writes` (for example `"writes": ["build/"]`, or `[]` for a command that only reads);
- the read and search tools write nothing and never wait, so a read in the same answer as a write of that file may see the file before or after it.

What a call writes is taken from its input, so an edit of a file that an earlier call in the same answer creates waits for it. Two calls conflict when one writes a path the other writes, or a directory containing it, or when either may write anything. A call waits only for earlier calls it conflicts with, so conflicting calls run in the order the model made them. Subagents share the workspace and the same locks, so the rule holds across them too. Results go back to the model in the order of the calls, whichever finished first.

## Subagents

`spawn_subagent` starts a subagent on a subtask and waits for its final answer, which becomes the call's result. Several calls in one answer run their subagents in parallel.

- A subagent has its own conversation: Actlet's base prompt with a note that it is a subagent, the profile's layers, and the subtask as its first message. It has the same tools, and shares the workspace.
- Its model is the profile's `subagent` role alias (or the agent's own), or one the call picks from the profile's `models.selectable`.
- A parent's task isn't done until all its subagents have finished and delivered their results: the call blocks until then, so there are no orphans, and `actlet run` exits 0 only when the whole tree has finished.
- Limits, from the profile: `limits.max_subagent_depth` (how deep subagents may nest, default 2), `limits.max_subagents` (how many a session creates, default 8) and `limits.max_workers` (how many workers run at once, the agent included). A spawn beyond a limit fails with an error saying which, so the model can do the work itself. A worker waiting for its subagents gives up its place while it waits, and waiting workers start in order.
- The call's `budget_share` is recorded but not enforced: [budgets](profiles.md#budgets) are the session's, subagents included.
- Workers are named after their parent: `root` is the agent, `root.1` its first subagent, `root.1.2` that one's second. Every activity event and journal record names its worker, and `subagent_spawned` and `subagent_finished` events mark a subagent's start and end.
- A subagent that fails, for example by using up its turns, returns the failure as the call's error result; the parent carries on. A panic ends only its own worker.

## Context compaction

Long tasks fill a model's context. When a model call's context, its input and output tokens, reaches `compaction.threshold_percent` (80 by default) of the worker's alias's `context_window`, the worker compacts it before its next call, as Claude Code's `/compact` does:

- A model writes a summary of the older turns: the profile's `compaction.model`, else the catalogue's `compaction` role, else the worker's own model. The call goes through the gateway with purpose `compaction`, gets a transcript of the turns with long tool inputs and results cut short, and counts towards the [budgets](profiles.md#budgets).
- The worker carries on with its first message, the task, now ending with the summary, followed by its `compaction.keep_turns` most recent turns (4 by default) as they were. A later compaction replaces the earlier summary. Calls in flight and pending tool results are never summarised: a worker compacts only between turns.
- A `compaction` activity event reports it: the model, the turns summarised and kept, the context's size, the summary and the call's usage. Viewers show it in the worker's timeline.
- A checkpoint is written before the compaction's intent and after its outcome. A crash in between makes the summary's call again with the same intent ID; one after it carries on from the compacted conversation.

A worker whose alias has no `context_window` never compacts, nor does any worker with `compaction.strategy: off`.

## Durability and recovery

Every model call and every tool call is written to the session journal as an **intent** before it starts and an **outcome** when it ends. Each record is written and applied to the workers' state as one step, so a **checkpoint** holds the state of every worker, calls in flight included. Checkpoints are written at model-turn boundaries: after every turn, or every `checkpoint_every_turns` turns from the profile, and when the session pauses. A checkpoint larger than a journal record (4 MiB), as long conversations make, is compressed and written in parts and then a small record that names them by checksum; a crash among the parts leaves the previous checkpoint the latest. A run restores every worker from the latest checkpoint, replays the records after it, and continues each worker:

- **A model call without an outcome** is made again, from the same conversation and with the same intent ID, which the gateway uses as an idempotency key. A call is never continued mid-answer.
- **A tool call without an outcome** follows its tool's policy:
  - *run again*: read-only tools repeat the call;
  - *reconcile*: writes and edits record the file's hash before and after in the intent. If the file now has the new content, the call is done; if it still has the old content, it runs again; if it has anything else, the outcome is unknown;
  - *never*: a shell command or Python script may have done anything. It isn't run again, and the model gets `outcome_unknown` as the call's result, so that it checks before repeating it.
- **A subagent's result reaches its parent exactly once.** Spawning is journaled (it is also the call's intent), then the subagent's final answer, then the parent's tool outcome. A run that finds a spawn without an outcome waits for that subagent rather than starting another, and one that finds the answer journaled delivers it.

### Without the gateway

Local work doesn't need the gateway. If it goes away, running tools finish and their outcomes are journaled, activity piles up in memory and the spill file, and the next model call waits, retrying, until the gateway is back. The session then carries on from where it was.

## Pausing and stopping

- `pause drain` lets the running tool or model call finish, records it, and starts nothing new.
- `pause interrupt` stops it: a command's process group gets `SIGTERM`, and a model call is abandoned (and made again with the same intent ID on resume). The model is told a command was interrupted and may have run partly.
- `stop` interrupts, then kills after the grace period.

The next run carries on from where the session paused: remaining tool calls run, then the next model call.

## Steer notes

An operator's `steer` note, from the orchestrator or typed into a [viewer](tui.md), reaches the agent (the root worker) at its next turn boundary: it is added to the message before the next model call as `[Operator note from <who>, sent while you worked; take it into account] <note>`. Each note is journaled when it joins the conversation, so it is given exactly once, also across crashes and pauses; a note sent just before a pause reaches the agent when the session resumes. Subagents don't get notes; the agent can pass them on.

Actlet's own notices reach the agent the same way, as `[Notice from Actlet] <notice>`: today, that a budget has reached its alert threshold.

## Trying it

`actlet-devgw -model <script>` answers model calls from a script of turns ([`sdk/scripted`](../sdk/scripted)), so you can run the agent without a real model:

```sh
make build
mkdir -p agent-session/inputs && cp examples/agent/numbers.txt agent-session/inputs/
bin/actlet-devgw -token dev -task examples/agent/task.json -model examples/agent/model.yaml &
ACTLET_GATEWAY=127.0.0.1:7400 ACTLET_TOKEN=dev bin/actlet run -session ./agent-session
cat agent-session/output/report.md
```

The gateway prints each model turn, tool call and diff, with the worker it is from. Each scripted turn can check the tool results it receives (`expect`) and texts the conversation must hold exactly once (`expect_once`), and wait before answering (`delay`) to try pausing or crashing during a model call. A script's `subagents` give subagents their own turns, chosen by a phrase in their subtask.
