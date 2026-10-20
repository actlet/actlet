// Package scripted is a model that plays a script, for testing agents
// deterministically without a real model: each call is answered with the
// next turn of a YAML or JSON script.
//
// A script is a list of turns:
//
//	turns:
//	  - text: I'll read the input first.
//	    tool_calls:
//	      - name: read_file
//	        input: {path: ../inputs/numbers.txt}
//	  - expect:                  # the tool results this turn receives
//	      - contains: "1\t3"
//	    text: Now I'll add them up.
//	    tool_calls:
//	      - name: python
//	        input: {code: "print(1 + 2)"}
//	    delay: 1s                # answer after a pause
//	    expect_once: [metric]    # texts the conversation has exactly once
//	  - text: The sum is 3.     # no tool calls: the task ends
//	subagents:                   # scripts for subagents, by their subtask
//	  - match: count the words   # a phrase in the subtask
//	    turns:
//	      - text: 42 words.
//
// The turn played is chosen by the number of model answers already in the
// conversation, so a repeated call gets the same turn. A conversation
// whose first message contains a subagent script's phrase plays that
// script instead. A call that doesn't match the script (its tool results,
// or the continuation of the previous turn) fails with
// codes.FailedPrecondition, which ends the worker's task with the reason.
package scripted

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"sigs.k8s.io/yaml"

	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
	"github.com/actlet/actlet/sdk"
)

// Script is the turns the model plays.
type Script struct {
	Turns     []Turn      `json:"turns"`
	Subagents []Subscript `json:"subagents,omitempty"`
}

// Subscript is the script of the subagents whose subtask contains Match.
type Subscript struct {
	Match string `json:"match"`
	Turns []Turn `json:"turns"`
}

// Turn is one answer of the model.
type Turn struct {
	// Expect checks the tool results the turn receives, in order.
	Expect []Expect `json:"expect,omitempty"`
	// ExpectOnce are texts that must each appear exactly once in the
	// conversation's text, such as an operator's note.
	ExpectOnce []string `json:"expect_once,omitempty"`
	// Text is the model's text.
	Text string `json:"text,omitempty"`
	// ToolCalls are the tools the model calls. A turn without any ends the
	// task.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// Delay holds the answer back, to try pausing or crashing during a
	// model call.
	Delay sdk.Duration `json:"delay,omitempty"`
}

// ToolCall is a tool the model calls.
type ToolCall struct {
	Name  string         `json:"name"`
	Input map[string]any `json:"input,omitempty"`
}

// Expect is a check on one tool result.
type Expect struct {
	// Contains must occur in the result.
	Contains string `json:"contains,omitempty"`
	// IsError, if set, must match whether the call failed.
	IsError *bool `json:"is_error,omitempty"`
}

// Call is a call the model answered.
type Call struct {
	Intent string
	Turn   int
	Time   time.Time
	// Script is the subagent script's phrase, or "" for the main script.
	Script string
}

// Model plays a script. Use Serve as sdk.Options.OnInference.
type Model struct {
	script Script

	mu    sync.Mutex
	calls []Call
}

