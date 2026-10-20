package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/actlet/actlet/internal/agent/agentpb"
	"github.com/actlet/actlet/internal/gwclient"
	"github.com/actlet/actlet/internal/journal"
	"github.com/actlet/actlet/internal/sessionfs"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// turn is a scripted model answer.
type turn struct {
	text  string
	calls [][2]string // tool name, JSON input
	stop  gatewayv0.StopReason
	// check looks at the request; an error fails the call.
	check func(req *gatewayv0.InferenceRequest) error
	// block holds the answer until the context ends.
	block bool
}

// model answers each worker with the turn for the number of answers in
// its conversation, like the scripted model. A subagent's script is
// chosen by a phrase in its subtask.
type model struct {
	turns []turn
	sub   map[string][]turn

	mu    sync.Mutex
	calls []call
	llm   []*gatewayv0.InferenceRequest // the llm tool's calls
}

type call struct {
	intent, script string
	turn           int
}

func (m *model) infer(ctx context.Context, req *gatewayv0.InferenceRequest) (*gwclient.Inference, error) {
	if req.GetPurpose() == "llm" {
		m.mu.Lock()
		m.llm = append(m.llm, req)
		m.mu.Unlock()
		return &gwclient.Inference{Attempts: 1,
			Message: &gatewayv0.Message{Role: gatewayv0.Role_ROLE_ASSISTANT, Parts: []*gatewayv0.Part{{Part: &gatewayv0.Part_Text{Text: "LLM says: " + messageText(req.GetMessages()[0])}}}},
			Done:    &gatewayv0.InferenceDone{StopReason: gatewayv0.StopReason_STOP_REASON_END_TURN, Usage: &gatewayv0.Usage{InputTokens: 100, OutputTokens: 10, Cost: 0.5, Currency: "EUR"}}}, nil
	}
	n := 0
	for _, msg := range req.GetMessages() {
		if msg.GetRole() == gatewayv0.Role_ROLE_ASSISTANT {
			n++
		}
	}
	turns, script := m.turns, "root"
	first := req.GetMessages()[0].GetParts()[0].GetText()
	for phrase, ts := range m.sub {
		if strings.Contains(first, phrase) {
			turns, script = ts, phrase
		}
	}
	m.mu.Lock()
	m.calls = append(m.calls, call{req.GetIntentId(), script, n + 1})
	m.mu.Unlock()
	if n >= len(turns) {
		return nil, fmt.Errorf("%s: script exhausted", script)
	}
	t := turns[n]
	if t.check != nil {
		if err := t.check(req); err != nil {
			return nil, fmt.Errorf("%s turn %d: %w", script, n+1, err)
		}
	}
	if t.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	msg := &gatewayv0.Message{Role: gatewayv0.Role_ROLE_ASSISTANT}
	if t.text != "" {
		msg.Parts = append(msg.Parts, &gatewayv0.Part{Part: &gatewayv0.Part_Text{Text: t.text}})
	}
	for i, c := range t.calls {
		msg.Parts = append(msg.Parts, &gatewayv0.Part{Part: &gatewayv0.Part_ToolCall{ToolCall: &gatewayv0.ToolCall{
			Id: fmt.Sprintf("call_%d_%d", n+1, i+1), Name: c[0], Input: c[1],
		}}})
	}
	stop := t.stop
	if stop == gatewayv0.StopReason_STOP_REASON_UNSPECIFIED {
		stop = gatewayv0.StopReason_STOP_REASON_END_TURN
		if len(t.calls) > 0 {
			stop = gatewayv0.StopReason_STOP_REASON_TOOL_USE
		}
	}
	return &gwclient.Inference{Message: msg, Attempts: 1, Done: &gatewayv0.InferenceDone{
		StopReason: stop, Continuation: []byte{byte(n + 1)}, Usage: &gatewayv0.Usage{InputTokens: 10, OutputTokens: 2},
	}}, nil
}

