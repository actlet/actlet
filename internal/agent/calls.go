package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/actlet/actlet/internal/agent/agentpb"
	"github.com/actlet/actlet/internal/crashpoint"
	"github.com/actlet/actlet/internal/tools"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// SpawnTool is the name of the tool that starts a subagent.
const SpawnTool = "spawn_subagent"

// crashHook, set by tests, is called at the crash points. When it returns
// true, the worker stops right there, as if the process had died.
var crashHook func(point string) bool

// crashHit is a crash point, also scoped to a worker: "name" and
// "name@worker". It reports whether a test stopped the worker there.
func crashHit(name, worker string) bool {
	crashpoint.Hit(name)
	crashpoint.Hit(name + "@" + worker)
	return crashHook != nil && (crashHook(name) || crashHook(name+"@"+worker))
}

// specs are the tools offered to a worker at a nesting depth.
func (b *box) specs(depth uint32) []*gatewayv0.ToolSpec {
	var out []*gatewayv0.ToolSpec
	for _, sp := range b.tools.Specs() {
		out = append(out, &gatewayv0.ToolSpec{Name: sp.Name, Description: sp.Description, InputSchema: string(sp.Schema)})
	}
	s := b.env.Settings
	if int(depth) < s.MaxSubagentDepth && s.MaxSubagents > 0 {
		out = append(out, b.spawnSpec())
	}
	return out
}

func (b *box) spawnSpec() *gatewayv0.ToolSpec {
	model := `"model":{"type":"string","description":"leave it out to use the default model"}`
	if sel := b.env.Settings.Selectable; len(sel) > 0 {
		names, _ := json.Marshal(sel)
		model = `"model":{"type":"string","enum":` + string(names) + `,"description":"a model to use instead of the default"}`
	}
	return &gatewayv0.ToolSpec{
		Name: SpawnTool,
		Description: "Start a subagent on a self-contained subtask and wait for its final answer, which becomes this call's " +
			"result. Several calls in one turn run their subagents in parallel. Subagents share your workspace, so give " +
			"parallel ones different files to write. Describe the subtask fully: the subagent sees only what you write.",
		InputSchema: `{"type":"object","properties":{` +
			`"task":{"type":"string","description":"the subtask, with everything the subagent needs to know"},` + model + `,` +
			`"budget_share":{"type":"number","minimum":0,"maximum":1,"description":"the share of your budget it may use"}},` +
			`"required":["task"],"additionalProperties":false}`,
	}
}

// runTools runs the pending answer's tool calls that have no outcome yet.
// Calls run in parallel, up to the tool concurrency limit, except that a
// call waits for earlier calls whose writes conflict with its own. A pause
// or stop starts no new call.
func (w *worker) runTools(st *agentpb.WorkerState) error {
	b := w.b
	sem := make(chan struct{}, max(b.env.Settings.MaxToolConcurrency, 1))
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
	}
	// A worker waiting for its subagents gives up its slot, so that they
	// can run.
	spawning := false
	for _, c := range toolCalls(st.GetPending()) {
		if c.GetName() == SpawnTool && result(st, c.GetId()) == nil {
			spawning = true
		}
	}
	if spawning {
		w.releaseSlot()
	}

	for _, call := range toolCalls(st.GetPending()) {
		if result(st, call.GetId()) != nil {
			continue
		}
		if b.stopping() {
			fail(errStopped)
			break
		}
		prev := intent(st, call.GetId())
		if call.GetName() == SpawnTool {
			wg.Go(func() {
				if err := w.spawn(call, prev); err != nil {
					fail(err)
				}
			})
			continue
		}
		// Plan the call now to learn what it writes, and take its place in
		// the lock queue in call order.
		input := json.RawMessage(call.GetInput())
		planned, perr := b.tools.Prepare(call.GetName(), input)
		ticket := b.locks.Enqueue(&tools.Call{})
		if perr == nil {
			ticket = b.locks.Enqueue(planned)
		}
		wg.Go(func() {
			select {
			case sem <- struct{}{}:
			case <-b.env.Stopping:
				return
			case <-b.stopAll:
				return
			}
			defer func() { <-sem }()
			release, err := ticket.Wait(stopContext(b))
			if err != nil {
				return // stopping before it started
			}
			defer release()
			if err := w.runTool(call, prev); err != nil {
				fail(err)
			}
		})
	}
	wg.Wait()
	if spawning {
		if err := w.acquireSlot(); err != nil {
			return err
		}
	}
	if firstErr != nil {
		return firstErr
	}
	if b.stopping() {
		// Some calls never started; the next run starts them.
		if st := b.view(w.id); st.GetPending() != nil {
			return errStopped
		}
	}
	return nil
}

// stopContext ends when the box is stopping.
func stopContext(b *box) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-b.env.Stopping:
		case <-b.stopAll:
		case <-b.env.Hard.Done():
		}
		cancel()
	}()
	return ctx
}

