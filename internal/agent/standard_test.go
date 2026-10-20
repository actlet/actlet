package agent

import (
	"fmt"
	"slices"
	"testing"

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
