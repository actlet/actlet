// Package agent runs the root agent of a session: it builds the prompt,
// calls the model through the gateway, runs the tools the model asks for,
// and loops until the model ends the task.
//
// Every model call and tool call is journaled as an intent before it
// starts and an outcome when it ends, and the whole conversation is
// checkpointed at model-turn boundaries. A run recovers from the latest
// checkpoint and the records after it: an interrupted model call is made
// again with the same intent ID, and a tool call with an intent but no
// outcome is handled by its tool's recovery policy, so that nothing with
// side effects runs twice.
package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/actlet/actlet/internal/agent/agentpb"
	"github.com/actlet/actlet/internal/crashpoint"
	"github.com/actlet/actlet/internal/gwclient"
	"github.com/actlet/actlet/internal/journal"
	"github.com/actlet/actlet/internal/sessionfs"
	"github.com/actlet/actlet/internal/tools"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// Kind is the task kind the agent runs.
const Kind = "agent"

// Journal record kinds.
const (
	KindCheckpoint       = "agent.checkpoint"
	KindInferenceIntent  = "agent.inference.intent"
	KindInferenceOutcome = "agent.inference.outcome"
	KindToolIntent       = "tool.intent"
	KindToolOutcome      = "tool.outcome"
)

// IsRecord reports whether a journal record kind is the agent's.
func IsRecord(kind string) bool {
	return strings.HasPrefix(kind, "agent.") || strings.HasPrefix(kind, "tool.")
}

// Limits.
const (
	defaultMaxTurns = 100
	maxEventText    = 16 << 10 // model text in activity
	maxEventIO      = 4 << 10  // tool input and output in activity
	maxManifest     = 64 << 10
)

// Settings configure the agent, from the profile.
type Settings struct {
	// Model is the model alias to call.
	Model  string
	Prompt Prompt
	// CheckpointEvery is the number of model turns between checkpoints.
	// 0 means every turn.
	CheckpointEvery uint64
	// Tools selects and configures the tools; the agent sets the paths.
	Tools tools.Options
}

// InferFunc makes a model call through the gateway.
type InferFunc func(ctx context.Context, req *gatewayv0.InferenceRequest) (*gwclient.Inference, error)

// Env is what a run of the agent works with.
type Env struct {
	Settings  Settings
	Folder    sessionfs.Folder
	Task      *sessionfs.Task
	SessionID string
	Epoch     uint64
	// Records are the agent's journal records from the latest checkpoint
	// on, from earlier runs of the session.
	Records []journal.Record
	// Record writes a record to the session journal.
	Record func(ctx context.Context, kind string, m proto.Message) error
	// Emit publishes an activity event. It must not block.
	Emit  func(*gatewayv0.ActivityEvent)
	Infer InferFunc
	// Soft ends when running work should stop: an interrupting pause, or
	// a stop. Hard ends when it must end at once.
	Soft, Hard context.Context
	// WantsStop reports whether new work must not start: a pause or stop
	// was requested.
	WantsStop func() bool
	Logger    *slog.Logger
}

// definition is an agent task's definition in task.json.
type definition struct {
	// MaxTurns bounds the model turns. Default 100.
	MaxTurns uint64 `json:"max_turns"`
	// MaxOutputTokens bounds each answer; 0 lets the gateway choose.
	MaxOutputTokens uint32 `json:"max_output_tokens"`
}

// ErrRefused is returned when the model refuses the task.
var ErrRefused = errors.New("the model refused to continue")

// state is the agent's execution state.
type state struct {
	turn         uint64
	model        string
	system       string
	version      string
	messages     []*gatewayv0.Message
	continuation []byte
	usage        *gatewayv0.Usage
	checkpointed uint64 // the turn of the latest checkpoint

	// inference is a model call with an intent but no outcome.
	inference *agentpb.InferenceIntent
	// pending is the latest answer while its tool calls run, with the
	// results so far and the calls that have an intent but no result.
	pending *gatewayv0.Message
	results map[string]*agentpb.ToolOutcome
	intents map[string]*agentpb.ToolIntent
	// ended: the latest answer ended the task.
	ended   bool
	refused bool
}

