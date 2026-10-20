package agent

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/actlet/actlet/internal/agent/agentpb"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

func spawnCall(task string) [2]string {
	return [2]string{SpawnTool, fmt.Sprintf(`{"task":%q}`, task)}
}

// writerScript is a subagent that writes a file with the shell, taking
// its time, and reports.
func writerScript(name string) []turn {
	return []turn{
		{calls: [][2]string{{"shell", fmt.Sprintf(`{"command":"sleep 0.4; echo %s > %s.txt","writes":["%s.txt"]}`, name, name, name)}}},
		{check: expect("exit code 0"), text: name + " written"},
	}
}

// The root spawns two subagents in one turn; they run in parallel, each
// writes its own file, and each result reaches the root exactly once.
func TestSubagentsInParallel(t *testing.T) {
	if !hasShell() {
		t.Skip("no shell")
	}
	s := newSandbox(t,
		turn{calls: [][2]string{spawnCall("write a"), spawnCall("write b")}},
		turn{check: expect("a written", "b written"), text: "both done"},
	)
	s.model.sub["write a"] = writerScript("a")
	s.model.sub["write b"] = writerScript("b")
	start := time.Now()
	s.mustFinish()
	took := time.Since(start)
	if s.file("workspace/a.txt") != "a\n" || s.file("workspace/b.txt") != "b\n" {
		t.Errorf("files %q %q", s.file("workspace/a.txt"), s.file("workspace/b.txt"))
	}
	if took > 750*time.Millisecond {
		t.Errorf("took %v: the subagents didn't run in parallel", took)
	}
	if s.count(KindSpawn) != 2 || s.count(KindWorkerDone) != 2 {
		t.Errorf("%d spawns, %d finished", s.count(KindSpawn), s.count(KindWorkerDone))
	}
	delivered := 0
	for _, o := range s.outcomes() {
		if o.GetWorkerId() == RootWorker {
			delivered++
		}
	}
	if delivered != 2 {
		t.Errorf("%d results delivered to the root", delivered)
	}
	for _, w := range []string{"root.1", "root.2"} {
		if s.eventsOf(func(ev *gatewayv0.ActivityEvent) bool {
			return ev.GetWorker() == w && ev.GetSubagentSpawned().GetParent() == RootWorker
		}) != 1 ||
			s.eventsOf(func(ev *gatewayv0.ActivityEvent) bool { return ev.GetWorker() == w && ev.GetModelTurn() != nil }) != 2 ||
			s.eventsOf(func(ev *gatewayv0.ActivityEvent) bool { return ev.GetWorker() == w && ev.GetSubagentFinished() != nil }) != 1 {
			t.Errorf("activity of %s is incomplete", w)
		}
	}
	for _, m := range s.decoded(KindToolIntent) {
		if in := m.(*agentpb.ToolIntent); in.GetWorkerId() != "root.1" && in.GetWorkerId() != "root.2" {
			t.Errorf("tool intent of worker %q", in.GetWorkerId())
		}
	}
}

