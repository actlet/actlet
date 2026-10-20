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
//	    usage: {input: 1200, output: 80}  # tokens to report
//	  - text: The sum is 3.     # no tool calls: the task ends
//	price: {currency: EUR, input: 3, output: 15}  # per million tokens
//	subagents:                   # scripts for subagents, by their subtask
//	  - match: count the words   # a phrase in the subtask
//	    turns:
//	      - text: 42 words.
//	decisions:                   # answers to decide calls, by their question
//	  - match: deploy            # a phrase in the question
//	    decision: yes
//	    reason: the tests pass
//	    delay: 2s                # decide after a pause
//	llm:                         # answers to the llm tool, by its prompt
//	  - match: summarise
//	    text: It's about sales.
//	    usage: {input: 300, output: 20}
//	    cost: 0.001              # what the call cost, in the price's currency
//	summary:                     # the answer to compaction calls
//	  text: Read the input; the sum is 3.
//	  usage: {input: 900, output: 40}
//
// The turn played is chosen by the number of model answers already in the
// conversation, or, once a compaction summarised some, by the previous
// turn's continuation, so a repeated call gets the same turn. A conversation
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
	// Price, if set, makes each call report a cost.
	Price *Price `json:"price,omitempty"`
	// Decisions answer decide calls; LLM answers the llm tool's calls.
	Decisions []Decision  `json:"decisions,omitempty"`
	LLM       []LLMAnswer `json:"llm,omitempty"`
	// Summary answers compaction calls; by default with a short text.
	Summary *LLMAnswer `json:"summary,omitempty"`
}

// Decision answers the decide calls whose question contains Match.
type Decision struct {
	Match     string       `json:"match"`
	Decision  string       `json:"decision"`
	Reason    string       `json:"reason,omitempty"`
	DecidedBy string       `json:"decided_by,omitempty"`
	Delay     sdk.Duration `json:"delay,omitempty"`
}

// LLMAnswer answers the llm tool's calls whose prompt contains Match.
type LLMAnswer struct {
	Match string     `json:"match"`
	Text  string     `json:"text"`
	Usage *TurnUsage `json:"usage,omitempty"`
	// Cost, if set, is what the call cost, in the price's currency.
	Cost *float64 `json:"cost,omitempty"`
}

// Price is what tokens cost, per million.
type Price struct {
	Currency string  `json:"currency"`
	Input    float64 `json:"input"`
	Output   float64 `json:"output"`
}

