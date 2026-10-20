// Package agent runs a session's agent: the root worker, which works on
// the task, and the subagents it spawns, which work on subtasks in
// parallel. Each worker builds its prompt, calls the model through the
// gateway, runs the tools the model asks for, and loops until the model
// ends its task.
//
// Every model call and tool call is journaled as an intent before it
// starts and an outcome when it ends, and the state of all workers is
// checkpointed at model-turn boundaries. A run recovers from the latest
// checkpoint and the records after it: an interrupted model call is made
// again with the same intent ID, a tool call with an intent but no
// outcome is handled by its tool's recovery policy, and a subagent's
// result reaches its parent exactly once.
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
	"slices"
	"sort"
	"strings"
	"sync"

	"google.golang.org/protobuf/proto"

	"github.com/actlet/actlet/internal/agent/agentpb"
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
	KindSpawn            = "agent.spawn"
	KindWorkerDone       = "agent.worker.done"
	KindSteer            = "agent.steer"
)

// RootWorker is the ID of the session's own agent. Subagents are named
// after their parent: "root.1", "root.1.2" and so on.
const RootWorker = "root"

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

// Defaults of the subagent limits.
const (
	DefaultMaxSubagentDepth = 2
	DefaultMaxSubagents     = 8
)

// Settings configure the agent, from the profile.
type Settings struct {
	// Model is the root worker's model alias; SubagentModel is the
	// default for subagents.
	Model, SubagentModel string
	// Selectable are the aliases the agent may choose for a subagent.
	Selectable []string
	Prompt     Prompt
	// CheckpointEvery is the number of model turns between checkpoints.
	// 0 means every turn.
	CheckpointEvery uint64
	// MaxWorkers bounds the workers running at once, and
	// MaxToolConcurrency the tool calls of one turn running at once. 0
	// means 1.
	MaxWorkers, MaxToolConcurrency int
	// MaxSubagentDepth bounds how deep subagents nest, and MaxSubagents
	// how many a session creates. 0 means none.
	MaxSubagentDepth, MaxSubagents int
	// Tools selects and configures the tools; the agent sets the paths.
	Tools tools.Options
	// Standard are the standard tools the profile grants, with their
	// settings.
	Standard []tools.Grant
}

// InferFunc makes a model call through the gateway.
type InferFunc func(ctx context.Context, req *gatewayv0.InferenceRequest) (*gwclient.Inference, error)

// Steer is an operator's steer note.
type Steer struct {
	CommandID string
	Note      string
	// From names who sent it.
	From string
}

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
	// Stopping is closed when new work must not start: a pause or stop
	// was requested.
	Stopping <-chan struct{}
	// Steers returns the operator's accepted steer notes so far, in the
	// order they arrived. Nil means none.
	Steers func() []Steer
	Logger *slog.Logger
}

// definition is an agent task's definition in task.json.
type definition struct {
	// MaxTurns bounds the model turns of each worker. Default 100.
	MaxTurns uint64 `json:"max_turns"`
	// MaxOutputTokens bounds each answer; 0 lets the gateway choose.
	MaxOutputTokens uint32 `json:"max_output_tokens"`
}

// ErrRefused is returned when the model refuses the task.
var ErrRefused = errors.New("the model refused to continue")

// errStopped ends a worker for a pause or stop.
var errStopped = errors.New("stopped")

// box is the agent of a session: its workers and what they share.
type box struct {
	env      Env
	log      *slog.Logger
	def      definition
	tools    *tools.Set
	standard map[string]*standardTool
	locks    *tools.Locks
	slots    chan struct{} // running workers

	mu         sync.Mutex
	st         *stateSet
	turns      uint64 // model turns since the latest checkpoint
	running    map[string]*worker
	wg         sync.WaitGroup
	failed     error // a journal write failed: everything stops
	stopAll    chan struct{}
	stopAllSet bool
}