func (m *model) callsOf(script string) []call {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []call
	for _, c := range m.calls {
		if c.script == script {
			out = append(out, c)
		}
	}
	return out
}

// lastResults returns the tool results the request carries.
func lastResults(req *gatewayv0.InferenceRequest) []*gatewayv0.ToolResult {
	msgs := req.GetMessages()
	var out []*gatewayv0.ToolResult
	for _, p := range msgs[len(msgs)-1].GetParts() {
		if r := p.GetToolResult(); r != nil {
			out = append(out, r)
		}
	}
	return out
}

// expect checks the tool results of the previous turn.
func expect(want ...string) func(*gatewayv0.InferenceRequest) error {
	return func(req *gatewayv0.InferenceRequest) error {
		got := lastResults(req)
		if len(got) != len(want) {
			return fmt.Errorf("%d tool results, want %d", len(got), len(want))
		}
		for i, w := range want {
			if !strings.Contains(got[i].GetContent(), w) {
				return fmt.Errorf("tool result %q lacks %q", got[i].GetContent(), w)
			}
		}
		return nil
	}
}

var errCrash = errors.New("simulated crash")

// sandbox is a session folder with an in-memory journal.
type sandbox struct {
	t       *testing.T
	folder  sessionfs.Folder
	model   *model
	mu      sync.Mutex
	records []journal.Record
	events  []*gatewayv0.ActivityEvent
	steers  []Steer
	crashed bool
	// A simulated crash: at the crash point crashAt, or before or after a
	// matching record is stored. Nothing more reaches the journal after
	// it, and the worker at a crash point stops right there.
	crashAt                 string
	crashBefore, crashAfter func(kind string, m proto.Message) bool
	settings                Settings
	budget                  Budget
	callTool                CallToolFunc
}

func newSandbox(t *testing.T, turns ...turn) *sandbox {
	t.Helper()
	f, err := sessionfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &sandbox{t: t, folder: f, model: &model{turns: turns, sub: map[string][]turn{}}, settings: Settings{
		Model: "main", SubagentModel: "sub", MaxWorkers: 4, MaxToolConcurrency: 4, MaxSubagentDepth: 2, MaxSubagents: 8,
	}}
}

func (s *sandbox) task() *sessionfs.Task {
	return &sessionfs.Task{Schema: "v0", ID: "t1", Kind: Kind, Input: json.RawMessage(`"count the numbers"`)}
}

// sinceCheckpoint returns the records from the latest checkpoint on.
func (s *sandbox) sinceCheckpoint() []journal.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	start := 0
	for i, r := range s.records {
		if r.Kind == KindCheckpoint {
			start = i
		}
	}
	return append([]journal.Record(nil), s.records[start:]...)
}

func (s *sandbox) record(_ context.Context, kind string, m proto.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.crashed {
		return errCrash
	}
	if s.crashBefore != nil && s.crashBefore(kind, m) {
		s.crashed, s.crashBefore = true, nil
		return errCrash
	}
	data, err := proto.Marshal(m)
	if err != nil {
		return err
	}
	s.records = append(s.records, journal.Record{Kind: kind, Data: data, Seq: uint64(len(s.records) + 1)})
	if s.crashAfter != nil && s.crashAfter(kind, m) {
		s.crashed, s.crashAfter = true, nil
	}
	return nil
}

