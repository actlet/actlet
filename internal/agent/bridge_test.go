package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/actlet/actlet/internal/tools"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// decider is a gateway's decide tool: it answers once released, and
// records the calls.
type decider struct {
	mu      sync.Mutex
	calls   []*gatewayv0.ToolCallRequest
	release chan struct{}
}

func (d *decider) call(ctx context.Context, req *gatewayv0.ToolCallRequest) (*gatewayv0.ToolCallResponse, error) {
	d.mu.Lock()
	d.calls = append(d.calls, req)
	d.mu.Unlock()
	select {
	case <-d.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &gatewayv0.ToolCallResponse{Output: `{"decision":"ship it","decided_by":"alice"}`, Usage: &gatewayv0.Usage{InputTokens: 7, OutputTokens: 3}}, nil
}

func (d *decider) intents() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for _, c := range d.calls {
		out = append(out, c.GetIntentId())
	}
	return out
}

const decideCall = `{"question":"Ship the release?","options":["ship it","wait"],"context":"all tests pass"}`

// A decide call goes to the gateway with its intent ID; its answer is
// the call's result.
func TestDecide(t *testing.T) {
	s := newSandbox(t,
		turn{check: offers(DecideTool), calls: [][2]string{{DecideTool, decideCall}, {DecideTool, `{"question":""}`}}},
		turn{check: expect("ship it", "question is required"), text: "done"},
	)
	s.settings.Standard = []tools.Grant{{Name: DecideTool}}
	d := &decider{release: make(chan struct{})}
	close(d.release)
	s.callTool = d.call
	s.mustFinish()
	if len(d.calls) != 1 || d.calls[0].GetTool() != DecideTool || d.calls[0].GetWorker() != RootWorker || !strings.Contains(d.calls[0].GetInput(), "Ship the release?") {
		t.Fatalf("calls %v", d.calls)
	}
	for _, o := range s.outcomes() {
		if o.GetCallId() == "call_1_1" && (o.GetIntentId() != d.calls[0].GetIntentId() || o.GetUsage().GetInputTokens() != 7) {
			t.Errorf("outcome %v", o)
		}
	}
}

// A decide call waiting for its answer survives a pause and a crash: the
// next run asks again with the same intent ID, and the answer is
// recorded once.
func TestDecideSurvivesPauseAndCrash(t *testing.T) {
	s := newSandbox(t,
		turn{calls: [][2]string{{DecideTool, decideCall}}},
		turn{check: expect("ship it"), text: "done"},
	)
	s.settings.Standard = []tools.Grant{{Name: DecideTool}}
	d := &decider{release: make(chan struct{})}
	s.callTool = d.call

	// A pause while waiting ends the run without an outcome.
	stopping := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		if finished, err, _ := s.run(t.Context(), t.Context(), stopping); finished || err != nil {
			t.Errorf("paused run: done %v, err %v", finished, err)
		}
	}()
	waitUntil(t, func() bool { return len(d.intents()) == 1 })
	close(stopping)
	<-done
	if n := len(s.outcomes()); n != 0 {
		t.Fatalf("%d outcomes after the pause", n)
	}

	// A crash right after asking again: no outcome either.
	s.crashAt = "tool.done:" + DecideTool
	close(d.release)
	s.mustCrash()

	s.mustFinish()
	ids := d.intents()
	if len(ids) != 3 || ids[0] != ids[1] || ids[1] != ids[2] {
		t.Fatalf("intent IDs %v: every run must ask with the same one", ids)
	}
	if o := s.outcomes(); len(o) != 1 || o[0].GetIntentId() != ids[0] {
		t.Errorf("outcomes %v", o)
	}
}

// Without a gateway that bridges tools, decide fails for the model.
func TestDecideUnavailable(t *testing.T) {
	s := newSandbox(t,
		turn{calls: [][2]string{{DecideTool, decideCall}}},
		turn{check: expect("this gateway provides no decide tool"), text: "done"},
	)
	s.settings.Standard = []tools.Grant{{Name: DecideTool}}
	s.mustFinish()
	s = newSandbox(t,
		turn{calls: [][2]string{{DecideTool, decideCall}}},
		turn{check: expect("decide call: rpc error"), text: "done"},
	)
	s.settings.Standard = []tools.Grant{{Name: DecideTool}}
	s.callTool = func(context.Context, *gatewayv0.ToolCallRequest) (*gatewayv0.ToolCallResponse, error) {
		return nil, errors.New("decide call: rpc error: code = Unimplemented")
	}
	s.mustFinish()
}

// The llm tool calls the model for side work with the llm alias, or one
// the agent may choose; its usage counts.
func TestLLM(t *testing.T) {
	s := newSandbox(t,
		turn{check: offers(LLMTool), calls: [][2]string{
			{LLMTool, `{"prompt":"summarise this"}`},
			{LLMTool, `{"prompt":"classify this","model":"cheap"}`},
			{LLMTool, `{"prompt":"x","model":"huge"}`},
		}},
		turn{check: expect("LLM says: summarise this", "LLM says: classify this", `model "huge" isn't one you may use; use side, cheap`), text: "done"},
	)
	s.settings.Standard = []tools.Grant{{Name: LLMTool}}
	s.settings.LLMModel, s.settings.Selectable = "side", []string{"cheap"}
	fb := &fakeBudget{release: make(chan struct{})}
	close(fb.release)
	s.budget = fb
	s.mustFinish()
	models := map[string]string{} // prompt → model; the calls run in parallel
	for _, req := range s.model.llm {
		models[messageText(req.GetMessages()[0])] = req.GetModel()
		if len(req.GetTools()) != 0 {
			t.Errorf("an llm call offers tools: %v", req.GetTools())
		}
	}
	if len(s.model.llm) != 2 || models["summarise this"] != "side" || models["classify this"] != "cheap" {
		t.Fatalf("llm calls %v", s.model.llm)
	}
	if fb.tokens != 2*12+2*110 || fb.cost != 1 {
		t.Errorf("budget told %d tokens, %.2f cost", fb.tokens, fb.cost)
	}
	// No alias to use: no llm tool, even granted.
	s = newSandbox(t, turn{check: offers(), text: "done"})
	s.settings.Standard = []tools.Grant{{Name: LLMTool}}
	s.mustFinish()
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
