# The agent

A task of kind `agent` runs Actlet's agent loop: the worker builds a prompt, calls the model through the gateway, runs the tools the model asks for, and repeats until the model ends the task. Actlet holds no provider SDKs or credentials; every model call goes through the gateway's [`Infer` call](gateway-protocol.md#inference).

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

A profile's `tools` list restricts the tools to those named, with settings: `shell` takes `shell`, `timeout`, `max_timeout` and `output_cap`, and `python` takes `interpreter`. Without a list, all tools are offered.

## Durability and recovery

Every model call and every tool call is written to the session journal as an **intent** before it starts and an **outcome** when it ends. The conversation is **checkpointed** at model-turn boundaries: every turn, or every `checkpoint_every_turns` from the profile, and when the session pauses. A run recovers from the latest checkpoint and replays the records after it:

- **A model call without an outcome** is made again, from the same conversation and with the same intent ID, which the gateway uses as an idempotency key. A call is never continued mid-answer.
- **A tool call without an outcome** follows its tool's policy:
  - *run again*: read-only tools repeat the call;
  - *reconcile*: writes and edits record the file's hash before and after in the intent. If the file now has the new content, the call is done; if it still has the old content, it runs again; if it has anything else, the outcome is unknown;
  - *never*: a shell command or Python script may have done anything. It isn't run again, and the model gets `outcome_unknown` as the call's result, so that it checks before repeating it.

## Pausing and stopping

- `pause drain` lets the running tool or model call finish, records it, and starts nothing new.
- `pause interrupt` stops it: a command's process group gets `SIGTERM`, and a model call is abandoned (and made again with the same intent ID on resume). The model is told a command was interrupted and may have run partly.
- `stop` interrupts, then kills after the grace period.

The next run carries on from where the session paused: remaining tool calls run, then the next model call.

Steer notes are recorded and published, but not shown to the model yet.

## Trying it

`actlet-devgw -model <script>` answers model calls from a script of turns ([`sdk/scripted`](../sdk/scripted)), so you can run the agent without a real model:

```sh
make build
mkdir -p agent-session/inputs && cp examples/agent/numbers.txt agent-session/inputs/
bin/actlet-devgw -token dev -task examples/agent/task.json -model examples/agent/model.yaml &
ACTLET_GATEWAY=127.0.0.1:7400 ACTLET_TOKEN=dev bin/actlet run -session ./agent-session
cat agent-session/output/report.md
```

The gateway prints each model turn, tool call and diff. Each scripted turn can check the tool results it receives (`expect`), and wait before answering (`delay`) to try pausing or crashing during a model call.