// Parse reads a script in YAML or JSON. Unknown fields are errors.
func Parse(data []byte) (*Model, error) {
	j, err := yaml.YAMLToJSONStrict(data)
	if err != nil {
		return nil, fmt.Errorf("script: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(j))
	dec.DisallowUnknownFields()
	var s Script
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("script: %w", err)
	}
	check := func(name string, turns []Turn) error {
		if len(turns) == 0 {
			return fmt.Errorf("script: %s: no turns", name)
		}
		for i, t := range turns {
			for j, c := range t.ToolCalls {
				if c.Name == "" {
					return fmt.Errorf("script: %s, turn %d, tool call %d: no name", name, i+1, j+1)
				}
			}
		}
		return nil
	}
	if err := check("main", s.Turns); err != nil {
		return nil, err
	}
	for i, sub := range s.Subagents {
		if sub.Match == "" {
			return nil, fmt.Errorf("script: subagent script %d: no match", i+1)
		}
		if err := check(fmt.Sprintf("subagent %q", sub.Match), sub.Turns); err != nil {
			return nil, err
		}
	}
	return &Model{script: s}, nil
}

// Load reads a script file.
func Load(path string) (*Model, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Calls returns the calls answered so far.
func (m *Model) Calls() []Call {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Call(nil), m.calls...)
}

func continuation(turn int) []byte { return []byte("scripted:" + strconv.Itoa(turn)) }

// Serve answers a call with the script's turn for it.
func (m *Model) Serve(ctx context.Context, call *sdk.InferenceCall) error {
	req := call.Request
	n := 0
	for _, msg := range req.GetMessages() {
		if msg.GetRole() == gatewayv0.Role_ROLE_ASSISTANT {
			n++
		}
	}
	turns, script := m.script.Turns, ""
	if msgs := req.GetMessages(); len(msgs) > 0 {
		var first strings.Builder
		for _, p := range msgs[0].GetParts() {
			first.WriteString(p.GetText())
		}
		for _, sub := range m.script.Subagents {
			if strings.Contains(first.String(), sub.Match) {
				turns, script = sub.Turns, sub.Match
				break
			}
		}
	}
	m.mu.Lock()
	m.calls = append(m.calls, Call{Intent: req.GetIntentId(), Turn: n + 1, Time: time.Now(), Script: script})
	m.mu.Unlock()
	name := "main script"
	if script != "" {
		name = fmt.Sprintf("subagent script %q", script)
	}
	mismatch := func(format string, args ...any) error {
		return status.Errorf(codes.FailedPrecondition, "scripted model, %s, turn %d: %s", name, n+1, fmt.Sprintf(format, args...))
	}
	if n >= len(turns) {
		return mismatch("the script has only %d turns", len(turns))
	}
	turn := turns[n]
	if n > 0 && !bytes.Equal(req.GetContinuation(), continuation(n)) {
		return mismatch("continuation %q, want %q", req.GetContinuation(), continuation(n))
	}
	if err := checkResults(turn.Expect, req.GetMessages()); err != nil {
		return mismatch("%v", err)
	}
	for _, want := range turn.ExpectOnce {
		n := 0
		for _, msg := range req.GetMessages() {
			for _, p := range msg.GetParts() {
				n += strings.Count(p.GetText(), want)
			}
		}
		if n != 1 {
			return mismatch("%q appears %d times in the conversation, the script expects once", want, n)
		}
	}
	if d := time.Duration(turn.Delay); d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	for _, w := range strings.SplitAfter(turn.Text, " ") {
		if w == "" {
			continue
		}
		if err := call.Text(w); err != nil {
			return err
		}
	}
	for i, c := range turn.ToolCalls {
		input := "{}"
		if c.Input != nil {
			var buf bytes.Buffer
			enc := json.NewEncoder(&buf)
			enc.SetEscapeHTML(false)
			if err := enc.Encode(c.Input); err != nil {
				return status.Errorf(codes.Internal, "scripted model: %v", err)
			}
			input = strings.TrimSuffix(buf.String(), "\n")
		}
		if err := call.ToolCall(fmt.Sprintf("call_%d_%d", n+1, i+1), c.Name, input); err != nil {
			return err
		}
	}
	stop := gatewayv0.StopReason_STOP_REASON_END_TURN
	if len(turn.ToolCalls) > 0 {
		stop = gatewayv0.StopReason_STOP_REASON_TOOL_USE
	}
	in := len(req.GetSystem())
	for _, msg := range req.GetMessages() {
		for _, p := range msg.GetParts() {
			in += len(p.GetText()) + len(p.GetToolResult().GetContent()) + len(p.GetToolCall().GetInput())
		}
	}
	return call.Done(&gatewayv0.InferenceDone{
		StopReason:   stop,
		Usage:        &gatewayv0.Usage{InputTokens: uint64(in/4 + 1), OutputTokens: uint64(len(turn.Text)/4 + 1)},
		Continuation: continuation(n + 1),
	})
}

// checkResults checks the tool results in the last message against the
// expectations.
func checkResults(expect []Expect, msgs []*gatewayv0.Message) error {
	if len(expect) == 0 {
		return nil
	}
	var results []*gatewayv0.ToolResult
	if len(msgs) > 0 {
		for _, p := range msgs[len(msgs)-1].GetParts() {
			if r := p.GetToolResult(); r != nil {
				results = append(results, r)
			}
		}
	}
	if len(results) != len(expect) {
		return fmt.Errorf("got %d tool results, the script expects %d", len(results), len(expect))
	}
	for i, e := range expect {
		r := results[i]
		if !strings.Contains(r.GetContent(), e.Contains) {
			return fmt.Errorf("tool result %d doesn't contain %q:\n%s", i+1, e.Contains, r.GetContent())
		}
		if e.IsError != nil && *e.IsError != r.GetIsError() {
			return fmt.Errorf("tool result %d: is_error is %v, the script expects %v:\n%s", i+1, r.GetIsError(), *e.IsError, r.GetContent())
		}
	}
	return nil
}
