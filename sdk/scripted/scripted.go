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
//	  - text: The sum is 3.     # no tool calls: the task ends
//
// The turn played is chosen by the number of model answers already in the
// conversation, so a repeated call gets the same turn. A call that doesn't
// match the script (its tool results, or the continuation of the previous
// turn) fails with codes.FailedPrecondition, which ends the worker's
// session with the reason.
package scripted

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	Turns []Turn `json:"turns"`
}

// Turn is one answer of the model.
type Turn struct {
	// Expect checks the tool results the turn receives, in order.
	Expect []Expect `json:"expect,omitempty"`
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
	if len(s.Turns) == 0 {
		return nil, errors.New("script: no turns")
	}
	for i, t := range s.Turns {
		for j, c := range t.ToolCalls {
			if c.Name == "" {
				return nil, fmt.Errorf("script: turn %d, tool call %d: no name", i+1, j+1)
			}
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
	m.mu.Lock()
	m.calls = append(m.calls, Call{Intent: req.GetIntentId(), Turn: n + 1, Time: time.Now()})
	m.mu.Unlock()
	mismatch := func(format string, args ...any) error {
		return status.Errorf(codes.FailedPrecondition, "scripted model, turn %d: %s", n+1, fmt.Sprintf(format, args...))
	}
	if n >= len(m.script.Turns) {
		return mismatch("the script has only %d turns", len(m.script.Turns))
	}
	turn := m.script.Turns[n]
	if n > 0 && !bytes.Equal(req.GetContinuation(), continuation(n)) {
		return mismatch("continuation %q, want %q", req.GetContinuation(), continuation(n))
	}
	if err := checkResults(turn.Expect, req.GetMessages()); err != nil {
		return mismatch("%v", err)
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
		input := []byte("{}")
		if c.Input != nil {
			var err error
			if input, err = json.Marshal(c.Input); err != nil {
				return status.Errorf(codes.Internal, "scripted model: %v", err)
			}
		}
		if err := call.ToolCall(fmt.Sprintf("call_%d_%d", n+1, i+1), c.Name, string(input)); err != nil {
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
