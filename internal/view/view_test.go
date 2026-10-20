package view

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// events builds a numbered event sequence.
type events struct {
	seq uint64
	out []*gatewayv0.ActivityEvent
}

func (e *events) add(worker string, body any) {
	e.seq++
	ev := &gatewayv0.ActivityEvent{Epoch: 1, Seq: e.seq, Id: fmt.Sprintf("1.%d", e.seq), Worker: worker,
		Time: timestamppb.New(time.Unix(1700000000, 0).Add(time.Duration(e.seq) * time.Second))}
	switch b := body.(type) {
	case *gatewayv0.SessionStarted:
		ev.Body = &gatewayv0.ActivityEvent_Started{Started: b}
	case *gatewayv0.StateChange:
		ev.Body = &gatewayv0.ActivityEvent_State{State: b}
	case *gatewayv0.SessionInfo:
		ev.Body = &gatewayv0.ActivityEvent_SessionInfo{SessionInfo: b}
	case *gatewayv0.ModelTurn:
		ev.Body = &gatewayv0.ActivityEvent_ModelTurn{ModelTurn: b}
	case *gatewayv0.ToolStarted:
		ev.Body = &gatewayv0.ActivityEvent_ToolStarted{ToolStarted: b}
	case *gatewayv0.ToolFinished:
		ev.Body = &gatewayv0.ActivityEvent_ToolFinished{ToolFinished: b}
	case *gatewayv0.FileChange:
		ev.Body = &gatewayv0.ActivityEvent_FileChange{FileChange: b}
	case *gatewayv0.SubagentSpawned:
		ev.Body = &gatewayv0.ActivityEvent_SubagentSpawned{SubagentSpawned: b}
	case *gatewayv0.SubagentFinished:
		ev.Body = &gatewayv0.ActivityEvent_SubagentFinished{SubagentFinished: b}
	case *gatewayv0.SteerNote:
		ev.Body = &gatewayv0.ActivityEvent_Steer{Steer: b}
	case *gatewayv0.CommandReceived:
		ev.Body = &gatewayv0.ActivityEvent_Command{Command: b}
	case *gatewayv0.Gap:
		ev.Body = &gatewayv0.ActivityEvent_Gap{Gap: b}
	case *gatewayv0.Progress:
		ev.Body = &gatewayv0.ActivityEvent_Progress{Progress: b}
	case *gatewayv0.Feedback:
		ev.Body = &gatewayv0.ActivityEvent_Feedback{Feedback: b}
	case *gatewayv0.BudgetStatus:
		ev.Body = &gatewayv0.ActivityEvent_BudgetStatus{BudgetStatus: b}
	case string:
		ev.Body = &gatewayv0.ActivityEvent_Text{Text: &gatewayv0.Text{Text: b}}
	default:
		panic(fmt.Sprintf("%T", body))
	}
	e.out = append(e.out, ev)
}

// subagentRun is a session whose agent spawns a subagent.
func subagentRun() []*gatewayv0.ActivityEvent {
	var e events
	e.add("", &gatewayv0.SessionStarted{Epoch: 1, ProfileId: "coding"})
	e.add("", &gatewayv0.SessionInfo{TaskId: "t1", TaskKind: "agent", Task: `{"id":"t1"}`, Config: `{"model":"large"}`,
		Tools: []*gatewayv0.ToolInfo{{Name: "shell", Policy: "never"}}})
	e.add("", &gatewayv0.StateChange{To: gatewayv0.SessionState_SESSION_STATE_RUNNING, Reason: "started"})
	e.add("root", &gatewayv0.ModelTurn{Turn: 1, Model: "large", Text: "Splitting.", ToolCalls: []string{"spawn_subagent"},
		Usage: &gatewayv0.Usage{InputTokens: 1000, OutputTokens: 20}})
	e.add("root.1", &gatewayv0.SubagentSpawned{Parent: "root", Task: "write a", Model: "fast", CallId: "s1"})
	e.add("root.1", &gatewayv0.ModelTurn{Turn: 1, Model: "fast", ToolCalls: []string{"write_file"}, Usage: &gatewayv0.Usage{InputTokens: 300, OutputTokens: 5}})
	e.add("root.1", &gatewayv0.ToolStarted{CallId: "c1", Tool: "write_file", Input: `{"path":"a"}`})
	e.add("root.1", &gatewayv0.FileChange{CallId: "c1", Path: "a", Created: true, Diff: "+x\n"})
	e.add("root.1", &gatewayv0.ToolFinished{CallId: "c1", Tool: "write_file", Output: "created a", Took: durationpb.New(time.Millisecond)})
	e.add("", &gatewayv0.SteerNote{Note: "be quick", Origin: gatewayv0.Origin_ORIGIN_LOCAL, Actor: "alice"})
	e.add("root.1", &gatewayv0.SubagentFinished{Result: "wrote a", Turns: 2})
	e.add("root", &gatewayv0.ToolFinished{CallId: "s1", Tool: "spawn_subagent", Output: "wrote a"})
	e.add("root", &gatewayv0.ModelTurn{Turn: 2, Model: "large", Text: "Done.", Usage: &gatewayv0.Usage{InputTokens: 1100, OutputTokens: 3}})
	e.add("", &gatewayv0.StateChange{From: gatewayv0.SessionState_SESSION_STATE_RUNNING, To: gatewayv0.SessionState_SESSION_STATE_COMPLETED})
	return e.out
}