// run runs the agent once; crashed reports a simulated crash.
func (s *sandbox) run(soft, hard context.Context, stopping <-chan struct{}) (done bool, err error, crashed bool) {
	if stopping == nil {
		stopping = make(chan struct{})
	}
	s.mu.Lock()
	s.crashed = false
	s.mu.Unlock()
	crashHook = func(point string) bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.crashAt != "" && point == s.crashAt {
			s.crashed, s.crashAt = true, ""
			return true
		}
		return false
	}
	defer func() { crashHook = nil }()
	done, err = Run(Env{
		Settings: s.settings, Folder: s.folder, Task: s.task(), SessionID: "s", Epoch: 1,
		Records: s.sinceCheckpoint(), Record: s.record,
		Emit: func(ev *gatewayv0.ActivityEvent) {
			s.mu.Lock()
			s.events = append(s.events, ev)
			s.mu.Unlock()
		},
		Infer: s.model.infer, Soft: soft, Hard: hard, Stopping: stopping, Budget: s.budget, CallTool: s.callTool,
		Steers: func() []Steer {
			s.mu.Lock()
			defer s.mu.Unlock()
			return append([]Steer(nil), s.steers...)
		},
		Logger: slog.New(slog.DiscardHandler),
	})
	s.mu.Lock()
	crashed = s.crashed
	s.mu.Unlock()
	if crashed {
		return false, nil, true
	}
	return done, err, false
}

func (s *sandbox) mustCrash() {
	s.t.Helper()
	if done, err, crashed := s.run(s.t.Context(), s.t.Context(), nil); !crashed {
		s.t.Fatalf("didn't crash: done %v, err %v", done, err)
	}
}

func (s *sandbox) mustFinish() {
	s.t.Helper()
	done, err, crashed := s.run(s.t.Context(), s.t.Context(), nil)
	if crashed || !done || err != nil {
		s.t.Fatalf("crashed %v, done %v, err %v", crashed, done, err)
	}
}

func (s *sandbox) count(kind string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.records {
		if r.Kind == kind {
			n++
		}
	}
	return n
}

// decoded returns the records of a kind, decoded.
func (s *sandbox) decoded(kind string) []proto.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []proto.Message
	for _, r := range s.records {
		if r.Kind == kind {
			m, err := decode(r)
			if err != nil {
				s.t.Fatal(err)
			}
			out = append(out, m)
		}
	}
	return out
}

func (s *sandbox) outcomes() []*agentpb.ToolOutcome {
	var out []*agentpb.ToolOutcome
	for _, m := range s.decoded(KindToolOutcome) {
		out = append(out, m.(*agentpb.ToolOutcome))
	}
	return out
}

func (s *sandbox) eventsOf(match func(*gatewayv0.ActivityEvent) bool) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, ev := range s.events {
		if match(ev) {
			n++
		}
	}
	return n
}

func (s *sandbox) file(rel string) string {
	data, err := os.ReadFile(filepath.Join(s.folder.Dir, rel))
	if err != nil {
		return ""
	}
	return string(data)
}

func hasShell() bool {
	_, err := exec.LookPath("sh")
	return err == nil
}