func TestSpawnLimits(t *testing.T) {
	tests := []struct {
		name   string
		limit  func(*Settings)
		turns  []turn
		sub    map[string][]turn
		errors []string // in the root's spawn results
	}{
		{
			name:   "count",
			limit:  func(s *Settings) { s.MaxSubagents = 1 },
			turns:  []turn{{calls: [][2]string{spawnCall("one"), spawnCall("two")}}, {text: "done"}},
			sub:    map[string][]turn{"one": {{text: "ok"}}, "two": {{text: "ok"}}},
			errors: []string{"may create at most 1 subagents"},
		},
		{
			name:  "depth",
			limit: func(s *Settings) { s.MaxSubagentDepth = 1 },
			turns: []turn{{calls: [][2]string{spawnCall("nest")}}, {check: expect("can't nest"), text: "done"}},
			sub: map[string][]turn{
				"nest": {{calls: [][2]string{spawnCall("deeper")}}, {check: expect("nest at most 1 deep"), text: "can't nest"}},
			},
		},
		{
			name:   "model",
			limit:  func(s *Settings) { s.Selectable = []string{"fast"} },
			turns:  []turn{{calls: [][2]string{{SpawnTool, `{"task":"x","model":"huge"}`}}}, {text: "done"}},
			errors: []string{`model "huge" isn't one you may choose; choose from fast`},
		},
		{
			name:   "no task",
			turns:  []turn{{calls: [][2]string{{SpawnTool, `{"task":" "}`}}}, {text: "done"}},
			errors: []string{"task is required"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newSandbox(t, tt.turns...)
			if tt.sub != nil {
				s.model.sub = tt.sub
			}
			if tt.limit != nil {
				tt.limit(&s.settings)
			}
			s.mustFinish()
			var errs []string
			for _, o := range s.outcomes() {
				if o.GetWorkerId() == RootWorker && o.GetIsError() {
					errs = append(errs, o.GetContent())
				}
			}
			if len(errs) != len(tt.errors) {
				t.Fatalf("errors %q, want %q", errs, tt.errors)
			}
			for i, e := range tt.errors {
				if !strings.Contains(errs[i], e) {
					t.Errorf("error %q lacks %q", errs[i], e)
				}
			}
		})
	}
}

// Crashes around a subagent: each recovers with the result delivered
// exactly once and no side effect repeated.
func TestSubagentCrashes(t *testing.T) {
	if !hasShell() {
		t.Skip("no shell")
	}
	tests := []struct {
		name, crashAt string
		childResult   string // what the child's shell call returned
	}{
		{"after spawn, before the child starts", "spawn@root", "exit code 0"},
		{"while the child runs a command", "tool.done:shell@root.1", "outcome_unknown"},
		{"after the child finished, before delivery", "worker.done@root.1", "exit code 0"},
		{"while delivering", "spawn.deliver@root", "exit code 0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newSandbox(t,
				turn{calls: [][2]string{spawnCall("append")}},
				turn{check: expect("appended"), text: "done"},
			)
			s.model.sub["append"] = []turn{
				{calls: [][2]string{{"shell", `{"command":"echo once >> side.txt"}`}}},
				{check: expect(tt.childResult), text: "appended"},
			}
			s.crashAt = tt.crashAt
			s.mustCrash()
			s.mustFinish()
			if got := s.file("workspace/side.txt"); got != "once\n" {
				t.Errorf("side.txt = %q", got)
			}
			if n := s.count(KindSpawn); n != 1 {
				t.Errorf("%d spawns", n)
			}
			if n := s.count(KindWorkerDone); n != 1 {
				t.Errorf("%d finished", n)
			}
			delivered := 0
			for _, o := range s.outcomes() {
				if o.GetWorkerId() == RootWorker {
					delivered++
				}
			}
			if delivered != 1 {
				t.Errorf("the result was delivered %d times", delivered)
			}
		})
	}
}

// Calls in one turn that write the same file run one after the other;
// calls that write different files run together.
func TestToolLocking(t *testing.T) {
	if !hasShell() {
		t.Skip("no shell")
	}
	sleepWrite := func(file string) [2]string {
		return [2]string{"shell", fmt.Sprintf(`{"command":"sleep 0.4; echo x >> %s","writes":["%s"]}`, file, file)}
	}
	tests := []struct {
		name     string
		calls    [][2]string
		min, max time.Duration
	}{
		{"same file", [][2]string{sleepWrite("x.txt"), sleepWrite("x.txt")}, 800 * time.Millisecond, 3 * time.Second},
		{"different files", [][2]string{sleepWrite("x.txt"), sleepWrite("y.txt")}, 0, 700 * time.Millisecond},
		{"undeclared writes", [][2]string{
			{"shell", `{"command":"sleep 0.4"}`}, sleepWrite("y.txt"),
		}, 800 * time.Millisecond, 3 * time.Second},
		{"reads run freely", [][2]string{
			{"shell", `{"command":"sleep 0.4"}`}, {"read_file", `{"path":"../task.json"}`}, {"glob", `{"pattern":"*"}`},
		}, 0, 700 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newSandbox(t, turn{calls: tt.calls}, turn{text: "done"})
			if err := s.folder.WriteTask(s.task()); err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			s.mustFinish()
			if took := time.Since(start); took < tt.min || took > tt.max {
				t.Errorf("took %v, want %v to %v", took, tt.min, tt.max)
			}
		})
	}

	// The tool concurrency limit holds parallel calls back too.
	s := newSandbox(t, turn{calls: [][2]string{sleepWrite("x.txt"), sleepWrite("y.txt")}}, turn{text: "done"})
	s.settings.MaxToolConcurrency = 1
	start := time.Now()
	s.mustFinish()
	if took := time.Since(start); took < 800*time.Millisecond {
		t.Errorf("took %v with one call at a time", took)
	}
}

