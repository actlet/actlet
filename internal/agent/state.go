package agent

import (
	"fmt"

	"google.golang.org/protobuf/proto"

	"github.com/actlet/actlet/internal/agent/agentpb"
	"github.com/actlet/actlet/internal/journal"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// The agent's state is a set of workers, changed only by applying journal
// records. A live run records each record and applies it under one lock,
// so a checkpoint is a consistent snapshot of every worker; recovery
// applies the same records to the latest checkpoint and arrives at the
// same state.

// stateSet is the state of all workers.
type stateSet struct {
	workers   map[string]*agentpb.WorkerState
	order     []string // creation order
	delivered map[string]bool
	spawned   uint32
	parts     parts // of a packed checkpoint, during recovery
}

func newStateSet() *stateSet {
	return &stateSet{workers: map[string]*agentpb.WorkerState{}, delivered: map[string]bool{}}
}

func (s *stateSet) add(w *agentpb.WorkerState) {
	if w.GetUsage() == nil {
		w.Usage = &gatewayv0.Usage{}
	}
	s.workers[w.GetWorkerId()] = w
	s.order = append(s.order, w.GetWorkerId())
}

// checkpoint returns the state as a checkpoint. The caller must not change
// the state until the checkpoint is marshalled.
func (s *stateSet) checkpoint() *agentpb.Checkpoint {
	cp := &agentpb.Checkpoint{Spawned: s.spawned}
	for _, id := range s.order {
		cp.Workers = append(cp.Workers, s.workers[id])
	}
	for id := range s.delivered {
		cp.SteersDelivered = append(cp.SteersDelivered, id)
	}
	return cp
}

func (s *stateSet) restore(cp *agentpb.Checkpoint) {
	*s = *newStateSet()
	for _, w := range cp.GetWorkers() {
		s.add(w)
	}
	for _, id := range cp.GetSteersDelivered() {
		s.delivered[id] = true
	}
	s.spawned = cp.GetSpawned()
}

// decode decodes an agent journal record.
func decode(r journal.Record) (proto.Message, error) {
	var m proto.Message
	switch r.Kind {
	case KindCheckpoint:
		m = &agentpb.Checkpoint{}
	case KindCheckpointPart:
		m = &agentpb.CheckpointPart{}
	case KindInferenceIntent:
		m = &agentpb.InferenceIntent{}
	case KindInferenceOutcome:
		m = &agentpb.InferenceOutcome{}
	case KindToolIntent:
		m = &agentpb.ToolIntent{}
	case KindToolOutcome:
		m = &agentpb.ToolOutcome{}
	case KindSpawn:
		m = &agentpb.Spawn{}
	case KindWorkerDone:
		m = &agentpb.WorkerDone{}
	case KindSteer:
		m = &agentpb.SteerDelivered{}
	default:
		return nil, nil
	}
	if err := proto.Unmarshal(r.Data, m); err != nil {
		return nil, fmt.Errorf("journal record %d (%s): %w", r.Seq, r.Kind, err)
	}
	return m, nil
}

// apply changes the state by one record.
func (s *stateSet) apply(m proto.Message) error {
	worker := func(id string) (*agentpb.WorkerState, error) {
		w := s.workers[id]
		if w == nil {
			return nil, fmt.Errorf("record for unknown worker %q", id)
		}
		return w, nil
	}
	switch m := m.(type) {
	case *agentpb.CheckpointPart:
		s.parts.add(m)
	case *agentpb.Checkpoint:
		if m.GetPacked() != nil {
			cp, err := s.parts.unpack(m.GetPacked())
			if err != nil {
				return err
			}
			m = cp
		}
		s.restore(m)
	case *agentpb.InferenceIntent:
		w, err := worker(m.GetWorkerId())
		if err != nil {
			return err
		}
		w.Inference = m
	case *agentpb.InferenceOutcome:
		w, err := worker(m.GetWorkerId())
		if err != nil {
			return err
		}
		answered(w, m)
	case *agentpb.ToolIntent:
		w, err := worker(m.GetWorkerId())
		if err != nil {
			return err
		}
		w.Intents = append(w.Intents, m)
	case *agentpb.ToolOutcome:
		w, err := worker(m.GetWorkerId())
		if err != nil {
			return err
		}
		resolved(w, m)
	case *agentpb.Spawn:
		parent, err := worker(m.GetParentId())
		if err != nil {
			return err
		}
		parent.Intents = append(parent.Intents, &agentpb.ToolIntent{
			IntentId: m.GetIntentId(), CallId: m.GetCallId(), Tool: SpawnTool, Policy: "spawn", WorkerId: m.GetParentId(),
		})
		s.add(proto.Clone(m.GetWorker()).(*agentpb.WorkerState))
		s.spawned++
	case *agentpb.WorkerDone:
		w, err := worker(m.GetWorkerId())
		if err != nil {
			return err
		}
		w.Done = m
	case *agentpb.SteerDelivered:
		w, err := worker(m.GetWorkerId())
		if err != nil {
			return err
		}
		addUserText(w, m.GetText())
		s.delivered[m.GetCommandId()] = true
	}
	return nil
}

// answered applies a model answer to a worker.
func answered(w *agentpb.WorkerState, o *agentpb.InferenceOutcome) {
	w.Messages = append(w.Messages, o.GetMessage())
	w.Continuation = o.GetDone().GetContinuation()
	addUsage(w.Usage, o.GetDone().GetUsage())
	w.Turn++
	w.Inference = nil
	switch {
	case len(toolCalls(o.GetMessage())) > 0:
		w.Pending = o.GetMessage()
	case o.GetDone().GetStopReason() == gatewayv0.StopReason_STOP_REASON_MAX_TOKENS:
		w.Messages = append(w.Messages, userText("Your answer reached the output limit and was cut off. Continue where it stopped."))
	case o.GetDone().GetStopReason() == gatewayv0.StopReason_STOP_REASON_REFUSAL:
		w.Refused = true
	default:
		w.Ended = true
	}
}

// resolved applies a tool call's outcome. When every call of the answer
// has one, the results join the conversation, in the order of the calls.
func resolved(w *agentpb.WorkerState, o *agentpb.ToolOutcome) {
	addUsage(w.Usage, o.GetUsage())
	for i, in := range w.Intents {
		if in.GetCallId() == o.GetCallId() {
			w.Intents = append(w.Intents[:i], w.Intents[i+1:]...)
			break
		}
	}
	w.Results = append(w.Results, o)
	calls := toolCalls(w.Pending)
	if len(w.Results) < len(calls) {
		return
	}
	msg := &gatewayv0.Message{Role: gatewayv0.Role_ROLE_USER}
	for _, c := range calls {
		r := result(w, c.GetId())
		msg.Parts = append(msg.Parts, &gatewayv0.Part{Part: &gatewayv0.Part_ToolResult{ToolResult: &gatewayv0.ToolResult{
			CallId: c.GetId(), Content: r.GetContent(), IsError: r.GetIsError(),
		}}})
	}
	w.Messages = append(w.Messages, msg)
	w.Pending, w.Intents, w.Results = nil, nil, nil
}

// addUsage adds what a call used to a worker's usage. Costs add up in the
// currency the worker's first priced call used; a call priced in another
// currency adds its tokens only.
func addUsage(to, u *gatewayv0.Usage) {
	to.InputTokens += u.GetInputTokens()
	to.OutputTokens += u.GetOutputTokens()
	to.CacheReadTokens += u.GetCacheReadTokens()
	to.CacheWriteTokens += u.GetCacheWriteTokens()
	if u.GetCost() > 0 && (to.Currency == "" || to.Currency == u.GetCurrency()) {
		to.Cost += u.GetCost()
		to.Currency = u.GetCurrency()
	}
}

// result returns the outcome of a call of the pending answer, or nil.
func result(w *agentpb.WorkerState, callID string) *agentpb.ToolOutcome {
	for _, r := range w.GetResults() {
		if r.GetCallId() == callID {
			return r
		}
	}
	return nil
}

// intent returns the intent of a call of the pending answer, or nil.
func intent(w *agentpb.WorkerState, callID string) *agentpb.ToolIntent {
	for _, in := range w.GetIntents() {
		if in.GetCallId() == callID {
			return in
		}
	}
	return nil
}

// addUserText adds text to the user message the next model call ends
// with.
func addUserText(w *agentpb.WorkerState, text string) {
	if n := len(w.Messages); n > 0 && w.Messages[n-1].GetRole() == gatewayv0.Role_ROLE_USER {
		last := proto.Clone(w.Messages[n-1]).(*gatewayv0.Message)
		last.Parts = append(last.Parts, &gatewayv0.Part{Part: &gatewayv0.Part_Text{Text: text}})
		w.Messages[n-1] = last
		return
	}
	w.Messages = append(w.Messages, userText(text))
}
