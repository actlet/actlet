package agent

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/actlet/actlet/internal/tools"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// offers checks which standard tools a model call is offered.
func offers(want ...string) func(*gatewayv0.InferenceRequest) error {
	return func(req *gatewayv0.InferenceRequest) error {
		var got []string
		for _, sp := range req.GetTools() {
			if IsStandard(sp.GetName()) {
				got = append(got, sp.GetName())
			}
		}
		if !slices.Equal(got, want) {
			return fmt.Errorf("standard tools offered %v, want %v", got, want)
		}
		return nil
	}
}

// The event tools are offered only when granted, and their calls become
// activity events.
func TestEventTools(t *testing.T) {
	progress := func(ev *gatewayv0.ActivityEvent) bool { return ev.GetProgress() != nil }
	feedback := func(ev *gatewayv0.ActivityEvent) bool { return ev.GetFeedback() != nil }

	// Not granted: not offered, and refused if called anyway.
	s := newSandbox(t,
		turn{check: offers(), calls: [][2]string{{ProgressTool, `{"message":"starting"}`}}},
		turn{check: expect("unknown tool"), text: "done"},
	)
	s.mustFinish()
	if s.eventsOf(progress) != 0 {
		t.Error("an ungranted tool reported progress")
	}

	s = newSandbox(t,
		turn{check: offers(ProgressTool, FeedbackTool), calls: [][2]string{
			{ProgressTool, `{"message":"reading the input","percent":40}`},
			{FeedbackTool, `{"kind":"problem","message":"the input has no header"}`},
			{ProgressTool, `{"message":"too far","percent":140}`},
			{FeedbackTool, `{"kind":"rant","message":"x"}`},
		}},
		turn{check: expect("Progress reported", "Feedback sent", "percent must be between 0 and 100", `kind "rant"`), text: "done"},
	)
	s.settings.Standard = []tools.Grant{{Name: ProgressTool}, {Name: FeedbackTool}}
	s.mustFinish()
	if s.eventsOf(progress) != 1 || s.eventsOf(feedback) != 1 {
		t.Fatalf("%d progress and %d feedback events", s.eventsOf(progress), s.eventsOf(feedback))
	}
	var p *gatewayv0.Progress
	var f *gatewayv0.Feedback
	for _, ev := range s.events {
		if ev.GetProgress() != nil {
			p = ev.GetProgress()
		}
		if ev.GetFeedback() != nil {
			f = ev.GetFeedback()
		}
	}
	if p.GetMessage() != "reading the input" || p.GetPercent() != 40 || f.GetKind() != "problem" || f.GetMessage() != "the input has no header" {
		t.Errorf("progress %v, feedback %v", p, f)
	}
}

// A progress report interrupted after its intent is made again.
func TestEventToolRecovery(t *testing.T) {
	s := newSandbox(t,
		turn{calls: [][2]string{{ProgressTool, `{"message":"halfway","percent":50}`}}},
		turn{check: expect("Progress reported"), text: "done"},
	)
	s.settings.Standard = []tools.Grant{{Name: ProgressTool}}
	s.crashAt = "tool.intent:" + ProgressTool
	s.mustCrash()
	s.mustFinish()
	if n := s.eventsOf(func(ev *gatewayv0.ActivityEvent) bool { return ev.GetProgress() != nil }); n != 1 {
		t.Errorf("%d progress events", n)
	}
	if o := s.outcomes(); len(o) != 1 || o[0].GetContent() != "Progress reported." {
		t.Errorf("outcomes %v", o)
	}
}

// fakeBudget holds model calls back until released, and records what
// the agent reports.
type fakeBudget struct {
	mu      sync.Mutex
	release chan struct{}
	waits   int
	tokens  uint64
	cost    float64
	tools   int
}

func (f *fakeBudget) Spent(tokens uint64, cost float64, currency string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens, f.cost = tokens, cost
}

func (f *fakeBudget) ToolDone() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tools++
}

func (f *fakeBudget) Wait(ctx context.Context) error {
	f.mu.Lock()
	f.waits++
	f.mu.Unlock()
	select {
	case <-f.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *fakeBudget) Summary() string { return "money: 1.00 EUR of 2.00 EUR used (50%), ok" }

// Model calls wait for the budget; the agent reports what it used, and
// the budget_status tool describes the budgets.
func TestAgentBudget(t *testing.T) {
	s := newSandbox(t,
		turn{calls: [][2]string{{BudgetTool, `{}`}, {"write_file", `{"path":"a.txt","content":"a"}`}}},
		turn{check: expect("1.00 EUR of 2.00 EUR", "created"), text: "done"},
	)
	s.settings.Standard = []tools.Grant{{Name: BudgetTool}}
	fb := &fakeBudget{release: make(chan struct{})}
	s.budget = fb
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.mustFinish()
	}()
	time.Sleep(200 * time.Millisecond)
	if n := len(s.model.callsOf("root")); n != 0 {
		t.Fatalf("%d model calls before the budget allowed them", n)
	}
	close(fb.release)
	<-done
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if fb.waits != 2 || fb.tokens != 24 || fb.tools != 1 {
		t.Errorf("waits %d, tokens %d, tool calls %d", fb.waits, fb.tokens, fb.tools)
	}
}
