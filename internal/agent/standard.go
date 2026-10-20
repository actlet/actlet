package agent

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/actlet/actlet/internal/agent/agentpb"
	"github.com/actlet/actlet/internal/tools"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// The standard tools: built into Actlet, and offered to the agent only
// when the profile grants them by name.
const (
	ProgressTool = "report_progress"
	FeedbackTool = "send_feedback"
)

// StandardTools are the names of the standard tools.
var StandardTools = []string{ProgressTool, FeedbackTool}

// IsStandard reports whether a tool is one of the standard tools.
func IsStandard(name string) bool { return slices.Contains(StandardTools, name) }

// Limits of the standard tools' input.
const (
	maxProgress = 1000
	maxFeedback = 4000
)

// FeedbackKinds are the kinds of feedback send_feedback takes.
var FeedbackKinds = []string{"problem", "suggestion", "question", "other"}

// standardTool is a standard tool.
type standardTool struct {
	spec *gatewayv0.ToolSpec
	// policy is what recovery does with a call that has an intent but no
	// outcome: "retry" runs it again.
	policy string
	// run carries a call out. The intent ID identifies the call to the
	// gateway, for tools it bridges.
	run func(w *worker, intentID string, input json.RawMessage) standardResult
}

// standardResult is what a standard tool call did.
type standardResult struct {
	tools.Result
	// Usage is what a call through the gateway used.
	Usage *gatewayv0.Usage
	// stopped: the call didn't finish for a pause or stop; it has no
	// outcome, and the next run makes it again.
	stopped bool
}

// standardTools returns the standard tools the settings grant.
func (b *box) standardTools() map[string]*standardTool {
	out := map[string]*standardTool{}
	for _, g := range b.env.Settings.Standard {
		var t *standardTool
		switch g.Name {
		case ProgressTool:
			t = progressTool()
		case FeedbackTool:
			t = feedbackTool()
		}
		if t != nil {
			out[g.Name] = t
		}
	}
	return out
}

// standardSpecs are the granted standard tools' specs, by name.
func (b *box) standardSpecs() []*gatewayv0.ToolSpec {
	var out []*gatewayv0.ToolSpec
	for _, name := range StandardTools {
		if t := b.standard[name]; t != nil {
			out = append(out, t.spec)
		}
	}
	return out
}

func progressTool() *standardTool {
	return &standardTool{
		spec: &gatewayv0.ToolSpec{
			Name: ProgressTool,
			Description: "Tell the people watching how the task is going: what you are doing now and, if you can tell, " +
				"how far along you are. Report at milestones, not after every step.",
			InputSchema: fmt.Sprintf(`{"type":"object","properties":{`+
				`"message":{"type":"string","maxLength":%d,"description":"what you are doing, in a sentence"},`+
				`"percent":{"type":"integer","minimum":0,"maximum":100,"description":"how much of the task is done"}},`+
				`"required":["message"],"additionalProperties":false}`, maxProgress),
		},
		policy: "retry",
		run: func(w *worker, _ string, input json.RawMessage) standardResult {
			var in struct {
				Message string  `json:"message"`
				Percent *uint32 `json:"percent"`
			}
			if err := decodeStrict(input, &in); err != nil {
				return refused("invalid input: %v", err)
			}
			msg := strings.TrimSpace(in.Message)
			switch {
			case msg == "":
				return refused("message is required")
			case len(msg) > maxProgress:
				return refused("message is longer than %d bytes", maxProgress)
			case in.Percent != nil && *in.Percent > 100:
				return refused("percent must be between 0 and 100")
			}
			w.b.emit(w.id, &gatewayv0.ActivityEvent{Body: &gatewayv0.ActivityEvent_Progress{Progress: &gatewayv0.Progress{
				Message: msg, Percent: in.Percent,
			}}})
			return standardResult{Result: tools.Result{Output: "Progress reported."}}
		},
	}
}