func (s *state) checkpoint() *agentpb.Checkpoint {
	return &agentpb.Checkpoint{
		Turn: s.turn, Model: s.model, System: s.system, Messages: s.messages,
		Continuation: s.continuation, Usage: s.usage, PromptVersion: s.version,
	}
}

func (s *state) restore(cp *agentpb.Checkpoint) {
	*s = state{
		turn: cp.GetTurn(), model: cp.GetModel(), system: cp.GetSystem(), version: cp.GetPromptVersion(),
		messages: cp.GetMessages(), continuation: cp.GetContinuation(), usage: cp.GetUsage(), checkpointed: cp.GetTurn(),
	}
	if s.usage == nil {
		s.usage = &gatewayv0.Usage{}
	}
}

// answered applies the model's answer.
func (s *state) answered(o *agentpb.InferenceOutcome) {
	s.messages = append(s.messages, o.GetMessage())
	s.continuation = o.GetDone().GetContinuation()
	u := o.GetDone().GetUsage()
	s.usage.InputTokens += u.GetInputTokens()
	s.usage.OutputTokens += u.GetOutputTokens()
	s.usage.CacheReadTokens += u.GetCacheReadTokens()
	s.usage.CacheWriteTokens += u.GetCacheWriteTokens()
	s.turn++
	s.inference = nil
	switch {
	case len(toolCalls(o.GetMessage())) > 0:
		s.pending = o.GetMessage()
		s.results = map[string]*agentpb.ToolOutcome{}
		s.intents = map[string]*agentpb.ToolIntent{}
	case o.GetDone().GetStopReason() == gatewayv0.StopReason_STOP_REASON_MAX_TOKENS:
		s.messages = append(s.messages, userText("Your answer reached the output limit and was cut off. Continue where it stopped."))
	case o.GetDone().GetStopReason() == gatewayv0.StopReason_STOP_REASON_REFUSAL:
		s.refused = true
	default:
		s.ended = true
	}
}

// flush adds the results of the pending answer's tool calls to the
// conversation, in the order of the calls.
func (s *state) flush() {
	if s.pending == nil {
		return
	}
	msg := &gatewayv0.Message{Role: gatewayv0.Role_ROLE_USER}
	for _, c := range toolCalls(s.pending) {
		r := s.results[c.GetId()]
		msg.Parts = append(msg.Parts, &gatewayv0.Part{Part: &gatewayv0.Part_ToolResult{ToolResult: &gatewayv0.ToolResult{
			CallId: c.GetId(), Content: r.GetContent(), IsError: r.GetIsError(),
		}}})
	}
	s.messages = append(s.messages, msg)
	s.pending, s.results, s.intents = nil, nil, nil
}

// replay rebuilds the state from the latest checkpoint and the records
// after it.
func (s *state) replay(records []journal.Record) error {
	for _, r := range records {
		var m proto.Message
		switch r.Kind {
		case KindCheckpoint:
			m = &agentpb.Checkpoint{}
		case KindInferenceIntent:
			m = &agentpb.InferenceIntent{}
		case KindInferenceOutcome:
			m = &agentpb.InferenceOutcome{}
		case KindToolIntent:
			m = &agentpb.ToolIntent{}
		case KindToolOutcome:
			m = &agentpb.ToolOutcome{}
		default:
			continue
		}
		if err := proto.Unmarshal(r.Data, m); err != nil {
			return fmt.Errorf("journal record %d (%s): %w", r.Seq, r.Kind, err)
		}
		switch m := m.(type) {
		case *agentpb.Checkpoint:
			s.restore(m)
		case *agentpb.InferenceIntent:
			s.flush()
			s.inference = m
		case *agentpb.InferenceOutcome:
			s.answered(m)
		case *agentpb.ToolIntent:
			if s.pending != nil {
				s.intents[m.GetCallId()] = m
			}
		case *agentpb.ToolOutcome:
			if s.pending != nil {
				s.results[m.GetCallId()] = m
				delete(s.intents, m.GetCallId())
			}
		}
	}
	return nil
}