// TurnUsage sets the tokens a turn reports.
type TurnUsage struct {
	Input  uint64 `json:"input"`
	Output uint64 `json:"output"`
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
	// Usage sets the tokens the turn reports; by default they follow the
	// conversation's length.
	Usage *TurnUsage `json:"usage,omitempty"`
	// Cost, if set, is what the turn cost, in the price's currency,
	// instead of what the price makes of its tokens.
	Cost *float64 `json:"cost,omitempty"`
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

	mu      sync.Mutex
	calls   []Call
	decides []string // the intent IDs of the decide calls answered
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
	priced := func(cost *float64, what string) error {
		if cost != nil && s.Price == nil {
			return fmt.Errorf("script: %s has a cost but the script has no price, for its currency", what)
		}
		return nil
	}
	for i, t := range s.Turns {
		if err := priced(t.Cost, fmt.Sprintf("turn %d", i+1)); err != nil {
			return nil, err
		}
	}
	for i, d := range s.Decisions {
		if d.Match == "" || d.Decision == "" {
			return nil, fmt.Errorf("script: decision %d: match and decision are required", i+1)
		}
	}
	for i, a := range s.LLM {
		if a.Match == "" {
			return nil, fmt.Errorf("script: llm answer %d: no match", i+1)
		}
		if err := priced(a.Cost, fmt.Sprintf("llm answer %d", i+1)); err != nil {
			return nil, err
		}
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

// continuationTurn returns the turn a continuation ended.
func continuationTurn(c []byte) (int, bool) {
	s, ok := strings.CutPrefix(string(c), "scripted:")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

// Decides returns the intent IDs of the decide calls answered so far.
func (m *Model) Decides() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.decides...)
}

// Decide answers a decide call with the script's decision for its
// question. Use it as sdk.Options.OnDecide.
func (m *Model) Decide(ctx context.Context, d *sdk.DecideRequest) (*sdk.Decision, error) {
	for _, sd := range m.script.Decisions {
		if !strings.Contains(d.Question, sd.Match) {
			continue
		}
		if delay := time.Duration(sd.Delay); delay > 0 {
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		m.mu.Lock()
		m.decides = append(m.decides, d.IntentID)
		m.mu.Unlock()
		by := sd.DecidedBy
		if by == "" {
			by = "the script"
		}
		return &sdk.Decision{Decision: sd.Decision, Reason: sd.Reason, DecidedBy: by}, nil
	}
	return nil, status.Errorf(codes.FailedPrecondition, "scripted model: no decision for %q", d.Question)
}

// serveLLM answers an llm tool call with the script's answer for its
// prompt.
func (m *Model) serveLLM(call *sdk.InferenceCall) error {
	req := call.Request
	var prompt strings.Builder
	for _, msg := range req.GetMessages() {
		for _, p := range msg.GetParts() {
			prompt.WriteString(p.GetText())
		}
	}
	for _, a := range m.script.LLM {
		if !strings.Contains(prompt.String(), a.Match) {
			continue
		}
		m.mu.Lock()
		m.calls = append(m.calls, Call{Intent: req.GetIntentId(), Turn: 1, Time: time.Now(), Script: "llm:" + a.Match})
		m.mu.Unlock()
		if err := call.Text(a.Text); err != nil {
			return err
		}
		usage := &gatewayv0.Usage{InputTokens: uint64(prompt.Len()/4 + 1), OutputTokens: uint64(len(a.Text)/4 + 1)}
		if a.Usage != nil {
			usage.InputTokens, usage.OutputTokens = a.Usage.Input, a.Usage.Output
		}
		m.price(usage, a.Cost)
		return call.Done(&gatewayv0.InferenceDone{StopReason: gatewayv0.StopReason_STOP_REASON_END_TURN, Usage: usage})
	}
	return status.Errorf(codes.FailedPrecondition, "scripted model: no llm answer for the prompt %q", prompt.String())
}

// serveSummary answers a compaction call with the script's summary.
func (m *Model) serveSummary(call *sdk.InferenceCall) error {
	a := m.script.Summary
	if a == nil {
		a = &LLMAnswer{Text: "The earlier turns, summarised."}
	}
	in := 0
	for _, msg := range call.Request.GetMessages() {
		for _, p := range msg.GetParts() {
			in += len(p.GetText())
		}
	}
	m.mu.Lock()
	m.calls = append(m.calls, Call{Intent: call.Request.GetIntentId(), Turn: 1, Time: time.Now(), Script: "summary"})
	m.mu.Unlock()
	if err := call.Text(a.Text); err != nil {
		return err
	}
	usage := &gatewayv0.Usage{InputTokens: uint64(in/4 + 1), OutputTokens: uint64(len(a.Text)/4 + 1)}
	if a.Usage != nil {
		usage.InputTokens, usage.OutputTokens = a.Usage.Input, a.Usage.Output
	}
	m.price(usage, a.Cost)
	return call.Done(&gatewayv0.InferenceDone{StopReason: gatewayv0.StopReason_STOP_REASON_END_TURN, Usage: usage})
}

// price sets a call's cost: the given one, or what the price makes of its
// tokens.
func (m *Model) price(usage *gatewayv0.Usage, cost *float64) {
	p := m.script.Price
	if p == nil {
		return
	}
	usage.Currency = p.Currency
	if cost != nil {
		usage.Cost = *cost
		return
	}
	usage.Cost = (float64(usage.InputTokens)*p.Input + float64(usage.OutputTokens)*p.Output) / 1e6
}

// Serve answers a call with the script's turn for it.
func (m *Model) Serve(ctx context.Context, call *sdk.InferenceCall) error {
	switch call.Request.GetPurpose() {
	case "llm":
		return m.serveLLM(call)
	case "compaction":
		return m.serveSummary(call)
	}
	a, err := m.Answer(ctx, call.Request)
	if err != nil {
		return err
	}
	for _, w := range strings.SplitAfter(a.Text, " ") {
		if w == "" {
			continue
		}
		if err := call.Text(w); err != nil {
			return err
		}
	}
	for _, c := range a.ToolCalls {
		if err := call.ToolCall(c.GetId(), c.GetName(), c.GetInput()); err != nil {
			return err
		}
	}
	return call.Done(a.Done)
}

// Answer is a whole answer of the model.
type Answer struct {
	Text      string
	ToolCalls []*gatewayv0.ToolCall
	Done      *gatewayv0.InferenceDone
}

// Answer returns the script's turn for a call, as a whole, such as for a
// mock provider to send in its own format. A call with no continuation
// isn't checked for one, as when a provider stands between.
func (m *Model) Answer(ctx context.Context, req *gatewayv0.InferenceRequest) (*Answer, error) {
	n := 0
	for _, msg := range req.GetMessages() {
		if msg.GetRole() == gatewayv0.Role_ROLE_ASSISTANT {
			n++
		}
	}
	if k, ok := continuationTurn(req.GetContinuation()); ok && k > n {
		n = k // a compaction summarised some answers
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
		return nil, mismatch("the script has only %d turns", len(turns))
	}
	turn := turns[n]
	if n > 0 && req.GetContinuation() != nil && !bytes.Equal(req.GetContinuation(), continuation(n)) {
		return nil, mismatch("continuation %q, want %q", req.GetContinuation(), continuation(n))
	}
	if err := checkResults(turn.Expect, req.GetMessages()); err != nil {
		return nil, mismatch("%v", err)
	}
	for _, want := range turn.ExpectOnce {
		n := 0
		for _, msg := range req.GetMessages() {
			for _, p := range msg.GetParts() {
				n += strings.Count(p.GetText(), want)
			}
		}
		if n != 1 {
			return nil, mismatch("%q appears %d times in the conversation, the script expects once", want, n)
		}
	}
	if d := time.Duration(turn.Delay); d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	a := &Answer{Text: turn.Text}
	for i, c := range turn.ToolCalls {
		input := "{}"
		if c.Input != nil {
			var buf bytes.Buffer
			enc := json.NewEncoder(&buf)
			enc.SetEscapeHTML(false)
			if err := enc.Encode(c.Input); err != nil {
				return nil, status.Errorf(codes.Internal, "scripted model: %v", err)
			}
			input = strings.TrimSuffix(buf.String(), "\n")
		}
		a.ToolCalls = append(a.ToolCalls, &gatewayv0.ToolCall{Id: fmt.Sprintf("call_%d_%d", n+1, i+1), Name: c.Name, Input: input})
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
	usage := &gatewayv0.Usage{InputTokens: uint64(in/4 + 1), OutputTokens: uint64(len(turn.Text)/4 + 1)}
	if turn.Usage != nil {
		usage.InputTokens, usage.OutputTokens = turn.Usage.Input, turn.Usage.Output
	}
	m.price(usage, turn.Cost)
	a.Done = &gatewayv0.InferenceDone{StopReason: stop, Usage: usage, Continuation: continuation(n + 1)}
	return a, nil
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