// worker is a running worker.
type worker struct {
	b    *box
	id   string
	done chan struct{} // closed when its goroutine ends
	err  error         // why it ended, once done is closed
	slot bool          // it holds a slot in b.slots
}

// Run runs the agent until the root worker ends the task, which it
// reports as done, or a pause or stop takes effect. An error fails the
// session.
func Run(env Env) (done bool, err error) {
	b := &box{env: env, log: env.Logger, st: newStateSet(), running: map[string]*worker{}, locks: tools.NewLocks(), stopAll: make(chan struct{})}
	if b.log == nil {
		b.log = slog.Default()
	}
	if err := decodeStrict(env.Task.Definition, &b.def); err != nil {
		return false, fmt.Errorf("task definition: %w", err)
	}
	if b.def.MaxTurns == 0 {
		b.def.MaxTurns = defaultMaxTurns
	}
	b.slots = make(chan struct{}, max(env.Settings.MaxWorkers, 1))
	topts := toolOptions(env.Settings, env.Folder)
	if err := os.MkdirAll(topts.TempDir, 0o700); err != nil {
		return false, err
	}
	if b.tools, err = tools.New(topts); err != nil {
		return false, err
	}
	b.standard = b.standardTools()

	if len(env.Records) > 0 {
		for _, r := range env.Records {
			m, err := decode(r)
			if err != nil {
				return false, err
			}
			if m == nil {
				continue
			}
			if err := b.st.apply(m); err != nil {
				return false, fmt.Errorf("journal record %d (%s): %w", r.Seq, r.Kind, err)
			}
			switch m.(type) {
			case *agentpb.Checkpoint:
				b.turns = 0
			case *agentpb.InferenceOutcome:
				b.turns++
			}
		}
		if b.st.workers[RootWorker] == nil {
			return false, errors.New("the journal's agent records have no root worker")
		}
		b.text(RootWorker, "resuming the agent: %d workers, root at turn %d", len(b.st.workers), b.st.workers[RootWorker].GetTurn())
	} else if err := b.start(); err != nil {
		return false, err
	}

	// Continue every unfinished worker; parents wait for their subagents.
	b.mu.Lock()
	var root *worker
	for _, id := range b.st.order {
		if w := b.st.workers[id]; w.GetDone() == nil {
			rw := b.launchLocked(id)
			if id == RootWorker {
				root = rw
			}
		}
	}
	b.mu.Unlock()
	<-root.done
	b.wg.Wait()
	b.mu.Lock()
	failed := b.failed
	b.mu.Unlock()
	switch {
	case failed != nil:
		return false, failed
	case errors.Is(root.err, errStopped):
		return false, nil
	case root.err != nil:
		return false, root.err
	}
	return true, nil
}

// toolOptions are the tools' options for a session folder.
func toolOptions(s Settings, f sessionfs.Folder) tools.Options {
	o := s.Tools
	o.Workspace = f.Workspace()
	o.ReadOnly = []string{f.Inputs(), f.State(), f.TaskFile()}
	o.TempDir = filepath.Join(f.State(), "tmp")
	return o
}

// Tools describes the tools the root worker gets with these settings,
// and notes on how they were set up, for viewers.
func Tools(s Settings, f sessionfs.Folder) ([]*gatewayv0.ToolInfo, []string, error) {
	ts, err := tools.New(toolOptions(s, f))
	if err != nil {
		return nil, nil, err
	}
	var out []*gatewayv0.ToolInfo
	for _, sp := range ts.Specs() {
		out = append(out, &gatewayv0.ToolInfo{Name: sp.Name, Policy: sp.Policy.String(), Description: sp.Description})
	}
	if s.MaxSubagentDepth > 0 && s.MaxSubagents > 0 {
		out = append(out, &gatewayv0.ToolInfo{Name: SpawnTool, Policy: "spawn", Description: "start a subagent on a subtask"})
	}
	b := &box{env: Env{Settings: s}}
	for name, t := range b.standardTools() {
		out = append(out, &gatewayv0.ToolInfo{Name: name, Policy: t.policy, Description: firstLine(t.spec.GetDescription())})
	}
	slices.SortFunc(out, func(a, b *gatewayv0.ToolInfo) int { return strings.Compare(a.GetName(), b.GetName()) })
	return out, ts.Notes(), nil
}