// agent is one run of the agent.
type agent struct {
	env   Env
	log   *slog.Logger
	def   definition
	tools *tools.Set
	st    state
	specs []*gatewayv0.ToolSpec
}

// Run runs the agent until the model ends the task, which it reports as
// done, or a pause or stop takes effect. An error fails the session.
func Run(env Env) (done bool, err error) {
	a := &agent{env: env, log: env.Logger}
	if a.log == nil {
		a.log = slog.Default()
	}
	if err := decodeStrict(env.Task.Definition, &a.def); err != nil {
		return false, fmt.Errorf("task definition: %w", err)
	}
	if a.def.MaxTurns == 0 {
		a.def.MaxTurns = defaultMaxTurns
	}
	topts := env.Settings.Tools
	topts.Workspace = env.Folder.Workspace()
	topts.ReadOnly = []string{env.Folder.Inputs(), env.Folder.State(), env.Folder.TaskFile()}
	topts.TempDir = filepath.Join(env.Folder.State(), "tmp")
	if err := os.MkdirAll(topts.TempDir, 0o700); err != nil {
		return false, err
	}
	if a.tools, err = tools.New(topts); err != nil {
		return false, err
	}
	for _, sp := range a.tools.Specs() {
		a.specs = append(a.specs, &gatewayv0.ToolSpec{Name: sp.Name, Description: sp.Description, InputSchema: string(sp.Schema)})
	}

	if len(env.Records) > 0 {
		if err := a.st.replay(env.Records); err != nil {
			return false, err
		}
		a.text("resuming the agent at turn %d", a.st.turn)
	} else if err := a.start(); err != nil {
		return false, err
	}
	return a.loop()
}

// start builds the prompt and records it as the first checkpoint.
func (a *agent) start() error {
	sys, version := a.env.Settings.Prompt.system(a.tools)
	a.st = state{
		model: a.env.Settings.Model, system: sys, version: version, usage: &gatewayv0.Usage{},
		messages: []*gatewayv0.Message{userText(taskMessage(a.env.Task, listInputs(a.env.Folder.Inputs())))},
	}
	return a.record(KindCheckpoint, a.st.checkpoint())
}

func (a *agent) loop() (bool, error) {
	for {
		if a.st.pending != nil {
			for _, call := range toolCalls(a.st.pending) {
				if a.st.results[call.GetId()] != nil {
					continue
				}
				if a.env.WantsStop() {
					return false, nil
				}
				if err := a.runTool(call); err != nil {
					return false, err
				}
			}
			a.st.flush()
			if a.st.turn-a.st.checkpointed >= max(a.env.Settings.CheckpointEvery, 1) {
				if err := a.saveCheckpoint(); err != nil {
					return false, err
				}
			}
		}
		switch {
		case a.st.refused:
			return false, ErrRefused
		case a.st.ended:
			return true, a.finish()
		case a.env.WantsStop():
			// At a turn boundary: checkpoint what a cadence above one left
			// unrecorded.
			if a.st.turn > a.st.checkpointed {
				if err := a.saveCheckpoint(); err != nil {
					return false, err
				}
			}
			return false, nil
		case a.st.turn >= a.def.MaxTurns:
			return false, fmt.Errorf("the task used all %d model turns without finishing", a.def.MaxTurns)
		}
		if stopped, err := a.inference(); stopped || err != nil {
			return false, err
		}
	}
}