func TestAgentCompletesTask(t *testing.T) {
	if !hasShell() {
		t.Skip("no shell")
	}
	s := newSandbox(t,
		turn{text: "Reading the input.", calls: [][2]string{{"read_file", `{"path":"../inputs/numbers.txt"}`}}},
		turn{check: expect("     1\t3"), calls: [][2]string{
			{"write_file", `{"path":"sum.sh","content":"echo $((3 + 4))\n"}`},
			{"shell", `{"command":"sh sum.sh > total.txt; cat total.txt"}`},
		}},
		turn{check: expect("created sum.sh", "stdout:\n7"), calls: [][2]string{
			{"edit_file", `{"path":"sum.sh","old_string":"3 + 4","new_string":"3 + 4 + 0"}`},
			{"write_file", `{"path":"../output/report.md","content":"The sum is 7.\n"}`},
			{"write_file", `{"path":"../inputs/numbers.txt","content":"hacked"}`},
		}},
		turn{check: expect("replaced 1 occurrence", "created ../output/report.md", "read-only"), text: "The sum is 7."},
	)
	if err := os.WriteFile(filepath.Join(s.folder.Inputs(), "numbers.txt"), []byte("3\n4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.mustFinish()
	if s.file("output/report.md") != "The sum is 7.\n" || s.file("inputs/numbers.txt") != "3\n4\n" || s.file("workspace/total.txt") != "7\n" {
		t.Errorf("files: report %q, input %q, total %q", s.file("output/report.md"), s.file("inputs/numbers.txt"), s.file("workspace/total.txt"))
	}
	if s.count(KindInferenceIntent) != 4 || s.count(KindInferenceOutcome) != 4 {
		t.Errorf("records: %d model intents, %d answers", s.count(KindInferenceIntent), s.count(KindInferenceOutcome))
	}
	if s.count(KindToolIntent) != 5 || s.count(KindToolOutcome) != 6 { // the refused write has no intent
		t.Errorf("records: %d tool intents, %d outcomes", s.count(KindToolIntent), s.count(KindToolOutcome))
	}
	for _, o := range s.outcomes() {
		if o.GetWorkerId() != RootWorker {
			t.Errorf("outcome without the worker: %v", o)
		}
	}
	turns := s.eventsOf(func(ev *gatewayv0.ActivityEvent) bool {
		return ev.GetModelTurn() != nil && ev.GetWorker() == RootWorker
	})
	finished := s.eventsOf(func(ev *gatewayv0.ActivityEvent) bool { return ev.GetToolFinished() != nil })
	diffs := s.eventsOf(func(ev *gatewayv0.ActivityEvent) bool {
		return strings.Contains(ev.GetFileChange().GetDiff(), "-echo $((3 + 4))\n+echo $((3 + 4 + 0))")
	})
	if turns != 4 || finished != 6 || diffs != 1 {
		t.Errorf("activity: %d model turns, %d tool results, %d edit diffs", turns, finished, diffs)
	}
}

func TestAgentWritesDeliverableIfNone(t *testing.T) {
	s := newSandbox(t, turn{text: "Nothing to do: the answer is 42."})
	s.mustFinish()
	if got := s.file("output/result.md"); got != "Nothing to do: the answer is 42.\n" {
		t.Errorf("result.md = %q", got)
	}
}

func TestAgentPrompt(t *testing.T) {
	s := newSandbox(t, turn{check: func(req *gatewayv0.InferenceRequest) error {
		sys, first := req.GetSystem(), req.GetMessages()[0].GetParts()[0].GetText()
		for _, want := range []string{"inside Actlet", "## Instructions\n\nBe brief.", "## What is installed\n\njq is installed."} {
			if !strings.Contains(sys, want) {
				return errors.New("system prompt lacks " + want)
			}
		}
		if strings.Contains(sys, "You are a subagent") {
			return errors.New("the root is told it is a subagent")
		}
		if !strings.Contains(first, "# Task t1\n\ncount the numbers") || !strings.Contains(first, "- a.txt") {
			return errors.New("task message: " + first)
		}
		var names []string
		for _, tl := range req.GetTools() {
			names = append(names, tl.GetName())
		}
		if req.GetModel() != "large" || !strings.Contains(strings.Join(names, ","), SpawnTool) {
			return fmt.Errorf("model %s, tools %v", req.GetModel(), names)
		}
		return nil
	}, text: "ok"})
	s.settings.Model = "large"
	s.settings.Prompt = Prompt{Instructions: "Be brief.", Manifests: []string{"", "jq is installed."}}
	if err := os.WriteFile(filepath.Join(s.folder.Inputs(), "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.mustFinish()
}

// A crash after a tool call's intent was recorded, before it ran: a call
// that is safe to repeat runs again; a shell command is never run again
// and its outcome is unknown.
func TestCrashAfterToolIntent(t *testing.T) {
	if !hasShell() {
		t.Skip("no shell")
	}
	for _, tt := range []struct {
		tool, input string
		want        string // in the result the model gets
		ran         bool   // the side effect happened
	}{
		{"shell", `{"command":"echo once >> side.txt"}`, "outcome_unknown", false},
		{"write_file", `{"path":"side.txt","content":"once\n"}`, "created side.txt", true},
		{"read_file", `{"path":"../task.json"}`, "\"kind\"", false},
	} {
		t.Run(tt.tool, func(t *testing.T) {
			s := newSandbox(t,
				turn{calls: [][2]string{{tt.tool, tt.input}}},
				turn{check: expect(tt.want), text: "done"},
			)
			if err := s.folder.WriteTask(s.task()); err != nil {
				t.Fatal(err)
			}
			s.crashAt = "tool.intent:" + tt.tool
			s.mustCrash()
			s.mustFinish()
			if got := s.file("workspace/side.txt"); (got == "once\n") != tt.ran || strings.Count(got, "once") > 1 {
				t.Errorf("side.txt = %q", got)
			}
			if o := s.outcomes(); len(o) != 1 || (tt.tool == "shell") != o[0].GetOutcomeUnknown() {
				t.Errorf("outcomes %v", o)
			}
			if s.count(KindToolIntent) != 1 {
				t.Errorf("%d intents: a repeated call needs no new intent", s.count(KindToolIntent))
			}
		})
	}
}

// A crash after a shell command ran but before its outcome was recorded:
// the side effect happened once, and the model is told the outcome is
// unknown rather than the command running again.
func TestCrashAfterShellSideEffect(t *testing.T) {
	if !hasShell() {
		t.Skip("no shell")
	}
	s := newSandbox(t,
		turn{calls: [][2]string{{"shell", `{"command":"echo once >> side.txt"}`}}},
		turn{check: expect("outcome_unknown"), text: "checked"},
	)
	s.crashAt = "tool.done:shell"
	s.mustCrash()
	s.mustFinish()
	if got := s.file("workspace/side.txt"); got != "once\n" {
		t.Errorf("side.txt = %q", got)
	}
	if o := s.outcomes(); len(o) != 1 || !o[0].GetOutcomeUnknown() || !o[0].GetRecovered() {
		t.Errorf("outcomes %v", o)
	}
}

// A crash during a model call: the next run makes the call again with the
// same intent ID, from the same conversation.
func TestCrashDuringInference(t *testing.T) {
	s := newSandbox(t,
		turn{calls: [][2]string{{"glob", `{"pattern":"*"}`}}},
		turn{check: expect("no files match"), text: "done"},
	)
	s.crashBefore = func(kind string, _ proto.Message) bool {
		return kind == KindInferenceOutcome && len(s.model.callsOf("root")) == 2
	}
	s.mustCrash()
	s.mustFinish()
	c := s.model.callsOf("root")
	if len(c) != 3 || c[1].intent != c[2].intent || c[0].intent == c[1].intent || c[2].turn != 2 {
		t.Errorf("calls %v", c)
	}
	if s.count(KindInferenceIntent) != 2 {
		t.Errorf("%d model intents", s.count(KindInferenceIntent))
	}
}

// Pausing while a tool runs: drain lets it finish, interrupt stops it.
// Either way the next run carries on from there.
func TestPauseDuringTool(t *testing.T) {
	if !hasShell() {
		t.Skip("no shell")
	}
	for _, interrupt := range []bool{false, true} {
		name := "drain"
		if interrupt {
			name = "interrupt"
		}
		t.Run(name, func(t *testing.T) {
			wantResult := "exit code 0"
			if interrupt {
				wantResult = "interrupted"
			}
			s := newSandbox(t,
				turn{calls: [][2]string{
					{"shell", `{"command":"echo yes > started; sleep 1; echo finished > finished"}`},
					{"shell", `{"command":"echo second > second"}`},
				}},
				turn{check: expect(wantResult, "exit code 0"), text: "done"},
			)
			soft, cancelSoft := context.WithCancel(t.Context())
			stopping := make(chan struct{})
			go func() {
				for s.file("workspace/started") == "" {
					time.Sleep(5 * time.Millisecond)
				}
				close(stopping)
				if interrupt {
					cancelSoft()
				}
			}()
			done, err, _ := s.run(soft, t.Context(), stopping)
			if done || err != nil {
				t.Fatalf("first run: %v %v", done, err)
			}
			if (s.file("workspace/finished") != "") == interrupt || s.file("workspace/second") != "" {
				t.Errorf("after pausing: finished %q, second %q", s.file("workspace/finished"), s.file("workspace/second"))
			}
			s.mustFinish()
			if s.file("workspace/second") != "second\n" {
				t.Error("the second call didn't run after resuming")
			}
		})
	}
}

// Pausing during a model call: interrupt abandons it, and the next run
// makes the same call again.
func TestPauseDuringInference(t *testing.T) {
	s := newSandbox(t, turn{block: true}, turn{text: "never"})
	soft, cancel := context.WithCancel(t.Context())
	stopping := make(chan struct{})
	time.AfterFunc(50*time.Millisecond, func() { close(stopping); cancel() })
	done, err, _ := s.run(soft, t.Context(), stopping)
	if done || err != nil {
		t.Fatal(done, err)
	}
	s.model.turns[0] = turn{text: "done"}
	s.mustFinish()
	if c := s.model.callsOf("root"); len(c) != 2 || c[0].intent != c[1].intent {
		t.Errorf("calls %v", c)
	}
}

func TestCheckpointCadence(t *testing.T) {
	var turns []turn
	for range 5 {
		turns = append(turns, turn{calls: [][2]string{{"glob", `{"pattern":"*.none"}`}}})
	}
	turns = append(turns, turn{text: "done"})
	s := newSandbox(t, turns...)
	s.settings.CheckpointEvery = 2
	// Crash in the middle; recovery replays the turns since the checkpoint.
	s.crashBefore = func(kind string, _ proto.Message) bool {
		return kind == KindInferenceIntent && len(s.model.callsOf("root")) == 3
	}
	s.mustCrash()
	s.mustFinish()
	// The initial one, then after turns 2, 4 and 6.
	if n := s.count(KindCheckpoint); n != 4 {
		t.Errorf("%d checkpoints", n)
	}
	if n := s.count(KindInferenceOutcome); n != 6 {
		t.Errorf("%d answers", n)
	}
}

func TestAgentEnds(t *testing.T) {
	tests := []struct {
		name  string
		turns []turn
		def   string
		err   string
	}{
		{"refusal", []turn{{text: "no", stop: gatewayv0.StopReason_STOP_REASON_REFUSAL}}, "", "refused"},
		{"turn limit", []turn{{calls: [][2]string{{"glob", `{"pattern":"x"}`}}}, {calls: [][2]string{{"glob", `{"pattern":"x"}`}}}}, `{"max_turns":2}`, "all 2 model turns"},
		{"cut off", []turn{{text: "part", stop: gatewayv0.StopReason_STOP_REASON_MAX_TOKENS}, {check: func(req *gatewayv0.InferenceRequest) error {
			msgs := req.GetMessages()
			if !strings.Contains(msgs[len(msgs)-1].GetParts()[0].GetText(), "cut off") {
				return errors.New("no continue message")
			}
			return nil
		}, text: "rest"}}, "", ""},
		{"model failure", []turn{{check: func(*gatewayv0.InferenceRequest) error { return errors.New("no such model") }}}, "", "no such model"},
		{"bad definition", nil, `{"colour":"red"}`, "unknown field"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newSandbox(t, tt.turns...)
			_, err := Run(Env{
				Settings: s.settings, Folder: s.folder,
				Task:   &sessionfs.Task{ID: "t", Kind: Kind, Definition: json.RawMessage(tt.def)},
				Record: func(context.Context, string, proto.Message) error { return nil },
				Emit:   func(*gatewayv0.ActivityEvent) {},
				Infer:  s.model.infer,
				Soft:   t.Context(), Hard: t.Context(), Stopping: make(chan struct{}),
			})
			if (err == nil) != (tt.err == "") || (err != nil && !strings.Contains(err.Error(), tt.err)) {
				t.Fatalf("err = %v, want %q", err, tt.err)
			}
		})
	}
}