// runTool runs one tool call and records its outcome. A call with an
// intent from an earlier run is recovered by its tool's policy instead.
func (w *worker) runTool(call *gatewayv0.ToolCall, prev *agentpb.ToolIntent) error {
	b := w.b
	input := json.RawMessage(call.GetInput())
	outcome := &agentpb.ToolOutcome{CallId: call.GetId(), WorkerId: w.id}
	var res callResult
	start := time.Now()

	if prev != nil {
		outcome.IntentId = prev.GetIntentId()
		rec := b.tools.Recover(call.GetName(), input, prev.GetCheck())
		if !rec.Rerun {
			outcome.Recovered = true
			res.Result = rec.Result
			b.text(w.id, "recovered %s call %s: %s", call.GetName(), call.GetId(), firstLine(res.Output))
		} else {
			b.text(w.id, "running %s call %s again: it is safe to repeat", call.GetName(), call.GetId())
			res = w.execute(call, input, nil)
		}
	} else {
		res = w.execute(call, input, outcome)
	}
	if res.stopped {
		return errStopped // no outcome: the next run handles the call
	}

	outcome.Content, outcome.IsError, outcome.OutcomeUnknown = res.Output, res.IsError, res.Unknown
	if crashHit("tool.done:"+call.GetName(), w.id) {
		return errStopped
	}
	if err := b.commit(outcome); err != nil {
		return err
	}
	for _, ch := range res.Changes {
		b.emit(w.id, &gatewayv0.ActivityEvent{Body: &gatewayv0.ActivityEvent_FileChange{FileChange: &gatewayv0.FileChange{
			CallId: call.GetId(), Path: ch.Path, Created: ch.Created, Diff: cut(ch.Diff, maxEventText),
		}}})
	}
	b.emit(w.id, &gatewayv0.ActivityEvent{Body: &gatewayv0.ActivityEvent_ToolFinished{ToolFinished: &gatewayv0.ToolFinished{
		CallId: call.GetId(), Tool: call.GetName(), IsError: res.IsError, OutcomeUnknown: res.Unknown,
		Output: cut(res.Output, maxEventIO), Took: durationpb.New(time.Since(start)),
	}}})
	return nil
}

// callResult is a tool result, or nothing if the call didn't run.
type callResult struct {
	tools.Result
	stopped bool
}

// execute prepares and runs a call. With a new outcome, it first records
// the intent; with nil, the call already has one.
func (w *worker) execute(call *gatewayv0.ToolCall, input json.RawMessage, outcome *agentpb.ToolOutcome) callResult {
	b := w.b
	b.emit(w.id, &gatewayv0.ActivityEvent{Body: &gatewayv0.ActivityEvent_ToolStarted{ToolStarted: &gatewayv0.ToolStarted{
		CallId: call.GetId(), Tool: call.GetName(), Input: cut(call.GetInput(), maxEventIO),
	}}})
	// Plan again now that the call holds its lock: what it changes may
	// have been changed by the call it waited for.
	c, err := b.tools.Prepare(call.GetName(), input)
	if err != nil {
		// Refused before running: nothing to recover, no intent needed.
		return callResult{Result: tools.Result{Output: err.Error(), IsError: true}}
	}
	if outcome != nil {
		in := &agentpb.ToolIntent{
			IntentId: newID(), CallId: call.GetId(), Tool: call.GetName(), Input: call.GetInput(),
			Check: c.Check, Policy: b.tools.Policy(call.GetName()).String(), WorkerId: w.id,
		}
		if err := b.commit(in); err != nil {
			return callResult{stopped: true}
		}
		outcome.IntentId = in.GetIntentId()
		if crashHit("tool.intent:"+call.GetName(), w.id) {
			return callResult{stopped: true}
		}
	}
	return callResult{Result: c.Run(tools.RunContext{Soft: b.env.Soft, Hard: b.env.Hard})}
}

// spawnInput is spawn_subagent's input.
type spawnInput struct {
	Task        string  `json:"task"`
	Model       string  `json:"model"`
	BudgetShare float64 `json:"budget_share"`
}