// inference makes the next model call, or makes again the one an earlier
// run didn't finish.
func (a *agent) inference() (stopped bool, err error) {
	intent := a.st.inference
	if intent == nil {
		intent = &agentpb.InferenceIntent{IntentId: newID(), Turn: a.st.turn + 1}
		if err := a.record(KindInferenceIntent, intent); err != nil {
			return false, err
		}
		a.st.inference = intent
	} else {
		a.text("making model call %d again after an interruption (intent %s)", intent.GetTurn(), intent.GetIntentId())
	}
	crashpoint.Hit("inference.intent")
	req := &gatewayv0.InferenceRequest{
		SessionId: a.env.SessionID, Epoch: a.env.Epoch, IntentId: intent.GetIntentId(), Model: a.st.model,
		System: a.st.system, Messages: a.st.messages, Tools: a.specs, Continuation: a.st.continuation,
		MaxOutputTokens: a.def.MaxOutputTokens,
	}
	ans, err := a.env.Infer(a.env.Soft, req)
	if err != nil {
		if a.env.Soft.Err() != nil {
			// Interrupted: the intent stays open, and the next run makes
			// the call again.
			return true, nil
		}
		return false, err
	}
	outcome := &agentpb.InferenceOutcome{IntentId: intent.GetIntentId(), Message: ans.Message, Done: ans.Done, Attempts: uint32(ans.Attempts)}
	if err := a.record(KindInferenceOutcome, outcome); err != nil {
		return false, err
	}
	a.st.answered(outcome)
	var names []string
	for _, c := range toolCalls(ans.Message) {
		names = append(names, c.GetName())
	}
	a.env.Emit(&gatewayv0.ActivityEvent{Body: &gatewayv0.ActivityEvent_ModelTurn{ModelTurn: &gatewayv0.ModelTurn{
		Turn: a.st.turn, Model: a.st.model, Text: cut(messageText(ans.Message), maxEventText), ToolCalls: names,
		StopReason: ans.Done.GetStopReason(), Usage: ans.Done.GetUsage(),
	}}})
	return false, nil
}

// runTool runs one tool call of the pending answer and records its
// outcome. A call with an intent from an earlier run is recovered by its
// tool's policy instead.
func (a *agent) runTool(call *gatewayv0.ToolCall) error {
	input := json.RawMessage(call.GetInput())
	outcome := &agentpb.ToolOutcome{CallId: call.GetId()}
	var res tools.Result
	start := time.Now()

	if prev := a.st.intents[call.GetId()]; prev != nil {
		outcome.IntentId = prev.GetIntentId()
		rec := a.tools.Recover(call.GetName(), input, prev.GetCheck())
		if !rec.Rerun {
			outcome.Recovered = true
			res = rec.Result
			a.text("recovered %s call %s: %s", call.GetName(), call.GetId(), firstLine(res.Output))
		} else {
			a.text("running %s call %s again: it is safe to repeat", call.GetName(), call.GetId())
			res = a.execute(call, input, nil)
		}
	} else {
		res = a.execute(call, input, outcome)
	}

	outcome.Content, outcome.IsError, outcome.OutcomeUnknown = res.Output, res.IsError, res.Unknown
	crashpoint.Hit("tool.done:" + call.GetName())
	if err := a.record(KindToolOutcome, outcome); err != nil {
		return err
	}
	a.st.results[call.GetId()] = outcome
	delete(a.st.intents, call.GetId())
	for _, ch := range res.Changes {
		a.env.Emit(&gatewayv0.ActivityEvent{Body: &gatewayv0.ActivityEvent_FileChange{FileChange: &gatewayv0.FileChange{
			CallId: call.GetId(), Path: ch.Path, Created: ch.Created, Diff: cut(ch.Diff, maxEventText),
		}}})
	}
	a.env.Emit(&gatewayv0.ActivityEvent{Body: &gatewayv0.ActivityEvent_ToolFinished{ToolFinished: &gatewayv0.ToolFinished{
		CallId: call.GetId(), Tool: call.GetName(), IsError: res.IsError, OutcomeUnknown: res.Unknown,
		Output: cut(res.Output, maxEventIO), Took: durationpb.New(time.Since(start)),
	}}})
	return nil
}

