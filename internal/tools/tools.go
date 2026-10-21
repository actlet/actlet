// Package tools implements the agent's local tools: reading, writing and
// editing files, finding files and searching them, running shell commands
// and Python scripts.
//
// Every call has two steps, so that the agent can journal its intent in
// between: Prepare validates the input and plans the call, and Run carries
// it out. Each tool declares what to do with a call that was interrupted by
// a crash after its intent was recorded but before its outcome was (see
// Policy).
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
)

// Policy says what recovery does with a call that has an intent but no
// outcome in the journal.
type Policy int

const (
	// PolicyRetry: the call has no side effects; run it again.
	PolicyRetry Policy = iota
	// PolicyReconcile: the intent records what the call was going to
	// change; recovery checks whether the change happened and runs the
	// call again only if it didn't.
	PolicyReconcile
	// PolicyNever: the call may have had any effect. It is never run
	// again; the model is told its outcome is unknown.
	PolicyNever
)

func (p Policy) String() string {
	switch p {
	case PolicyRetry:
		return "retry"
	case PolicyReconcile:
		return "reconcile"
	case PolicyNever:
		return "never"
	}
	return fmt.Sprintf("Policy(%d)", int(p))
}

// Result is what a call did.
type Result struct {
	// Output is what the model is told.
	Output string
	// IsError marks a failed call.
	IsError bool
	// Unknown marks a call whose outcome is unknown after a crash.
	Unknown bool
	// Changes are the files the call changed, for activity.
	Changes []Change
}

// Change is a file a tool changed.
type Change struct {
	Path string
	// Diff is a unified diff of the change.
	Diff    string
	Created bool
}

func errorResult(format string, args ...any) Result {
	return Result{Output: fmt.Sprintf(format, args...), IsError: true}
}

// fsError reports a file system error to the agent in the same words on
// every operating system, naming the file as the agent knows it rather
// than by its path on the host.
func (e *Env) fsError(err error) Result {
	reason := func(err error) string {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return "no such file or directory"
		case errors.Is(err, fs.ErrPermission):
			return "permission denied"
		case errors.Is(err, fs.ErrExist):
			return "file already exists"
		}
		return err.Error()
	}
	var pe *fs.PathError
	var le *os.LinkError
	switch {
	case errors.As(err, &pe):
		return errorResult("%s %s: %s", pe.Op, e.display(pe.Path), reason(pe.Err))
	case errors.As(err, &le):
		return errorResult("%s %s: %s", le.Op, e.display(le.New), reason(le.Err))
	}
	return errorResult("%v", err)
}

// UnknownOutcome is the result of a call that can't be recovered.
func UnknownOutcome(tool string) Result {
	return Result{
		Output: "outcome_unknown: the worker stopped while this " + tool + " call was running, so whether it finished, " +
			"and what it changed, is unknown. It was not run again. Check its effects before repeating it.",
		IsError: true, Unknown: true,
	}
}

// Spec describes a tool to the model.
type Spec struct {
	Name        string
	Description string
	// Schema is the JSON schema of the tool's input.
	Schema json.RawMessage
	Policy Policy
}

// Call is a prepared call.
type Call struct {
	// Check is recovery data to journal with the intent, for
	// PolicyReconcile tools.
	Check []byte
	run   func(ctx RunContext) Result
	// The paths the call writes, or everything; see Locks.
	writes    []string
	writesAll bool
}

// Writes describes what the call writes: everything, or the listed paths.
// A call that writes nothing is read-only.
func (c *Call) Writes() (all bool, paths []string) { return c.writesAll, c.writes }

// RunContext tells a running call when to stop.
type RunContext struct {
	// Soft ends when the call should stop gracefully: a pause or stop.
	Soft context.Context
	// Hard ends when the call must end at once.
	Hard context.Context
}

// Run carries out the call.
func (c *Call) Run(ctx RunContext) Result { return c.run(ctx) }

// Recovery is what recovery decided about an interrupted call.
type Recovery struct {
	// Rerun: run the call again.
	Rerun bool
	// Result is the call's result when it isn't run again.
	Result Result
}