// start builds the root worker's prompt and records it as the first
// checkpoint.
func (b *box) start() error {
	sys, version := b.env.Settings.Prompt.system(b.tools, false)
	b.st.add(&agentpb.WorkerState{
		WorkerId: RootWorker, Model: b.env.Settings.Model, System: sys, PromptVersion: version,
		Messages: []*gatewayv0.Message{userText(taskMessage(b.env.Task, listInputs(b.env.Folder.Inputs())))},
	})
	return b.checkpoint()
}

// launchLocked starts a worker's goroutine. The caller holds b.mu.
func (b *box) launchLocked(id string) *worker {
	w := &worker{b: b, id: id, done: make(chan struct{})}
	b.running[id] = w
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer close(w.done)
		defer func() {
			if r := recover(); r != nil {
				w.err = fmt.Errorf("worker %s panicked: %v", id, r)
				b.log.Error("agent: worker panicked", "worker", id, "panic", r)
			}
		}()
		w.err = w.run()
		if w.err != nil && !errors.Is(w.err, errStopped) && id == RootWorker {
			b.stopEverything()
		}
	}()
	return w
}

// stopEverything makes every worker stop, as a pause does, when the root
// worker failed or the journal can't be written.
func (b *box) stopEverything() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.stopAllSet {
		b.stopAllSet = true
		close(b.stopAll)
	}
}

// stopping reports whether new work must not start.
func (b *box) stopping() bool {
	select {
	case <-b.env.Stopping:
		return true
	case <-b.stopAll:
		return true
	default:
		return false
	}
}

// commit records m in the journal and applies it to the state, as one
// step. Nothing is applied if the record can't be written; the run then
// stops, since the journal no longer matches the work.
func (b *box) commit(m proto.Message) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.commitLocked(m)
}

func (b *box) commitLocked(m proto.Message) error {
	if b.failed != nil {
		return b.failed
	}
	if err := b.env.Record(context.Background(), kindOf(m), m); err != nil {
		b.failed = fmt.Errorf("recording %s: %w", kindOf(m), err)
		if !b.stopAllSet {
			b.stopAllSet = true
			close(b.stopAll)
		}
		return b.failed
	}
	if err := b.st.apply(m); err != nil {
		return err
	}
	if _, ok := m.(*agentpb.InferenceOutcome); ok {
		b.turns++
	}
	return nil
}

func kindOf(m proto.Message) string {
	switch m.(type) {
	case *agentpb.Checkpoint:
		return KindCheckpoint
	case *agentpb.InferenceIntent:
		return KindInferenceIntent
	case *agentpb.InferenceOutcome:
		return KindInferenceOutcome
	case *agentpb.ToolIntent:
		return KindToolIntent
	case *agentpb.ToolOutcome:
		return KindToolOutcome
	case *agentpb.Spawn:
		return KindSpawn
	case *agentpb.WorkerDone:
		return KindWorkerDone
	case *agentpb.SteerDelivered:
		return KindSteer
	}
	panic(fmt.Sprintf("agent: no record kind for %T", m))
}

// checkpoint records the state of every worker.
func (b *box) checkpoint() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failed != nil {
		return b.failed
	}
	cp := b.st.checkpoint()
	if size := proto.Size(cp); size > journal.DefaultLimits().MaxRecordData-1024 {
		// Recovery replays from the previous checkpoint instead.
		b.log.Warn("agent: the workers' state is too large to checkpoint", "bytes", size)
		return nil
	}
	if err := b.env.Record(context.Background(), KindCheckpoint, cp); err != nil {
		b.failed = fmt.Errorf("recording a checkpoint: %w", err)
		return b.failed
	}
	b.turns = 0
	return nil
}

