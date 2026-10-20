package agent

import (
	"context"
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
	BudgetTool   = "budget_status"
	DecideTool   = "decide"
	LLMTool      = "llm"
)

// StandardTools are the names of the standard tools.
var StandardTools = []string{ProgressTool, FeedbackTool, BudgetTool, DecideTool, LLMTool}

// IsStandard reports whether a tool is one of the standard tools.
func IsStandard(name string) bool { return slices.Contains(StandardTools, name) }

// Limits of the standard tools' input.
const (
	maxProgress    = 1000
	maxFeedback    = 4000
	maxQuestion    = 4000
	maxOptions     = 20
	maxOption      = 200
	maxContext     = 16 << 10
	maxPrompt      = 64 << 10
	maxLLMOutput   = 64 << 10
	maxLLMMaxToken = 32000
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
		case BudgetTool:
			t = budgetTool()
		case DecideTool:
			t = decideTool()
		case LLMTool:
			t = b.llmTool()
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

func budgetTool() *standardTool {
	return &standardTool{
		spec: &gatewayv0.ToolSpec{
			Name:        BudgetTool,
			Description: "See how much of the session's budgets (time, money, tokens, disk space) is used, to plan your work within them.",
			InputSchema: `{"type":"object","properties":{},"additionalProperties":false}`,
		},
		policy: "retry",
		run: func(w *worker, _ string, input json.RawMessage) standardResult {
			var in struct{}
			if err := decodeStrict(input, &in); err != nil {
				return refused("invalid input: %v", err)
			}
			if w.b.env.Budget == nil {
				return standardResult{Result: tools.Result{Output: "This session has no budgets."}}
			}
			return standardResult{Result: tools.Result{Output: w.b.env.Budget.Summary()}}
		},
	}
}

func decideTool() *standardTool {
	return &standardTool{
		spec: &gatewayv0.ToolSpec{
			Name: DecideTool,
			Description: "Ask for a decision you shouldn't make alone: the orchestrator puts the question to a person or a " +
				"system that decides, and the answer is this call's result. It may take a while. Give the options you see " +
				"and the context needed to choose.",
			InputSchema: fmt.Sprintf(`{"type":"object","properties":{`+
				`"question":{"type":"string","maxLength":%d},`+
				`"options":{"type":"array","maxItems":%d,"items":{"type":"string","maxLength":%d}},`+
				`"context":{"type":"string","maxLength":%d,"description":"what the decider needs to know"}},`+
				`"required":["question"],"additionalProperties":false}`, maxQuestion, maxOptions, maxOption, maxContext),
		},
		policy: "bridge",
		run: func(w *worker, intentID string, input json.RawMessage) standardResult {
			var in struct {
				Question string   `json:"question"`
				Options  []string `json:"options"`
				Context  string   `json:"context"`
			}
			if err := decodeStrict(input, &in); err != nil {
				return refused("invalid input: %v", err)
			}
			switch {
			case strings.TrimSpace(in.Question) == "":
				return refused("question is required")
			case len(in.Question) > maxQuestion:
				return refused("question is longer than %d bytes", maxQuestion)
			case len(in.Options) > maxOptions:
				return refused("give at most %d options", maxOptions)
			case len(in.Context) > maxContext:
				return refused("context is longer than %d bytes", maxContext)
			}
			for _, o := range in.Options {
				if len(o) > maxOption {
					return refused("an option is longer than %d bytes", maxOption)
				}
			}
			return w.bridge(DecideTool, intentID, input)
		},
	}
}

// bridge calls a tool the gateway implements, with the call's intent ID,
// once the budget allows. A pause or stop ends the wait without an
// outcome: the next run asks again with the same intent ID, and the
// gateway answers it once.
func (w *worker) bridge(tool, intentID string, input json.RawMessage) standardResult {
	b := w.b
	if b.env.CallTool == nil {
		return refused("this gateway provides no %s tool", tool)
	}
	if err := b.waitBudget(); err != nil {
		return standardResult{stopped: true}
	}
	ctx, cancel := b.callContext()
	defer cancel()
	resp, err := b.env.CallTool(ctx, &gatewayv0.ToolCallRequest{
		SessionId: b.env.SessionID, Epoch: b.env.Epoch, IntentId: intentID, Worker: w.id, Tool: tool, Input: string(input),
	})
	if err != nil {
		if ctx.Err() != nil {
			return standardResult{stopped: true}
		}
		return refused("%v", err)
	}
	return standardResult{Result: tools.Result{Output: resp.GetOutput(), IsError: resp.GetIsError()}, Usage: resp.GetUsage()}
}

// callContext ends when a call through the gateway must stop waiting: a
// pause or stop.
func (b *box) callContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-b.env.Stopping:
		case <-b.stopAll:
		case <-b.env.Soft.Done():
		case <-b.env.Hard.Done():
		case <-ctx.Done():
		}
		cancel()
	}()
	return ctx, cancel
}