func fold(evs []*gatewayv0.ActivityEvent) *Session {
	s := New()
	for _, ev := range evs {
		s.Apply(ev)
	}
	return s
}

func TestFoldSubagentRun(t *testing.T) {
	s := fold(subagentRun())
	if s.State != gatewayv0.SessionState_SESSION_STATE_COMPLETED || s.Epoch != 1 || s.Profile != "coding" || s.Events != 14 {
		t.Errorf("session %+v", s)
	}
	if s.Info == nil || s.Info.TaskID != "t1" || len(s.Info.Tools) != 1 {
		t.Errorf("info %+v", s.Info)
	}
	var ids []string
	for _, w := range s.Workers() {
		ids = append(ids, w.ID)
	}
	if fmt.Sprint(ids) != "[root root.1]" {
		t.Errorf("workers %v", ids)
	}
	root, sub := s.Worker("root"), s.Worker("root.1")
	if root.Turns != 2 || root.Context != 1100 || root.Model != "large" || root.Status != WorkerFinished {
		t.Errorf("root %+v", root)
	}
	if sub.Parent != "root" || sub.Task != "write a" || sub.Model != "fast" || sub.Status != WorkerFinished || sub.Result != "wrote a" {
		t.Errorf("subagent %+v", sub)
	}
	// The subagent's turn holds its tool call, with the result and diff.
	if len(sub.Items) != 1 || sub.Items[0].Turn == nil || len(sub.Items[0].Turn.Calls) != 1 {
		t.Fatalf("subagent items %+v", sub.Items)
	}
	call := sub.Items[0].Turn.Calls[0]
	if call.Status != ToolOK || call.Output != "created a" || len(call.Diffs) != 1 || !call.Diffs[0].Created || call.Took != time.Millisecond {
		t.Errorf("tool call %+v", call)
	}
	// The root's first turn holds the spawn call, which leads to the
	// subagent.
	if len(root.Items) != 2 || root.Items[0].Turn.Text != "Splitting." || len(root.Items[0].Turn.Calls) != 1 {
		t.Fatalf("root items %+v", root.Items)
	}
	if spawn := root.Items[0].Turn.Calls[0]; spawn.Child != "root.1" || spawn.Status != ToolOK || spawn.Name != "spawn_subagent" {
		t.Errorf("spawn call %+v", spawn)
	}
	if files := s.Files(); len(files) != 1 || files[0].Path != "workspace/a" {
		t.Errorf("files %+v", files)
	}
	if s.Usage.Input != 2400 || s.Usage.Output != 28 || fmt.Sprint(s.Models()) != "[fast large]" {
		t.Errorf("usage %+v, models %v", s.Usage, s.Models())
	}
	notes := 0
	for _, it := range s.Items {
		if it.Kind == ItemNote && it.From == "alice" && it.Text == "be quick" {
			notes++
		}
	}
	if notes != 1 || len(s.Log) != 14 {
		t.Errorf("%d notes, %d log lines", notes, len(s.Log))
	}
	if _, tool, n := s.Running(); tool != nil || n != 0 {
		t.Errorf("a tool still running: %v", tool)
	}
	if s.Last.Sub(s.First) != 13*time.Second {
		t.Errorf("first %v, last %v", s.First, s.Last)
	}
}

// Folding the same events gives the same state, however they arrive.
func TestFoldDeterministic(t *testing.T) {
	a, b := fold(subagentRun()), New()
	for _, ev := range subagentRun() {
		b.Apply(ev)
	}
	if !reflect.DeepEqual(a, b) {
		t.Error("two folds differ")
	}
	b.Apply(subagentRun()[3])
	if reflect.DeepEqual(a, b) {
		t.Error("an extra event changed nothing")
	}
}