// countNotes counts the operator notes in a conversation.
func countNotes(req *gatewayv0.InferenceRequest, note string) int {
	n := 0
	for _, m := range req.GetMessages() {
		for _, p := range m.GetParts() {
			if strings.Contains(p.GetText(), note) {
				n++
			}
		}
	}
	return n
}

func noteOnce(note string) func(*gatewayv0.InferenceRequest) error {
	return func(req *gatewayv0.InferenceRequest) error {
		if n := countNotes(req, note); n != 1 {
			return fmt.Errorf("the note %q appears %d times", note, n)
		}
		last := req.GetMessages()[len(req.GetMessages())-1]
		if last.GetRole() != gatewayv0.Role_ROLE_USER {
			return fmt.Errorf("the conversation ends with a %v message", last.GetRole())
		}
		return nil
	}
}

// A steer note reaches the agent at its next turn boundary, marked as the
// operator's, exactly once, also across a crash and a pause.
func TestSteerNotes(t *testing.T) {
	s := newSandbox(t,
		turn{check: noteOnce("metric units"), calls: [][2]string{{"glob", `{"pattern":"*"}`}}},
		turn{check: func(req *gatewayv0.InferenceRequest) error {
			if err := noteOnce("metric units")(req); err != nil {
				return err
			}
			if !strings.Contains(lastResults(req)[0].GetContent(), "no files") {
				return fmt.Errorf("tool result missing")
			}
			return noteOnce("[Operator note from alice")(req)
		}, calls: [][2]string{{"glob", `{"pattern":"*"}`}}},
		turn{check: noteOnce("also add a summary"), text: "done"},
	)
	s.steers = []Steer{{CommandID: "c1", Note: "use metric units", From: "alice"}}
	// Crash right after the note joined the conversation: it isn't given
	// twice.
	s.crashAfter = func(kind string, _ proto.Message) bool { return kind == KindSteer }
	s.mustCrash()

	// A pause after the first turn, and a note that arrives meanwhile.
	stopping := make(chan struct{})
	s.model.turns[0].check = func(req *gatewayv0.InferenceRequest) error {
		close(stopping)
		return noteOnce("metric units")(req)
	}
	if done, err, crashed := s.run(t.Context(), t.Context(), stopping); done || err != nil || crashed {
		t.Fatalf("paused run: %v %v %v", done, err, crashed)
	}
	s.mu.Lock()
	s.steers = append(s.steers, Steer{CommandID: "c2", Note: "also add a summary"})
	s.mu.Unlock()
	s.mustFinish()
	if n := s.count(KindSteer); n != 2 {
		t.Errorf("%d notes delivered, want 2", n)
	}
}

// Subagents see their own steer-free conversation; only the root gets
// operator notes.
func TestSteerGoesToRoot(t *testing.T) {
	s := newSandbox(t,
		turn{calls: [][2]string{spawnCall("child task")}},
		turn{text: "done"},
	)
	s.model.sub["child task"] = []turn{{check: func(req *gatewayv0.InferenceRequest) error {
		if countNotes(req, "note") != 0 {
			return fmt.Errorf("the subagent got the operator note")
		}
		return nil
	}, text: "ok"}}
	s.steers = []Steer{{CommandID: "c1", Note: "a note"}}
	s.mustFinish()
}