// llmModels are the aliases the llm tool may use: the default first.
func (b *box) llmModels() []string {
	var out []string
	if m := b.env.Settings.LLMModel; m != "" {
		out = append(out, m)
	}
	for _, m := range b.env.Settings.Selectable {
		if !slices.Contains(out, m) {
			out = append(out, m)
		}
	}
	return out
}

func (b *box) llmTool() *standardTool {
	models := b.llmModels()
	if len(models) == 0 {
		return nil // no model to call
	}
	names, _ := json.Marshal(models)
	return &standardTool{
		spec: &gatewayv0.ToolSpec{
			Name: LLMTool,
			Description: "Call another model for side work, such as summarising or classifying text, and get its answer. " +
				"It sees only what you send it and has no tools. Choose a model, or leave it out for the default, " + models[0] + ".",
			InputSchema: fmt.Sprintf(`{"type":"object","properties":{`+
				`"prompt":{"type":"string","maxLength":%d},`+
				`"system":{"type":"string","description":"instructions for the model"},`+
				`"model":{"type":"string","enum":%s},`+
				`"max_output_tokens":{"type":"integer","minimum":1,"maximum":%d}},`+
				`"required":["prompt"],"additionalProperties":false}`, maxPrompt, names, maxLLMMaxToken),
		},
		policy: "bridge",
		run: func(w *worker, intentID string, input json.RawMessage) standardResult {
			var in struct {
				Prompt          string `json:"prompt"`
				System          string `json:"system"`
				Model           string `json:"model"`
				MaxOutputTokens uint32 `json:"max_output_tokens"`
			}
			if err := decodeStrict(input, &in); err != nil {
				return refused("invalid input: %v", err)
			}
			model := models[0]
			switch {
			case strings.TrimSpace(in.Prompt) == "":
				return refused("prompt is required")
			case len(in.Prompt)+len(in.System) > maxPrompt:
				return refused("prompt and system are longer than %d bytes", maxPrompt)
			case in.MaxOutputTokens > maxLLMMaxToken:
				return refused("max_output_tokens is at most %d", maxLLMMaxToken)
			case in.Model != "" && !slices.Contains(models, in.Model):
				return refused("model %q isn't one you may use; use %s", in.Model, strings.Join(models, ", "))
			case in.Model != "":
				model = in.Model
			}
			return w.callLLM(intentID, model, in.System, in.Prompt, in.MaxOutputTokens)
		},
	}
}

// callLLM makes the llm tool's model call, with the call's intent ID.
func (w *worker) callLLM(intentID, model, system, prompt string, maxTokens uint32) standardResult {
	b := w.b
	if err := b.waitBudget(); err != nil {
		return standardResult{stopped: true}
	}
	ctx, cancel := b.callContext()
	defer cancel()
	ans, err := b.env.Infer(ctx, &gatewayv0.InferenceRequest{
		SessionId: b.env.SessionID, Epoch: b.env.Epoch, IntentId: intentID, Model: model, System: system,
		Messages: []*gatewayv0.Message{userText(prompt)}, MaxOutputTokens: maxTokens, Purpose: "llm", Worker: w.id,
	})
	if err != nil {
		if ctx.Err() != nil {
			return standardResult{stopped: true}
		}
		return refused("%v", err)
	}
	text := messageText(ans.Message)
	if ans.Done.GetStopReason() == gatewayv0.StopReason_STOP_REASON_REFUSAL {
		return standardResult{Result: tools.Result{Output: "the model refused: " + cut(text, maxLLMOutput), IsError: true}, Usage: ans.Done.GetUsage()}
	}
	if ans.Done.GetStopReason() == gatewayv0.StopReason_STOP_REASON_MAX_TOKENS {
		text += "\n[the answer reached max_output_tokens and was cut off]"
	}
	return standardResult{Result: tools.Result{Output: cut(text, maxLLMOutput)}, Usage: ans.Done.GetUsage()}
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