// maybeCheckpoint checkpoints when enough model turns have passed.
func (b *box) maybeCheckpoint() error {
	b.mu.Lock()
	due := b.turns >= max(b.env.Settings.CheckpointEvery, 1)
	b.mu.Unlock()
	if due {
		return b.checkpoint()
	}
	return nil
}

// view returns a copy of a worker's state, to read without the lock.
func (b *box) view(id string) *agentpb.WorkerState {
	b.mu.Lock()
	defer b.mu.Unlock()
	return proto.Clone(b.st.workers[id]).(*agentpb.WorkerState)
}

func (b *box) emit(worker string, ev *gatewayv0.ActivityEvent) {
	ev.Worker = worker
	b.env.Emit(ev)
}

func (b *box) text(worker, format string, args ...any) {
	b.emit(worker, &gatewayv0.ActivityEvent{Body: &gatewayv0.ActivityEvent_Text{Text: &gatewayv0.Text{Text: fmt.Sprintf(format, args...)}}})
}

// run is a worker's loop: tool calls, then the next model call, until its
// task ends or a pause or stop takes effect.
func (w *worker) run() error {
	b := w.b
	if err := w.acquireSlot(); err != nil {
		return err
	}
	defer w.releaseSlot()
	for {
		st := b.view(w.id)
		if st.GetPending() != nil {
			if err := w.runTools(st); err != nil {
				return err
			}
			if err := b.maybeCheckpoint(); err != nil {
				return err
			}
			continue
		}
		switch {
		case st.GetRefused():
			return w.end("", ErrRefused)
		case st.GetEnded():
			return w.end(messageText(lastMessage(st)), nil)
		case b.stopping():
			return w.pauseCheckpoint()
		case st.GetTurn() >= b.def.MaxTurns:
			return w.end("", fmt.Errorf("the task used all %d model turns without finishing", b.def.MaxTurns))
		}
		if w.id == RootWorker {
			if err := w.deliverSteers(); err != nil {
				return err
			}
		}
		if err := w.inference(); err != nil {
			return err
		}
		if err := b.maybeCheckpoint(); err != nil {
			return err
		}
	}
}

// pauseCheckpoint records what a cadence above one left unrecorded, then
// ends the worker for the pause or stop.
func (w *worker) pauseCheckpoint() error {
	if w.id == RootWorker {
		w.b.mu.Lock()
		due := w.b.turns > 0
		w.b.mu.Unlock()
		if due {
			if err := w.b.checkpoint(); err != nil {
				return err
			}
		}
	}
	return errStopped
}

// end ends the worker's task: the root writes the deliverable; a subagent
// records its result for its parent.
func (w *worker) end(final string, failure error) error {
	b := w.b
	if w.id == RootWorker {
		if failure != nil {
			return failure
		}
		return b.finish(final)
	}
	done := &agentpb.WorkerDone{WorkerId: w.id, Result: strings.TrimSpace(final)}
	if failure != nil {
		done.IsError, done.Result = true, "the subagent failed: "+failure.Error()
	}
	if done.Result == "" {
		done.Result = "(the subagent finished without a final answer)"
	}
	if err := b.commit(done); err != nil {
		return err
	}
	if crashHit("worker.done", w.id) {
		return errStopped
	}
	st := b.view(w.id)
	b.emit(w.id, &gatewayv0.ActivityEvent{Body: &gatewayv0.ActivityEvent_SubagentFinished{SubagentFinished: &gatewayv0.SubagentFinished{
		IsError: done.GetIsError(), Result: cut(done.GetResult(), maxEventText), Turns: st.GetTurn(),
	}}})
	return nil
}