func feedbackTool() *standardTool {
	kinds, _ := json.Marshal(FeedbackKinds)
	return &standardTool{
		spec: &gatewayv0.ToolSpec{
			Name: FeedbackTool,
			Description: "Send feedback to the people running this service: a problem with the task or your setup " +
				"(a missing tool, unclear instructions, bad input), a suggestion, or a question. It doesn't wait for an answer.",
			InputSchema: fmt.Sprintf(`{"type":"object","properties":{`+
				`"kind":{"type":"string","enum":%s},`+
				`"message":{"type":"string","maxLength":%d}},`+
				`"required":["kind","message"],"additionalProperties":false}`, kinds, maxFeedback),
		},
		policy: "retry",
		run: func(w *worker, _ string, input json.RawMessage) standardResult {
			var in struct {
				Kind    string `json:"kind"`
				Message string `json:"message"`
			}
			if err := decodeStrict(input, &in); err != nil {
				return refused("invalid input: %v", err)
			}
			msg := strings.TrimSpace(in.Message)
			switch {
			case !slices.Contains(FeedbackKinds, in.Kind):
				return refused("kind %q: use %s", in.Kind, strings.Join(FeedbackKinds, ", "))
			case msg == "":
				return refused("message is required")
			case len(msg) > maxFeedback:
				return refused("message is longer than %d bytes", maxFeedback)
			}
			w.b.emit(w.id, &gatewayv0.ActivityEvent{Body: &gatewayv0.ActivityEvent_Feedback{Feedback: &gatewayv0.Feedback{
				Kind: in.Kind, Message: msg,
			}}})
			return standardResult{Result: tools.Result{Output: "Feedback sent."}}
		},
	}
}

func refused(format string, args ...any) standardResult {
	return standardResult{Result: tools.Result{Output: fmt.Sprintf(format, args...), IsError: true}}
}

// runStandard runs a standard tool call and records its outcome. A call
// with an intent from an earlier run is made again with the same intent
// ID, so a tool the gateway bridges answers it once.
func (w *worker) runStandard(call *gatewayv0.ToolCall, prev *agentpb.ToolIntent) error {
	b := w.b
	t := b.standard[call.GetName()]
	start := time.Now()
	b.emit(w.id, &gatewayv0.ActivityEvent{Body: &gatewayv0.ActivityEvent_ToolStarted{ToolStarted: &gatewayv0.ToolStarted{
		CallId: call.GetId(), Tool: call.GetName(), Input: cut(call.GetInput(), maxEventIO),
	}}})
	intentID := prev.GetIntentId()
	if prev == nil {
		in := &agentpb.ToolIntent{
			IntentId: newID(), CallId: call.GetId(), Tool: call.GetName(), Input: call.GetInput(), Policy: t.policy, WorkerId: w.id,
		}
		if err := b.commit(in); err != nil {
			return err
		}
		intentID = in.GetIntentId()
		if crashHit("tool.intent:"+call.GetName(), w.id) {
			return errStopped
		}
	} else {
		b.text(w.id, "making %s call %s again after an interruption (intent %s)", call.GetName(), call.GetId(), intentID)
	}
	res := t.run(w, intentID, json.RawMessage(call.GetInput()))
	if res.stopped {
		return errStopped
	}
	outcome := &agentpb.ToolOutcome{
		IntentId: intentID, CallId: call.GetId(), WorkerId: w.id, Content: res.Output, IsError: res.IsError, Usage: res.Usage,
	}
	if crashHit("tool.done:"+call.GetName(), w.id) {
		return errStopped
	}
	if err := b.commit(outcome); err != nil {
		return err
	}
	b.emit(w.id, &gatewayv0.ActivityEvent{Body: &gatewayv0.ActivityEvent_ToolFinished{ToolFinished: &gatewayv0.ToolFinished{
		CallId: call.GetId(), Tool: call.GetName(), IsError: res.IsError, Output: cut(res.Output, maxEventIO),
		Took: durationpb.New(time.Since(start)), Usage: res.Usage,
	}}})
	return nil
}