type tool struct {
	spec    Spec
	prepare func(in json.RawMessage) (*Call, error)
	// reconcile decides about an interrupted PolicyReconcile call.
	reconcile func(in json.RawMessage, check []byte) Recovery
	// writes says what a call writes, from its input alone. Nil writes
	// nothing.
	writes func(in json.RawMessage) (all bool, paths []string)
}

// Set is the tools available to a session.
type Set struct {
	env   *Env
	tools map[string]*tool
	notes []string
}

// Specs returns the tools' descriptions, sorted by name.
func (s *Set) Specs() []Spec {
	out := make([]Spec, 0, len(s.tools))
	for _, t := range s.tools {
		out = append(out, t.spec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Notes describe how the tools were set up, such as a missing program
// replaced by a fallback.
func (s *Set) Notes() []string { return s.notes }

// Has reports whether the set has the named tool.
func (s *Set) Has(name string) bool { return s.tools[name] != nil }

// Policy returns a tool's recovery policy.
func (s *Set) Policy(name string) Policy {
	if t := s.tools[name]; t != nil {
		return t.spec.Policy
	}
	return PolicyRetry // an unknown tool is refused again
}

// ErrUnknownTool is returned by Prepare for a tool not in the set.
var ErrUnknownTool = errors.New("unknown tool")

// Prepare validates a call's input and plans it. An error is meant for the
// model.
func (s *Set) Prepare(name string, input json.RawMessage) (*Call, error) {
	t := s.tools[name]
	if t == nil {
		return nil, fmt.Errorf("%w %q; the tools are: %s", ErrUnknownTool, name, strings.Join(s.names(), ", "))
	}
	return t.prepare(input)
}

// Writes describes what a call with this input would write, for Locks,
// without planning it: a call that can't be planned yet, such as an edit
// of a file an earlier call creates, still waits for the calls it
// conflicts with. Input that can't be read is taken to write everything.
func (s *Set) Writes(name string, input json.RawMessage) *Call {
	t := s.tools[name]
	if t == nil || t.writes == nil {
		return &Call{}
	}
	all, paths := t.writes(input)
	return &Call{writesAll: all, writes: paths}
}

// Recover decides what to do with a call whose intent was journaled but
// whose outcome wasn't.
func (s *Set) Recover(name string, input json.RawMessage, check []byte) Recovery {
	t := s.tools[name]
	if t == nil {
		return Recovery{Rerun: true}
	}
	switch t.spec.Policy {
	case PolicyRetry:
		return Recovery{Rerun: true}
	case PolicyReconcile:
		if t.reconcile != nil {
			return t.reconcile(input, check)
		}
	}
	return Recovery{Result: UnknownOutcome(name)}
}

func (s *Set) names() []string {
	var names []string
	for n := range s.tools {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// decode decodes a tool's JSON input strictly.
func decode(in json.RawMessage, v any) error {
	if len(bytes.TrimSpace(in)) == 0 {
		in = json.RawMessage("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(in))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid input: %w", err)
	}
	if dec.More() {
		return errors.New("invalid input: trailing data after the JSON object")
	}
	return nil
}

// truncate keeps the start and end of s within max bytes.
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	head := limit * 2 / 3
	tail := limit - head
	return fmt.Sprintf("%s\n[... %d bytes omitted ...]\n%s", validPrefix(s[:head]), len(s)-head-tail, validSuffix(s[len(s)-tail:]))
}

// validPrefix drops a UTF-8 sequence cut at the end of s.
func validPrefix(s string) string {
	for i := 0; i < 3 && len(s) > 0 && !utf8Complete(s); i++ {
		s = s[:len(s)-1]
	}
	return s
}

// validSuffix drops continuation bytes at the start of s.
func validSuffix(s string) string {
	for i := 0; i < 3 && len(s) > 0 && s[0]&0xC0 == 0x80; i++ {
		s = s[1:]
	}
	return s
}

func utf8Complete(s string) bool {
	// Find the start of the last sequence and check it is whole.
	for i := len(s) - 1; i >= 0 && i >= len(s)-4; i-- {
		c := s[i]
		if c&0xC0 == 0x80 {
			continue
		}
		need := 1
		switch {
		case c >= 0xF0:
			need = 4
		case c >= 0xE0:
			need = 3
		case c >= 0xC0:
			need = 2
		}
		return len(s)-i >= need
	}
	return true
}
