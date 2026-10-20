package agent

import (
	"context"
	"encoding/json"
	"errors"
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

// model answers with the turn for the number of answers in the
// conversation, like the scripted model.
type model struct {
	turns []turn

	mu      sync.Mutex
	intents []string
	turnOf  []int
}

func (m *model) infer(ctx context.Context, req *gatewayv0.InferenceRequest) (*gwclient.Inference, error) {
	n := 0
	for _, msg := range req.GetMessages() {
		if msg.GetRole() == gatewayv0.Role_ROLE_ASSISTANT {
			n++
		}
	}
	m.mu.Lock()
	m.intents = append(m.intents, req.GetIntentId())
	m.turnOf = append(m.turnOf, n+1)
	m.mu.Unlock()
	if n >= len(m.turns) {
		return nil, errors.New("script exhausted")
	}
	t := m.turns[n]
	if t.check != nil {
		if err := t.check(req); err != nil {
			return nil, err
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
			Id: "call_" + string(rune('a'+n)) + string(rune('0'+i)), Name: c[0], Input: c[1],
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
			return errors.New("wrong number of tool results")
		}
		for i, w := range want {
			if !strings.Contains(got[i].GetContent(), w) {
				return errors.New("tool result " + got[i].GetContent() + " lacks " + w)
			}
		}
		return nil
	}
}

type crash struct{}

// box is a session folder with an in-memory journal.
type box struct {
	t       *testing.T
	folder  sessionfs.Folder
	model   *model
	records []journal.Record
	events  []*gatewayv0.ActivityEvent
	evMu    sync.Mutex
	// crashBefore and crashAfter end a run abruptly before or after a
	// record is stored, as a crash would.
	crashBefore, crashAfter func(kind string, m proto.Message) bool
	settings                Settings
}

func newBox(t *testing.T, input string, turns ...turn) *box {
	t.Helper()
	f, err := sessionfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &box{t: t, folder: f, model: &model{turns: turns}, settings: Settings{Model: "main"}}
}

func (b *box) task(input string) *sessionfs.Task {
	in, _ := json.Marshal(input)
	return &sessionfs.Task{Schema: "v0", ID: "t1", Kind: Kind, Input: in}
}

// sinceCheckpoint returns the records from the latest checkpoint on.
func (b *box) sinceCheckpoint() []journal.Record {
	start := 0
	for i, r := range b.records {
		if r.Kind == KindCheckpoint {
			start = i
		}
	}
	return append([]journal.Record(nil), b.records[start:]...)
}

// run runs the agent once; crashed reports an abrupt end.
func (b *box) run(soft, hard context.Context, wantsStop func() bool) (done bool, err error, crashed bool) {
	if wantsStop == nil {
		wantsStop = func() bool { return false }
	}
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(crash); !ok {
				panic(r)
			}
			crashed = true
		}
	}()
	done, err = Run(Env{
		Settings: b.settings, Folder: b.folder, Task: b.task("count the numbers"), SessionID: "s", Epoch: 1,
		Records: b.sinceCheckpoint(),
		Record: func(_ context.Context, kind string, m proto.Message) error {
			if b.crashBefore != nil && b.crashBefore(kind, m) {
				b.crashBefore = nil
				panic(crash{})
			}
			data, err := proto.Marshal(m)
			if err != nil {
				return err
			}
			b.records = append(b.records, journal.Record{Kind: kind, Data: data, Seq: uint64(len(b.records) + 1)})
			if b.crashAfter != nil && b.crashAfter(kind, m) {
				b.crashAfter = nil
				panic(crash{})
			}
			return nil
		},
		Emit: func(ev *gatewayv0.ActivityEvent) {
			b.evMu.Lock()
			b.events = append(b.events, ev)
			b.evMu.Unlock()
		},
		Infer: b.model.infer, Soft: soft, Hard: hard, WantsStop: wantsStop,
		Logger: slog.New(slog.DiscardHandler),
	})
	return done, err, false
}

func (b *box) runToEnd() (bool, error) {
	b.t.Helper()
	done, err, crashed := b.run(b.t.Context(), b.t.Context(), nil)
	if crashed {
		b.t.Fatal("unexpected crash")
	}
	return done, err
}