// finish makes sure the task has a deliverable: if the model left
// output/ empty, its final answer is written to output/result.md.
func (b *box) finish(final string) error {
	out := b.env.Folder.Output()
	entries, err := os.ReadDir(out)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		if err := sessionfs.WriteFileAtomic(filepath.Join(out, "result.md"), []byte(strings.TrimSpace(final)+"\n"), 0o644); err != nil {
			return fmt.Errorf("writing the deliverable: %w", err)
		}
	}
	b.text(RootWorker, "task completed after %d model turns; deliverables: %s", b.view(RootWorker).GetTurn(), strings.Join(listFiles(out), ", "))
	return nil
}

func (w *worker) acquireSlot() error {
	select {
	case w.b.slots <- struct{}{}:
		w.slot = true
		return nil
	case <-w.b.env.Stopping:
		return errStopped
	case <-w.b.stopAll:
		return errStopped
	}
}

func (w *worker) releaseSlot() {
	if w.slot {
		w.slot = false
		<-w.b.slots
	}
}

// deliverSteers gives the root worker the operator's steer notes it
// hasn't seen, in the message before its next model call.
func (w *worker) deliverSteers() error {
	b := w.b
	if b.env.Steers == nil {
		return nil
	}
	for _, s := range b.env.Steers() {
		b.mu.Lock()
		seen := b.st.delivered[s.CommandID]
		b.mu.Unlock()
		if seen {
			continue
		}
		from := s.From
		if from == "" {
			from = "the operator"
		}
		text := fmt.Sprintf("[Operator note from %s, sent while you worked; take it into account] %s", from, s.Note)
		if err := b.commit(&agentpb.SteerDelivered{WorkerId: w.id, CommandId: s.CommandID, Text: text}); err != nil {
			return err
		}
		b.text(w.id, "the agent got the note from %s", from)
	}
	return nil
}

// inference makes the worker's next model call, or makes again the one an
// earlier run didn't finish.
func (w *worker) inference() error {
	b := w.b
	st := b.view(w.id)
	in := st.GetInference()
	if in == nil {
		in = &agentpb.InferenceIntent{IntentId: newID(), Turn: st.GetTurn() + 1, WorkerId: w.id}
		if err := b.commit(in); err != nil {
			return err
		}
	} else {
		b.text(w.id, "making model call %d again after an interruption (intent %s)", in.GetTurn(), in.GetIntentId())
	}
	if crashHit("inference.intent", w.id) {
		return errStopped
	}
	req := &gatewayv0.InferenceRequest{
		SessionId: b.env.SessionID, Epoch: b.env.Epoch, IntentId: in.GetIntentId(), Model: st.GetModel(),
		System: st.GetSystem(), Messages: slices.Clone(st.GetMessages()), Tools: b.specs(st.GetDepth()),
		Continuation: st.GetContinuation(), MaxOutputTokens: b.def.MaxOutputTokens,
	}
	ans, err := b.env.Infer(b.env.Soft, req)
	if err != nil {
		if b.env.Soft.Err() != nil {
			// Interrupted: the intent stays open, and the next run makes
			// the call again.
			return errStopped
		}
		return w.end("", err)
	}
	outcome := &agentpb.InferenceOutcome{IntentId: in.GetIntentId(), Message: ans.Message, Done: ans.Done, Attempts: uint32(ans.Attempts), WorkerId: w.id}
	if err := b.commit(outcome); err != nil {
		return err
	}
	var names []string
	for _, c := range toolCalls(ans.Message) {
		names = append(names, c.GetName())
	}
	b.emit(w.id, &gatewayv0.ActivityEvent{Body: &gatewayv0.ActivityEvent_ModelTurn{ModelTurn: &gatewayv0.ModelTurn{
		Turn: st.GetTurn() + 1, Model: st.GetModel(), Text: cut(messageText(ans.Message), maxEventText), ToolCalls: names,
		StopReason: ans.Done.GetStopReason(), Usage: ans.Done.GetUsage(),
	}}})
	return nil
}

func lastMessage(st *agentpb.WorkerState) *gatewayv0.Message {
	if n := len(st.GetMessages()); n > 0 {
		return st.GetMessages()[n-1]
	}
	return nil
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
