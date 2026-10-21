# Profiles

A **profile** is what the orchestrator lets a session do: limits, how it pauses and stops, which features it needs from the worker, its budgets, its models and tools, and what the agent is told. The gateway hands one to every worker when it registers. The worker applies it within the ceilings of its own deployment (`actlet run -h` lists them) and records the effective configuration in the session journal.

The schema is versioned (`schema: v0`) and **experimental**. The SDK reads it from Go structs or from JSON or YAML files; all three describe the same fields.

## Example

```yaml
schema: v0                      # required
id: coding-default              # required: letters, digits and . _ : / -
version: "3"
required_features:              # the worker must advertise all of these
  - control.pause
  - control.stop
limits:
  max_workers: 2                # workers running at once; 0 or missing: the deployment's ceiling
  max_tool_concurrency: 4       # tool calls of one turn running at once
  max_subagent_depth: 2         # how deep subagents nest; 0 or missing: 2
  max_subagents: 8              # subagents a session creates; 0 or missing: 8
default_pause_mode: interrupt   # drain (default) or interrupt: pausing on SIGTERM
stop_grace: 45s                 # grace period of a stop that gives none

budgets:
  - kind: time                  # time, money, tokens or disk
    limit: 3600                 # seconds, currency units, tokens or bytes
    alert_at_percent: 80
    on_exhaust: wait            # kill or wait
    wait_mode: pause            # with wait: pause (persist and exit) or idle
  - kind: money
    limit: 12.5
    currency: EUR
    on_exhaust: kill
models:
  aliases:                      # names the orchestrator resolves: real models or tiers
    - alias: large
      capabilities: [tools, vision]
      context_window: 200000    # tokens; viewers show how full it is
    - alias: fast
  roles:                        # which alias each role uses
    main: large
    subagent: fast
    llm: fast
  selectable: [fast]            # aliases the agent may choose itself, such as for a subagent
tools:                          # the tools to offer; omit for all the local ones
  - name: shell
    settings:
      timeout: 5m
  - name: report_progress       # standard tools are offered only when named
  - name: decide
instructions: |                 # the implementor's layer of the agent's prompt
  Follow the repository's CONTRIBUTING.md.
tools_manifest: |               # what is installed, for the agent
  Go 1.27, Node 22 and jq are installed.
checkpoint_every_turns: 1       # model turns between checkpoints
skills:                         # skill folders sent with the profile, besides the image's
  - dir: skills/csv-report      # with a SKILL.md; relative to the profile file; 2 MiB in all
compaction:                     # when the context nears the model's window; on by default
  strategy: summarize           # summarize (default) or off
  threshold_percent: 80         # of the alias's context_window; 0 or missing: 80
  model: fast                   # writes the summary; missing: the compaction role, or the worker's model
  keep_turns: 4                 # recent model turns kept as they are; 0 or missing: 4
local_attach:                   # what a viewer attached to the box, or by default through the orchestrator, may do
  commands: [steer]             # steer, pause and stop; [none] allows nothing
tui:                            # how terminal viewers show the session, unless they choose otherwise
  separators: false             # lines between turns and tool calls; on unless false
  theme: light                  # dark, light, plain, or a theme file's name in the viewer's themes folder
```

The same document as JSON has the same field names. Durations are strings such as `"30s"` in both formats.

## Rules

Loading is strict, so a mistake is reported rather than ignored:

- Unknown fields are rejected, at any level, and so are duplicate keys in YAML.
- `schema` must be `v0`.
- At most one budget of each kind. A `money` budget needs a three-letter `currency`, and the other kinds must not have one.
- `wait_mode` is required with `on_exhaust: wait` and not allowed with `kill`.
- Every role must point to a defined alias. Aliases and tool names are unique.
- `tui.theme` is a name, of letters, digits, `-` and `_`, never a path.

All problems are reported at once.

### Skills in a profile

