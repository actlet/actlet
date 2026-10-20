package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/actlet/actlet/internal/sessionfs"
	"github.com/actlet/actlet/internal/tools"
)

// PromptVersion identifies BasePrompt. It is recorded in checkpoints, so a
// session's prompt can be traced to the Actlet release that wrote it.
const PromptVersion = "actlet-base-v0.1"

// BasePrompt is Actlet's own layer of the agent's system prompt: how the
// box works. The implementor's instructions, the tools manifest and the
// task come after it.
const BasePrompt = `You are an agent running inside Actlet, an isolated container that works on one task at a time. Work on the task until it is done, using your tools.

## The session folder

- workspace/ is your working directory. Relative paths in tools are relative to it.
- ../inputs/ holds the task's input files. It is read-only.
- ../output/ is for deliverables: whatever the task asks you to produce goes there. Only output/ is collected when the task ends.
- ../task.json is the task definition, also shown below.

## Tools

You call tools to read and change files, search, and run programs. The container is the toolbox: use the shell to find out what is installed (which, --help) and use it directly. Shell commands and Python scripts run in workspace/, one call at a time, with a timeout.

A tool result marked outcome_unknown means the worker stopped while that call was running, so it may or may not have taken effect. It was not run again. Check its effects, for example by reading files, before you repeat it.

## Working rules

- Read before you change: read a file before editing it, and copy text exactly for edits.
- Prefer small, checkable steps; run what you write.
- Don't ask questions; nobody answers during the task. Make reasonable choices and say what you assumed.
- When the task is done, write the deliverables to ../output/ and answer with a short summary, without calling tools. That ends the task.
`

// Prompt are the layers of the agent's prompt.
type Prompt struct {
	// Base replaces BasePrompt when set.
	Base string
	// Instructions are the implementor's.
	Instructions string
	// Manifests describe what is installed: from the image, then the
	// profile.
	Manifests []string
}

// system builds the system prompt.
func (p Prompt) system(ts *tools.Set) (text, version string) {
	var sb strings.Builder
	version = PromptVersion
	if p.Base != "" {
		sb.WriteString(strings.TrimSpace(p.Base))
		version = "custom"
	} else {
		sb.WriteString(strings.TrimSpace(BasePrompt))
	}
	if s := strings.TrimSpace(p.Instructions); s != "" {
		sb.WriteString("\n\n## Instructions\n\n")
		sb.WriteString(s)
	}
	var notes []string
	for _, m := range p.Manifests {
		if s := strings.TrimSpace(m); s != "" {
			notes = append(notes, s)
		}
	}
	notes = append(notes, ts.Notes()...)
	if len(notes) > 0 {
		sb.WriteString("\n\n## What is installed\n\n")
		sb.WriteString(strings.Join(notes, "\n\n"))
	}
	return sb.String(), version
}

// taskMessage describes the task to the model.
func taskMessage(t *sessionfs.Task, inputs []string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# Task %s\n\n", t.ID)
	var s string
	if json.Unmarshal(t.Input, &s) == nil {
		sb.WriteString(strings.TrimSpace(s))
	} else if len(bytes.TrimSpace(t.Input)) > 0 && string(t.Input) != "null" {
		var pretty bytes.Buffer
		if json.Indent(&pretty, t.Input, "", "  ") != nil {
			pretty.Reset()
			pretty.Write(t.Input)
		}
		sb.WriteString("The task's input:\n\n```json\n")
		sb.WriteString(pretty.String())
		sb.WriteString("\n```")
	} else {
		sb.WriteString("The task has no input text; see the input files.")
	}
	if len(inputs) > 0 {
		sb.WriteString("\n\nInput files in ../inputs/:\n")
		for _, f := range inputs {
			fmt.Fprintf(&sb, "- %s\n", f)
		}
	}
	return sb.String()
}