func TestRunningAndUnknown(t *testing.T) {
	var e events
	e.add("root", &gatewayv0.ToolStarted{CallId: "a", Tool: "shell"})
	e.add("root", &gatewayv0.ToolStarted{CallId: "b", Tool: "grep"})
	s := fold(e.out)
	if w, tool, n := s.Running(); w.ID != "root" || tool.Name != "grep" || n != 2 {
		t.Errorf("running %v %v %d", w, tool, n)
	}
	// A recovered call reports an outcome without having started in this
	// run.
	e.add("root", &gatewayv0.ToolFinished{CallId: "z", Tool: "shell", OutcomeUnknown: true, IsError: true})
	e.add("", &gatewayv0.Gap{Dropped: 7})
	s = fold(e.out)
	turn := s.Worker("root").Items[0].Turn
	last := turn.Calls[2]
	if !turn.Unseen || last.CallID != "z" || last.Status != ToolUnknown || s.Skipped != 7 {
		t.Errorf("recovered call %+v, skipped %d", last, s.Skipped)
	}
}

func TestBounded(t *testing.T) {
	var e events
	for i := range MaxLog + 10 {
		e.add("root", fmt.Sprintf("line %d", i))
	}
	s := fold(e.out)
	w := s.Worker("root")
	if len(w.Items) != MaxItems || w.Dropped != MaxLog+10-MaxItems || w.Items[0].Text != fmt.Sprintf("line %d", MaxLog+10-MaxItems) {
		t.Errorf("%d items, %d dropped, first %q", len(w.Items), w.Dropped, w.Items[0].Text)
	}
	if len(s.Log) != MaxLog || s.LogDropped != 10 {
		t.Errorf("%d log lines, %d dropped", len(s.Log), s.LogDropped)
	}
}

// The sidebar's files: paths relative to the session folder, and every
// change of a file counted, in the order the changes were made.
func TestFiles(t *testing.T) {
	var e events
	e.add("root", &gatewayv0.ToolStarted{CallId: "w", Tool: "write_file"})
	e.add("root", &gatewayv0.FileChange{CallId: "w", Path: "../output/report.md", Created: true, Diff: "+a\n+b\n+c\n"})
	e.add("root.1", &gatewayv0.ToolStarted{CallId: "e", Tool: "edit_file"})
	e.add("root.1", &gatewayv0.FileChange{CallId: "e", Path: "../output/report.md", Diff: "@@ -2 +2 @@\n-b\n+B\n"})
	e.add("root", &gatewayv0.ToolStarted{CallId: "t", Tool: "write_file"})
	e.add("root", &gatewayv0.FileChange{CallId: "t", Path: "./totals.py", Created: true, Diff: "+x\n"})
	s := fold(e.out)
	want := []File{{Path: "output/report.md", Added: 4, Removed: 1, Changes: 2}, {Path: "workspace/totals.py", Added: 1, Changes: 1}}
	if got := s.Files(); !reflect.DeepEqual(got, want) {
		t.Errorf("files %+v, want %+v", got, want)
	}
	// The subagent's edit came after the root's write, though root.1 is
	// listed after root.
	if h := s.FileHistory("output/report.md"); len(h) != 2 || !h[0].Created || h[1].Created {
		t.Errorf("history %+v", h)
	}
}

func TestSessionPath(t *testing.T) {
	for _, tt := range []struct{ tool, session string }{
		{"totals.py", "workspace/totals.py"},
		{"./a/b.md", "workspace/a/b.md"},
		{"../output/report.md", "output/report.md"},
		{"../inputs/sales.csv", "inputs/sales.csv"},
		{"/abs/file", "/abs/file"},
	} {
		if got := SessionPath(tt.tool); got != tt.session {
			t.Errorf("SessionPath(%q) = %q, want %q", tt.tool, got, tt.session)
		}
		if got := SessionPath(ToolPath(tt.session)); got != tt.session {
			t.Errorf("SessionPath(ToolPath(%q)) = %q", tt.session, got)
		}
	}
}

func TestWorkerOrder(t *testing.T) {
	ids := []string{"root.10", "root.2", "root", "root.1.1", "root.1"}
	s := New()
	for _, id := range ids {
		s.worker(id)
	}
	var got []string
	for _, w := range s.Workers() {
		got = append(got, w.ID)
	}
	if fmt.Sprint(got) != "[root root.1 root.1.1 root.2 root.10]" {
		t.Errorf("order %v", got)
	}
}