`skills` sends skill folders with the profile: every file in each folder, at most 2 MiB in all, with whether it is executable, which the worker keeps when it writes the folder into the session. A file is executable by its mode where files have execute bits. On Windows, which has none, a file is sent as executable when it starts with `#!`, as a script does, so a profile loaded there still gives the container scripts it can run. See [skills](agent.md#skills) for how the agent uses them.

### Required features

`required_features` names capabilities the worker announces when it registers. Today those are `control.pause`, `control.stop`, `control.steer`, `activity.v0`, `task.demo`, `task.agent`, `inference.v0`, `tools.local` and `attach.files`. A gateway built on the SDK refuses to register a worker that lacks any of them, and the worker refuses such a profile itself too. An older worker therefore never runs a profile it only half understands: it exits with code 1 and names the missing features.

## Budgets

A budget limits what the session uses over all its runs:

| Kind | Counts | Unit |
|---|---|---|
| `time` | wall-clock time since the session first started, paused time included; a reset counts from the reset | seconds |
| `money` | the cost the gateway reports with each model call and bridged tool call, in the budget's currency; costs in another currency don't count | currency units |
| `tokens` | the models' input and output tokens, the agent's, its subagents' and the `llm` tool's | tokens |
| `disk` | the session folder's size, looked at every 15 seconds and shortly after each tool call | bytes |

At `alert_at_percent`, the agent is told once, in a notice before its next model call, how much it has used and what happens when the budget runs out, so it can plan to finish. When the budget runs out, `on_exhaust` decides:

- `kill` stops the session, with the profile's `stop_grace`; it exits 4.
- `wait` with `wait_mode: pause` pauses it; it exits 3. A new run with the same budget pauses again at once; once the orchestrator raises the budget, a new run carries on.
- `wait` with `wait_mode: idle` keeps the session running but holds model calls and bridged tool calls back until a [profile update](#updating-a-running-session) raises or resets the budget. Running tool calls finish.

The time budget counts from the start of the session's first run, which every run records in the session journal, so a paused session's clock keeps running. A session resumed after a long pause can therefore be past its time budget at once; the orchestrator raises or resets the budget with a profile update when it wants the session to carry on. A forked session counts from its own first run.

Usage is checked as it is reported, before budget-consuming work starts, so a budget can be overshot by the one call that crosses it. Alerts and limits reached are recorded in the session journal before Actlet acts on them, and `budget_status` activity events report the budgets at the start of a run, at alerts and limits, and when they change. Viewers show each budget's use; the agent can ask with the `budget_status` tool.

## Updating a running session

The orchestrator can change some of a running session's profile with a `profile_update` command, `Session.UpdateProfile` in the SDK, or `profile-update <file>` in `actlet-devgw`. It names the fields it changes, and only these may change: `limits.max_workers`, `limits.max_tool_concurrency`, `limits.max_subagents`, `budgets` and `local_attach`. An update naming any other field, such as `instructions`, is refused as a whole. It can also reset budgets, which then count from zero again, such as at the start of a billing period (not `disk`, which is the folder's size).

```yaml
# profile-update.yaml: a partial profile; the fields present change.
limits:
  max_workers: 4
budgets:
  - {kind: money, limit: 5, currency: EUR, on_exhaust: wait, wait_mode: idle}
reset_budgets: [money]
```

The worker records the new configuration in the session journal before it takes effect. The worker limit and the tool concurrency apply as workers and tool calls start, within the deployment's ceilings. When the box registers again, such as to resume after a pause, the SDK applies its updates to the profile `Assign` returns, so the box starts with them; resets aren't applied again.

## Using profiles from Go

```go
// From a file.
p, err := sdk.LoadProfile("profiles/coding.yaml") // .json, .yaml or .yml

// Or in code.
p := &sdk.Profile{Schema: sdk.ProfileSchema, ID: "coding", StopGrace: sdk.Duration(45 * time.Second)}

wire, err := p.Proto() // validates, then converts for RegisterResponse.Profile
```

`actlet-devgw --profile profiles/coding.yaml` assigns a profile file to every session it serves.

## Models

Workers never see provider names, endpoints or credentials. They see only the aliases the orchestrator defines, and which alias each role (`main`, `subagent`, `llm`, `compaction`) uses; the `llm` tool uses the `llm` role, or `processing`. An alias's `context_window` lets workers [compact](agent.md#context-compaction) their context before it fills. The orchestrator resolves an alias when the worker asks for a model. The agent uses the `main` role's alias, or `main` when the profile has no models. Subagents use the `subagent` role's alias, or the agent's; the agent may pick one of `selectable` for a subagent instead. `selectable` must name defined aliases.

## The agent

`instructions`, `base_prompt` (which replaces Actlet's own) and `tools_manifest` make up the agent's prompt, and `tools` selects and configures its tools, local and [standard](agent.md#standard-tools). See [the agent](agent.md).