// spawn runs a spawn_subagent call: it creates the subagent, or finds the
// one an earlier run created, waits for it, and delivers its result as the
// call's outcome.
func (w *worker) spawn(call *gatewayv0.ToolCall, prev *agentpb.ToolIntent) error {
	b := w.b
	refuse := func(format string, args ...any) error {
		return b.commit(&agentpb.ToolOutcome{CallId: call.GetId(), WorkerId: w.id, Content: fmt.Sprintf(format, args...), IsError: true})
	}
	child := ""
	if prev != nil {
		child = b.childOf(w.id, call.GetId())
		if child == "" {
			return refuse("the subagent of this call was lost")
		}
	} else {
		var in spawnInput
		if err := decodeStrict(json.RawMessage(call.GetInput()), &in); err != nil {
			return refuse("invalid input: %v", err)
		}
		if strings.TrimSpace(in.Task) == "" {
			return refuse("task is required")
		}
		if in.BudgetShare < 0 || in.BudgetShare > 1 {
			return refuse("budget_share must be between 0 and 1")
		}
		var reason string
		var err error
		child, reason, err = w.createChild(call, in)
		if err != nil {
			return err
		}
		if reason != "" {
			return refuse("%s", reason)
		}
	}

	// Wait for the subagent; it runs on its own goroutine. One that
	// crashed without finishing, such as with a panic, failed: its parent
	// carries on.
	if err := b.waitFor(child); err != nil {
		if b.view(child).GetDone() == nil {
			crashed := &agentpb.WorkerDone{WorkerId: child, IsError: true, Result: "the subagent failed: " + err.Error()}
			if err := b.commit(crashed); err != nil {
				return err
			}
		}
	}
	st := b.view(child)
	done := st.GetDone()
	if done == nil {
		return errStopped
	}
	if crashHit("spawn.deliver", w.id) {
		return errStopped
	}
	out := &agentpb.ToolOutcome{CallId: call.GetId(), WorkerId: w.id, IntentId: prev.GetIntentId(), Content: done.GetResult(), IsError: done.GetIsError()}
	if prev == nil {
		out.IntentId = b.spawnIntent(w.id, call.GetId())
	}
	if err := b.commit(out); err != nil {
		return err
	}
	b.emit(w.id, &gatewayv0.ActivityEvent{Body: &gatewayv0.ActivityEvent_ToolFinished{ToolFinished: &gatewayv0.ToolFinished{
		CallId: call.GetId(), Tool: SpawnTool, IsError: done.GetIsError(), Output: cut(done.GetResult(), maxEventIO),
	}}})
	return nil
}

// createChild checks the limits and records the subagent's creation, as
// one step so that parallel spawns can't exceed them. It returns the
// reason for refusing, if it does.
func (w *worker) createChild(call *gatewayv0.ToolCall, in spawnInput) (child, refused string, err error) {
	b := w.b
	s := b.env.Settings
	model := s.SubagentModel
	if in.Model != "" {
		if !slices.Contains(s.Selectable, in.Model) {
			if len(s.Selectable) == 0 {
				return "", "this profile lets you choose no model; leave model out", nil
			}
			return "", fmt.Sprintf("model %q isn't one you may choose; choose from %s", in.Model, strings.Join(s.Selectable, ", ")), nil
		}
		model = in.Model
	}
	sys, version := s.Prompt.system(b.tools, true)

	b.mu.Lock()
	defer b.mu.Unlock()
	parent := b.st.workers[w.id]
	switch {
	case int(parent.GetDepth()) >= s.MaxSubagentDepth:
		return "", fmt.Sprintf("subagents may nest at most %d deep; this worker is at depth %d, so it can't spawn one. Do the subtask yourself", s.MaxSubagentDepth, parent.GetDepth()), nil
	case int(b.st.spawned) >= s.MaxSubagents:
		return "", fmt.Sprintf("the session may create at most %d subagents and has created %d. Do the subtask yourself", s.MaxSubagents, b.st.spawned), nil
	}
	n := 1
	for _, id := range b.st.order {
		if b.st.workers[id].GetParentId() == w.id {
			n++
		}
	}
	child = w.id + "." + strconv.Itoa(n)
	sp := &agentpb.Spawn{
		IntentId: newID(), ParentId: w.id, CallId: call.GetId(), BudgetShare: in.BudgetShare,
		Worker: &agentpb.WorkerState{
			WorkerId: child, ParentId: w.id, ParentCallId: call.GetId(), Depth: parent.GetDepth() + 1,
			Model: model, System: sys, PromptVersion: version,
			Messages: []*gatewayv0.Message{userText(fmt.Sprintf("# Subtask from %s\n\n%s", w.id, strings.TrimSpace(in.Task)))},
		},
	}
	if err := b.commitLocked(sp); err != nil {
		return "", "", err
	}
	b.emit(child, &gatewayv0.ActivityEvent{Body: &gatewayv0.ActivityEvent_SubagentSpawned{SubagentSpawned: &gatewayv0.SubagentSpawned{
		Parent: w.id, Task: cut(in.Task, maxEventText), Model: model,
	}}})
	if crashHit("spawn", w.id) {
		return "", "", errStopped
	}
	b.launchLocked(child)
	return child, "", nil
}

// childOf returns the subagent a parent's call created, or "".
func (b *box) childOf(parent, callID string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, id := range b.st.order {
		if w := b.st.workers[id]; w.GetParentId() == parent && w.GetParentCallId() == callID {
			return id
		}
	}
	return ""
}

// spawnIntent returns the intent ID of a parent's spawn call.
func (b *box) spawnIntent(parent, callID string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return intent(b.st.workers[parent], callID).GetIntentId()
}

// waitFor waits until a worker's goroutine ends.
func (b *box) waitFor(id string) error {
	b.mu.Lock()
	w := b.running[id]
	b.mu.Unlock()
	if w == nil {
		return nil // finished in an earlier run
	}
	<-w.done
	if w.err != nil && !errors.Is(w.err, errStopped) {
		return w.err
	}
	return nil
}
