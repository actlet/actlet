# Profiles

A **profile** is what the orchestrator lets a session do: limits, how it pauses and stops, which features it needs from the worker, its models and tools, what the agent is told, and later its budgets. The gateway hands one to every worker when it registers. The worker applies it within the ceilings of its own deployment (`actlet run -h` lists them) and records the effective configuration in the session journal.

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

# Schema only for now: workers don't enforce these yet.
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
tools:                          # the local tools to offer; omit for all of them
  - name: shell
    settings:
      timeout: 5m
  - name: decide
    settings:
      route: humans
instructions: |                 # the implementor's layer of the agent's prompt
  Follow the repository's CONTRIBUTING.md.
tools_manifest: |               # what is installed, for the agent
  Go 1.27, Node 22 and jq are installed.
checkpoint_every_turns: 1       # model turns between checkpoints
local_attach:                   # what a viewer attached to the box, or by default through the orchestrator, may do
  commands: [steer]             # steer, pause and stop; [none] allows nothing
```

The same document as JSON has the same field names. Durations are strings such as `"30s"` in both formats.

## Rules

Loading is strict, so a mistake is reported rather than ignored:

- Unknown fields are rejected, at any level, and so are duplicate keys in YAML.
- `schema` must be `v0`.
- At most one budget of each kind. A `money` budget needs a three-letter `currency`, and the other kinds must not have one.
- `wait_mode` is required with `on_exhaust: wait` and not allowed with `kill`.
- Every role must point to a defined alias. Aliases and tool names are unique.

All problems are reported at once.

### Required features

`required_features` names capabilities the worker announces when it registers. Today those are `control.pause`, `control.stop`, `control.steer`, `activity.v0`, `task.demo`, `task.agent`, `inference.v0`, `tools.local` and `attach.files`. A gateway built on the SDK refuses to register a worker that lacks any of them, and the worker refuses such a profile itself too. An older worker therefore never runs a profile it only half understands: it exits with code 1 and names the missing features.

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

Workers never see provider names, endpoints or credentials. They see only the aliases the orchestrator defines, and which alias each role (`main`, `subagent`, `llm`) uses. The orchestrator resolves an alias when the worker asks for a model. The agent uses the `main` role's alias, or `main` when the profile has no models. Subagents use the `subagent` role's alias, or the agent's; the agent may pick one of `selectable` for a subagent instead. `selectable` must name defined aliases.

## The agent

`instructions`, `base_prompt` (which replaces Actlet's own) and `tools_manifest` make up the agent's prompt, and `tools` selects and configures its local tools. See [the agent](agent.md).