// execute prepares and runs a call. With a new outcome, it first records
// the intent; with nil, the call already has one.
func (a *agent) execute(call *gatewayv0.ToolCall, input json.RawMessage, outcome *agentpb.ToolOutcome) tools.Result {
	a.env.Emit(&gatewayv0.ActivityEvent{Body: &gatewayv0.ActivityEvent_ToolStarted{ToolStarted: &gatewayv0.ToolStarted{
		CallId: call.GetId(), Tool: call.GetName(), Input: cut(call.GetInput(), maxEventIO),
	}}})
	c, err := a.tools.Prepare(call.GetName(), input)
	if err != nil {
		// Refused before running: nothing to recover, no intent needed.
		return tools.Result{Output: err.Error(), IsError: true}
	}
	if outcome != nil {
		intent := &agentpb.ToolIntent{
			IntentId: newID(), CallId: call.GetId(), Tool: call.GetName(), Input: call.GetInput(),
			Check: c.Check, Policy: a.tools.Policy(call.GetName()).String(),
		}
		if err := a.record(KindToolIntent, intent); err != nil {
			return tools.Result{Output: "couldn't record the call, so it didn't run: " + err.Error(), IsError: true}
		}
		outcome.IntentId = intent.GetIntentId()
		crashpoint.Hit("tool.intent:" + call.GetName())
	}
	return c.Run(tools.RunContext{Soft: a.env.Soft, Hard: a.env.Hard})
}

func (a *agent) saveCheckpoint() error {
	cp := a.st.checkpoint()
	if proto.Size(cp) > journal.DefaultLimits().MaxRecordData-1024 {
		// Recovery replays from the previous checkpoint instead.
		a.log.Warn("agent: the conversation is too large to checkpoint", "turn", a.st.turn, "bytes", proto.Size(cp))
		return nil
	}
	if err := a.record(KindCheckpoint, cp); err != nil {
		return err
	}
	a.st.checkpointed = a.st.turn
	return nil
}

// finish makes sure the task has a deliverable: if the model left
// output/ empty, its final answer is written to output/result.md.
func (a *agent) finish() error {
	out := a.env.Folder.Output()
	entries, err := os.ReadDir(out)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		final := ""
		if n := len(a.st.messages); n > 0 {
			final = messageText(a.st.messages[n-1])
		}
		if err := sessionfs.WriteFileAtomic(filepath.Join(out, "result.md"), []byte(strings.TrimSpace(final)+"\n"), 0o644); err != nil {
			return fmt.Errorf("writing the deliverable: %w", err)
		}
	}
	a.text("task completed after %d model turns; deliverables: %s", a.st.turn, strings.Join(listFiles(out), ", "))
	return nil
}

func (a *agent) record(kind string, m proto.Message) error {
	if err := a.env.Record(context.Background(), kind, m); err != nil {
		return fmt.Errorf("recording %s: %w", kind, err)
	}
	return nil
}

func (a *agent) text(format string, args ...any) {
	a.env.Emit(&gatewayv0.ActivityEvent{Body: &gatewayv0.ActivityEvent_Text{Text: &gatewayv0.Text{Text: fmt.Sprintf(format, args...)}}})
}

func userText(s string) *gatewayv0.Message {
	return &gatewayv0.Message{Role: gatewayv0.Role_ROLE_USER, Parts: []*gatewayv0.Part{{Part: &gatewayv0.Part_Text{Text: s}}}}
}

func toolCalls(m *gatewayv0.Message) []*gatewayv0.ToolCall {
	var calls []*gatewayv0.ToolCall
	for _, p := range m.GetParts() {
		if c := p.GetToolCall(); c != nil {
			calls = append(calls, c)
		}
	}
	return calls
}

func messageText(m *gatewayv0.Message) string {
	var parts []string
	for _, p := range m.GetParts() {
		if t := p.GetText(); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n")
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func cut(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	for limit > 0 && s[limit]&0xC0 == 0x80 {
		limit--
	}
	return s[:limit] + " [cut]"
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// listInputs lists the input files, relative to inputs/, up to 200.
func listInputs(dir string) []string {
	files := listFiles(dir)
	if len(files) > 200 {
		files = append(files[:200], fmt.Sprintf("... and %d more", len(files)-200))
	}
	return files
}

// listFiles lists the files under dir, relative to it, sorted.
func listFiles(dir string) []string {
	var files []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if rel, err := filepath.Rel(dir, p); err == nil {
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(files)
	return files
}

// decodeStrict decodes JSON, rejecting unknown fields. Empty is allowed.
func decodeStrict(raw json.RawMessage, v any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// ReadManifest reads a tools manifest file from the image; a missing file
// is no manifest.
func ReadManifest(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxManifest))
	return string(data), err
}