// Text from tools and models can't drive the terminal.
func TestSanitize(t *testing.T) {
	tests := []struct{ in, want string }{
		{"plain text\n\tindented", "plain text\n\tindented"},
		{"red \x1b[31malert\x1b[0m", "red alert"},
		{"title \x1b]0;pwned\x07 done", "title  done"},
		{"clip \x1b]52;c;ZXZpbA==\x1b\\ end", "clip  end"},
		{"clear\x1b[2J\x1b[H screen", "clear screen"},
		{"over\rwrite", "overwrite"},
		{"bell\x07 and nul\x00", "bell and nul"},
		{"c1 csi \u009b31m red", "c1 csi  red"},
		{"bidi \u202egnp.exe", "bidi gnp.exe"},
		{"bad utf8 \xff\xfe", "bad utf8 ��"},
		{"dcs \x1bPq#0;2;0;0;0\x1b\\ sixel", "dcs  sixel"},
		{"keypad \x1b= mode", "keypad  mode"},
		{"émoji ✓ 日本", "émoji ✓ 日本"},
		{"cut \x1b[", "cut "},
	}
	for _, tt := range tests {
		if got := Sanitize(tt.in); got != tt.want {
			t.Errorf("Sanitize(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	// Everything folded is sanitised.
	var e events
	e.add("root", &gatewayv0.ToolFinished{CallId: "x", Tool: "shell\x1b[2J", Output: "\x1b]0;evil\x07out"})
	e.add("root", "\x1b[31mtext")
	s := fold(e.out)
	for _, it := range s.Worker("root").Items {
		all := it.Text
		if it.Turn != nil {
			for _, c := range it.Turn.Calls {
				all += c.Name + c.Output
			}
		}
		if strings.ContainsAny(all, "\x1b\x07") {
			t.Errorf("unsanitised %q", all)
		}
	}
	for _, l := range s.Log {
		if strings.ContainsAny(l.Text, "\x1b\x07") {
			t.Errorf("unsanitised log %q", l.Text)
		}
	}
}

func FuzzSanitize(f *testing.F) {
	for _, s := range []string{"\x1b[31m", "\x1b]52;c;x\x07", "a\rb", "\u009b1;2m"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out := Sanitize(s)
		for _, r := range out {
			if r == 0x1b || r == 0x07 || r == '\r' || (r < 0x20 && r != '\n' && r != '\t') || (r >= 0x80 && r < 0xa0) {
				t.Fatalf("Sanitize(%q) kept %U", s, r)
			}
		}
	})
}

// Progress reports and feedback from the agent's tools.
func TestProgressAndFeedback(t *testing.T) {
	var e events
	pct := uint32(40)
	e.add("root", &gatewayv0.Progress{Message: "reading", Percent: &pct})
	e.add("root.1", &gatewayv0.Progress{Message: "almost\x1b[2J there"})
	e.add("root", &gatewayv0.Feedback{Kind: "problem", Message: "no header"})
	s := fold(e.out)
	if p := s.Progress; p == nil || p.Worker != "root.1" || p.Message != "almost there" || p.Percent != -1 {
		t.Errorf("progress %+v", p)
	}
	if len(s.Feedback) != 1 || s.Feedback[0].Kind != "problem" || s.Feedback[0].Message != "no header" {
		t.Errorf("feedback %+v", s.Feedback)
	}
	if got := Describe(e.out[0]); got != "progress 40%: reading" {
		t.Errorf("describe %q", got)
	}
}

// A profile update is named by what it changes, in the timeline and the
// log.
func TestProfileUpdateCommand(t *testing.T) {
	for _, tc := range []struct {
		u    *gatewayv0.ProfileUpdate
		want string
	}{
		{&gatewayv0.ProfileUpdate{Fields: []string{"budgets"}}, "profile update (budgets)"},
		{&gatewayv0.ProfileUpdate{Fields: []string{"budgets"}, ResetBudgets: []gatewayv0.BudgetKind{gatewayv0.BudgetKind_BUDGET_KIND_MONEY}}, "profile update (budgets; reset money)"},
		{&gatewayv0.ProfileUpdate{Fields: []string{"limits.max_workers", "local_attach"}, ResetBudgets: []gatewayv0.BudgetKind{
			gatewayv0.BudgetKind_BUDGET_KIND_MONEY, gatewayv0.BudgetKind_BUDGET_KIND_TOKENS}}, "profile update (limits.max_workers; local_attach; reset money, tokens)"},
	} {
		var e events
		cmd := &gatewayv0.Command{Id: "c1", Origin: gatewayv0.Origin_ORIGIN_ORCHESTRATOR, Action: &gatewayv0.Command_ProfileUpdate{ProfileUpdate: tc.u}}
		e.add("", &gatewayv0.CommandReceived{Command: cmd, Result: gatewayv0.CommandResult_COMMAND_RESULT_ACCEPTED})
		s := fold(e.out)
		if got := s.Items[0].Text; got != tc.want+" from the orchestrator accepted" {
			t.Errorf("timeline %q", got)
		}
		if got := s.Log[0].Text; got != "command c1: "+tc.want+" accepted" {
			t.Errorf("log %q", got)
		}
	}
}