func (b *box) count(kind string) int {
	n := 0
	for _, r := range b.records {
		if r.Kind == kind {
			n++
		}
	}
	return n
}

func (b *box) outcomes() []*agentpb.ToolOutcome {
	var out []*agentpb.ToolOutcome
	for _, r := range b.records {
		if r.Kind == KindToolOutcome {
			o := &agentpb.ToolOutcome{}
			if err := proto.Unmarshal(r.Data, o); err != nil {
				b.t.Fatal(err)
			}
			out = append(out, o)
		}
	}
	return out
}

func (b *box) eventsOf(match func(*gatewayv0.ActivityEvent) bool) int {
	b.evMu.Lock()
	defer b.evMu.Unlock()
	n := 0
	for _, ev := range b.events {
		if match(ev) {
			n++
		}
	}
	return n
}

func (b *box) file(rel string) string {
	data, err := os.ReadFile(filepath.Join(b.folder.Dir, rel))
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
	b := newBox(t, "",
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
	if err := os.WriteFile(filepath.Join(b.folder.Inputs(), "numbers.txt"), []byte("3\n4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	done, err := b.runToEnd()
	if err != nil || !done {
		t.Fatalf("done %v, err %v", done, err)
	}
	if b.file("output/report.md") != "The sum is 7.\n" || b.file("inputs/numbers.txt") != "3\n4\n" || b.file("workspace/total.txt") != "7\n" {
		t.Errorf("files: report %q, input %q, total %q", b.file("output/report.md"), b.file("inputs/numbers.txt"), b.file("workspace/total.txt"))
	}
	// One checkpoint to start and one per turn; an intent and outcome per call.
	if b.count(KindCheckpoint) != 4 || b.count(KindInferenceIntent) != 4 || b.count(KindInferenceOutcome) != 4 {
		t.Errorf("records: %d checkpoints, %d model intents, %d answers", b.count(KindCheckpoint), b.count(KindInferenceIntent), b.count(KindInferenceOutcome))
	}
	if b.count(KindToolIntent) != 5 || b.count(KindToolOutcome) != 6 { // the refused write has no intent
		t.Errorf("records: %d tool intents, %d outcomes", b.count(KindToolIntent), b.count(KindToolOutcome))
	}
	turns := b.eventsOf(func(ev *gatewayv0.ActivityEvent) bool { return ev.GetModelTurn() != nil })
	finished := b.eventsOf(func(ev *gatewayv0.ActivityEvent) bool { return ev.GetToolFinished() != nil })
	diffs := b.eventsOf(func(ev *gatewayv0.ActivityEvent) bool {
		return strings.Contains(ev.GetFileChange().GetDiff(), "-echo $((3 + 4))\n+echo $((3 + 4 + 0))")
	})
	if turns != 4 || finished != 6 || diffs != 1 {
		t.Errorf("activity: %d model turns, %d tool results, %d edit diffs", turns, finished, diffs)
	}
}

func TestAgentWritesDeliverableIfNone(t *testing.T) {
	b := newBox(t, "", turn{text: "Nothing to do: the answer is 42."})
	if done, err := b.runToEnd(); !done || err != nil {
		t.Fatal(done, err)
	}
	if got := b.file("output/result.md"); got != "Nothing to do: the answer is 42.\n" {
		t.Errorf("result.md = %q", got)
	}
}

func TestAgentPrompt(t *testing.T) {
	b := newBox(t, "", turn{check: func(req *gatewayv0.InferenceRequest) error {
		sys, first := req.GetSystem(), req.GetMessages()[0].GetParts()[0].GetText()
		for _, want := range []string{"inside Actlet", "## Instructions\n\nBe brief.", "## What is installed\n\njq is installed."} {
			if !strings.Contains(sys, want) {
				return errors.New("system prompt lacks " + want)
			}
		}
		if !strings.Contains(first, "# Task t1\n\ncount the numbers") || !strings.Contains(first, "- a.txt") {
			return errors.New("task message: " + first)
		}
		if req.GetModel() != "large" || len(req.GetTools()) < 5 {
			return errors.New("model or tools")
		}
		return nil
	}, text: "ok"})
	b.settings = Settings{Model: "large", Prompt: Prompt{Instructions: "Be brief.", Manifests: []string{"", "jq is installed."}}}
	if err := os.WriteFile(filepath.Join(b.folder.Inputs(), "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if done, err := b.runToEnd(); !done || err != nil {
		t.Fatal(done, err)
	}
}

func isTool(kind, tool string) func(string, proto.Message) bool {
	return func(k string, m proto.Message) bool {
		if k != kind {
			return false
		}
		switch m := m.(type) {
		case *agentpb.ToolIntent:
			return m.GetTool() == tool
		case *agentpb.ToolOutcome:
			return strings.HasPrefix(m.GetCallId(), "call_") && tool != "" // any outcome
		}
		return false
	}
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
			b := newBox(t, "",
				turn{calls: [][2]string{{tt.tool, tt.input}}},
				turn{check: expect(tt.want), text: "done"},
			)
			if err := b.folder.WriteTask(b.task("x")); err != nil {
				t.Fatal(err)
			}
			b.crashAfter = isTool(KindToolIntent, tt.tool)
			if _, _, crashed := b.run(t.Context(), t.Context(), nil); !crashed {
				t.Fatal("didn't crash")
			}
			if done, err := b.runToEnd(); !done || err != nil {
				t.Fatalf("after recovery: %v %v", done, err)
			}
			if got := b.file("workspace/side.txt"); (got == "once\n") != tt.ran || strings.Count(got, "once") > 1 {
				t.Errorf("side.txt = %q", got)
			}
			if o := b.outcomes(); len(o) != 1 || (tt.tool == "shell") != o[0].GetOutcomeUnknown() {
				t.Errorf("outcomes %v", o)
			}
			if b.count(KindToolIntent) != 1 {
				t.Errorf("%d intents: a repeated call needs no new intent", b.count(KindToolIntent))
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
	b := newBox(t, "",
		turn{calls: [][2]string{{"shell", `{"command":"echo once >> side.txt"}`}}},
		turn{check: expect("outcome_unknown"), text: "checked"},
	)
	b.crashBefore = isTool(KindToolOutcome, "shell")
	if _, _, crashed := b.run(t.Context(), t.Context(), nil); !crashed {
		t.Fatal("didn't crash")
	}
	if done, err := b.runToEnd(); !done || err != nil {
		t.Fatal(done, err)
	}
	if got := b.file("workspace/side.txt"); got != "once\n" {
		t.Errorf("side.txt = %q", got)
	}
	if o := b.outcomes(); len(o) != 1 || !o[0].GetOutcomeUnknown() || !o[0].GetRecovered() {
		t.Errorf("outcomes %v", o)
	}
}

// A crash during a model call: the next run makes the call again with the
// same intent ID, from the same conversation.
func TestCrashDuringInference(t *testing.T) {
	b := newBox(t, "",
		turn{calls: [][2]string{{"glob", `{"pattern":"*"}`}}},
		turn{check: expect("no files match"), text: "done"},
	)
	b.crashBefore = func(kind string, _ proto.Message) bool {
		return kind == KindInferenceOutcome && len(b.model.intents) == 2
	}
	if _, _, crashed := b.run(t.Context(), t.Context(), nil); !crashed {
		t.Fatal("didn't crash")
	}
	if done, err := b.runToEnd(); !done || err != nil {
		t.Fatal(done, err)
	}
	m := b.model
	if len(m.intents) != 3 || m.intents[1] != m.intents[2] || m.intents[0] == m.intents[1] || m.turnOf[2] != 2 {
		t.Errorf("intents %v for turns %v", m.intents, m.turnOf)
	}
	if b.count(KindInferenceIntent) != 2 {
		t.Errorf("%d model intents", b.count(KindInferenceIntent))
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
			b := newBox(t, "",
				turn{calls: [][2]string{
					{"shell", `{"command":"echo yes > started; sleep 1; echo finished > finished"}`},
					{"shell", `{"command":"echo second > second"}`},
				}},
				turn{check: expect(wantResult, "exit code 0"), text: "done"},
			)
			soft, cancelSoft := context.WithCancel(t.Context())
			var paused sync.Once
			var pausing bool
			var mu sync.Mutex
			go func() {
				for b.file("workspace/started") == "" {
					time.Sleep(5 * time.Millisecond)
				}
				paused.Do(func() {
					mu.Lock()
					pausing = true
					mu.Unlock()
					if interrupt {
						cancelSoft()
					}
				})
			}()
			wantsStop := func() bool { mu.Lock(); defer mu.Unlock(); return pausing }
			done, err, _ := b.run(soft, t.Context(), wantsStop)
			if done || err != nil {
				t.Fatalf("first run: %v %v", done, err)
			}
			if (b.file("workspace/finished") != "") == interrupt || b.file("workspace/second") != "" {
				t.Errorf("after pausing: finished %q, second %q", b.file("workspace/finished"), b.file("workspace/second"))
			}
			if done, err := b.runToEnd(); !done || err != nil {
				t.Fatalf("resumed run: %v %v", done, err)
			}
			if b.file("workspace/second") != "second\n" {
				t.Error("the second call didn't run after resuming")
			}
		})
	}
}

// Pausing during a model call: drain waits for the answer, interrupt
// abandons it, and the next run makes the same call again.
func TestPauseDuringInference(t *testing.T) {
	b := newBox(t, "", turn{block: true}, turn{text: "never"})
	soft, cancel := context.WithCancel(t.Context())
	time.AfterFunc(50*time.Millisecond, cancel)
	done, err, _ := b.run(soft, t.Context(), func() bool { return soft.Err() != nil })
	if done || err != nil {
		t.Fatal(done, err)
	}
	b.model.turns[0] = turn{text: "done"}
	if done, err := b.runToEnd(); !done || err != nil {
		t.Fatal(done, err)
	}
	if m := b.model; len(m.intents) != 2 || m.intents[0] != m.intents[1] {
		t.Errorf("intents %v", m.intents)
	}
}

func TestCheckpointCadence(t *testing.T) {
	var turns []turn
	for range 5 {
		turns = append(turns, turn{calls: [][2]string{{"glob", `{"pattern":"*.none"}`}}})
	}
	turns = append(turns, turn{text: "done"})
	b := newBox(t, "", turns...)
	b.settings.CheckpointEvery = 2
	// Crash in the middle; recovery replays the turns since the checkpoint.
	b.crashBefore = func(kind string, _ proto.Message) bool {
		return kind == KindInferenceIntent && len(b.model.intents) == 3
	}
	if _, _, crashed := b.run(t.Context(), t.Context(), nil); !crashed {
		t.Fatal("didn't crash")
	}
	if done, err := b.runToEnd(); !done || err != nil {
		t.Fatal(done, err)
	}
	// The initial one, then after turns 2 and 4.
	if n := b.count(KindCheckpoint); n != 3 {
		t.Errorf("%d checkpoints", n)
	}
	if n := b.count(KindInferenceOutcome); n != 6 {
		t.Errorf("%d answers", n)
	}
}

func TestAgentEnds(t *testing.T) {
	tests := []struct {
		name  string
		turns []turn
		max   string
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
			b := newBox(t, "", tt.turns...)
			def := tt.max
			_, err := Run(Env{
				Settings: b.settings, Folder: b.folder,
				Task:      &sessionfs.Task{ID: "t", Kind: Kind, Definition: json.RawMessage(def)},
				Record:    func(context.Context, string, proto.Message) error { return nil },
				Emit:      func(*gatewayv0.ActivityEvent) {},
				Infer:     b.model.infer,
				Soft:      t.Context(),
				Hard:      t.Context(),
				WantsStop: func() bool { return false },
			})
			if (err == nil) != (tt.err == "") || (err != nil && !strings.Contains(err.Error(), tt.err)) {
				t.Fatalf("err = %v, want %q", err, tt.err)
			}
		})
	}
}
