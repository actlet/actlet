package tui

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	attachv0 "github.com/actlet/actlet/proto/actlet/attach/v0"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
	"github.com/actlet/actlet/sdk"
)

var update = flag.Bool("update", false, "rewrite the golden files")

var epoch0 = time.Unix(1700000000, 0)

// run is a recorded session: the agent spawns a subagent that writes a
// file, an operator sends a note, and the agent finishes.
func run() []*gatewayv0.ActivityEvent {
	var out []*gatewayv0.ActivityEvent
	add := func(worker string, body func(*gatewayv0.ActivityEvent)) {
		seq := uint64(len(out) + 1)
		ev := &gatewayv0.ActivityEvent{Epoch: 1, Seq: seq, Id: sdk.EventID(1, seq), Worker: worker,
			Time: timestamppb.New(epoch0.Add(time.Duration(seq) * time.Second))}
		body(ev)
		out = append(out, ev)
	}
	add("", func(ev *gatewayv0.ActivityEvent) {
		ev.Body = &gatewayv0.ActivityEvent_Started{Started: &gatewayv0.SessionStarted{Epoch: 1, ProfileId: "coding"}}
	})
	add("", func(ev *gatewayv0.ActivityEvent) {
		ev.Body = &gatewayv0.ActivityEvent_SessionInfo{SessionInfo: &gatewayv0.SessionInfo{
			TaskId: "sum", TaskKind: "agent", Task: "{\n  \"id\": \"sum\",\n  \"input\": \"Add up the numbers.\"\n}",
			Config: "{\n  \"model\": \"large\"\n}", LocalCommands: []string{"steer"},
			Budgets: []*gatewayv0.Budget{{Kind: gatewayv0.BudgetKind_BUDGET_KIND_MONEY, Limit: 5, Currency: "EUR"}},
			Models: &gatewayv0.ModelCatalog{Aliases: []*gatewayv0.ModelAlias{{Alias: "large", ContextWindow: 200000}, {Alias: "fast", ContextWindow: 128000}},
				Roles: map[string]string{"main": "large", "subagent": "fast"}, Selectable: []string{"fast"}},
			Tools: []*gatewayv0.ToolInfo{{Name: "shell", Policy: "never", Description: "Run a command. More text."},
				{Name: "write_file", Policy: "reconcile", Description: "Write a file."}},
			Grants:        []*gatewayv0.ToolGrant{{Name: "shell", Settings: map[string]string{"timeout": "5m"}}},
			ToolsManifest: "jq and ripgrep are installed.",
		}}
	})
	add("", func(ev *gatewayv0.ActivityEvent) {
		ev.Body = &gatewayv0.ActivityEvent_State{State: &gatewayv0.StateChange{To: gatewayv0.SessionState_SESSION_STATE_RUNNING, Reason: "started"}}
	})
	add("root", func(ev *gatewayv0.ActivityEvent) {
		ev.Body = &gatewayv0.ActivityEvent_ModelTurn{ModelTurn: &gatewayv0.ModelTurn{Turn: 1, Model: "large", Text: "I'll hand the file to a subagent.",
			ToolCalls: []string{"spawn_subagent"}, Usage: &gatewayv0.Usage{InputTokens: 12000, OutputTokens: 400, Cost: 0.042, Currency: "EUR"}}}
	})
	add("root.1", func(ev *gatewayv0.ActivityEvent) {
		ev.Body = &gatewayv0.ActivityEvent_SubagentSpawned{SubagentSpawned: &gatewayv0.SubagentSpawned{Parent: "root", Task: "Write the report.", Model: "fast", CallId: "s1"}}
	})
	add("root.1", func(ev *gatewayv0.ActivityEvent) {
		ev.Body = &gatewayv0.ActivityEvent_ToolStarted{ToolStarted: &gatewayv0.ToolStarted{CallId: "c1", Tool: "write_file", Input: `{"path":"report.md"}`}}
	})
	add("root.1", func(ev *gatewayv0.ActivityEvent) {
		ev.Body = &gatewayv0.ActivityEvent_FileChange{FileChange: &gatewayv0.FileChange{CallId: "c1", Path: "report.md", Created: true,
			Diff: "--- /dev/null\n+++ b/report.md\n@@ -0,0 +1 @@\n+Total: 12\n"}}
	})
	add("root.1", func(ev *gatewayv0.ActivityEvent) {
		ev.Body = &gatewayv0.ActivityEvent_ToolFinished{ToolFinished: &gatewayv0.ToolFinished{CallId: "c1", Tool: "write_file",
			Output: "created report.md (10 bytes)", Took: durationpb.New(12 * time.Millisecond)}}
	})
	add("", func(ev *gatewayv0.ActivityEvent) {
		ev.Body = &gatewayv0.ActivityEvent_Steer{Steer: &gatewayv0.SteerNote{Note: "use metric units", Origin: gatewayv0.Origin_ORIGIN_LOCAL, Actor: "alice"}}
	})
	add("root.1", func(ev *gatewayv0.ActivityEvent) {
		ev.Body = &gatewayv0.ActivityEvent_SubagentFinished{SubagentFinished: &gatewayv0.SubagentFinished{Result: "Wrote report.md.", Turns: 2}}
	})
	add("root", func(ev *gatewayv0.ActivityEvent) {
		ev.Body = &gatewayv0.ActivityEvent_ToolFinished{ToolFinished: &gatewayv0.ToolFinished{CallId: "s1", Tool: "spawn_subagent",
			Output: "Wrote report.md.", Took: durationpb.New(4 * time.Second)}}
	})
	add("root", func(ev *gatewayv0.ActivityEvent) {
		ev.Body = &gatewayv0.ActivityEvent_ModelTurn{ModelTurn: &gatewayv0.ModelTurn{Turn: 2, Model: "large", Text: "Now the totals.",
			ToolCalls: []string{"shell"}, Usage: &gatewayv0.Usage{InputTokens: 13000, OutputTokens: 100, Cost: 0.04, Currency: "EUR"}}}
	})
	add("root", func(ev *gatewayv0.ActivityEvent) {
		ev.Body = &gatewayv0.ActivityEvent_ToolStarted{ToolStarted: &gatewayv0.ToolStarted{CallId: "c2", Tool: "shell", Input: `{"command":"sleep 5"}`}}
	})
	return out
}

func watch(evs []*gatewayv0.ActivityEvent) EventsMsg {
	var msgs EventsMsg
	for _, ev := range evs {
		msgs = append(msgs, &attachv0.WatchResponse{Msg: &attachv0.WatchResponse_Event{Event: ev}})
	}
	return msgs
}

// recording writes events to a recording file.
func recording(t *testing.T, evs []*gatewayv0.ActivityEvent) string {
	t.Helper()
	store, err := sdk.NewRecordingStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(context.Background(), "sess-1234abcd", evs); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return store.Path("sess-1234abcd")
}

func liveModel() *Model {
	m := NewLive(Connection{Kind: "local box", Target: "/work/session"}, nil, "test")
	m.SetClock(func() time.Time { return epoch0.Add(30 * time.Second) })
	m.SetSize(100, 32)
	m.Update(InfoMsg{Info: &attachv0.InfoResponse{SessionId: "sess-1234abcd", Epoch: 1, State: gatewayv0.SessionState_SESSION_STATE_RUNNING,
		Stats: &attachv0.Stats{Started: timestamppb.New(epoch0), FolderBytes: 52 << 10, OutputBytes: 10, MemoryBytes: 24 << 20,
			Cpu: durationpb.New(1500 * time.Millisecond), Gateway: "gw:7400", GatewayConnected: true}}})
	m.Update(watch(run()))
	return m
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".txt")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("%s differs from the golden file (go test -update to rewrite):\n%s", name, got)
	}
}

// Golden frames of every view, live and replayed.
func TestGoldenFrames(t *testing.T) {
	live := liveModel()
	p, err := OpenPlayer(recording(t, run()))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	p.Seek(p.Len())
	replay := NewReplay(p)
	replay.SetConnection(Connection{Kind: "recording", Target: "run.actrec"})
	replay.SetSize(100, 32)
	for tab, name := range tabNames {
		live.SetTab(tab)
		replay.SetTab(tab)
		golden(t, "live-"+strings.ToLower(name), live.Dump())
		golden(t, "replay-"+strings.ToLower(name), replay.Dump())
	}
	live.SetTab(TabChat)
	live.SetFilter("root.1", "")
	golden(t, "live-worker", live.Dump())
}

// The live view and the replay of the same events hold the same state.
func TestLiveAndReplayAgree(t *testing.T) {
	live := liveModel()
	p, err := OpenPlayer(recording(t, run()))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	p.Seek(p.Len())
	a, b := live.Session(), p.View()
	if a.ID != b.ID || a.Events != b.Events || len(a.Workers()) != len(b.Workers()) {
		t.Fatalf("live %+v, replay %+v", a, b)
	}
	if !reflect.DeepEqual(a, b) {
		t.Error("the live and the replayed views differ")
	}
}

// The real program runs headless, takes keys and quits.
func TestProgramSmoke(t *testing.T) {
	var sent []*attachv0.CommandRequest
	var mu sync.Mutex
	m := NewLive(Connection{Kind: "local box"}, func(req *attachv0.CommandRequest) (*gatewayv0.CommandAck, error) {
		mu.Lock()
		defer mu.Unlock()
		sent = append(sent, req)
		if req.GetPause() != nil {
			return nil, status.Error(codes.PermissionDenied, "the profile doesn't let a local viewer pause the session")
		}
		return &gatewayv0.CommandAck{Result: gatewayv0.CommandResult_COMMAND_RESULT_ACCEPTED}, nil
	}, "tester")
	var out bytes.Buffer
	p := tea.NewProgram(m, tea.WithInput(&bytes.Buffer{}), tea.WithOutput(&out), tea.WithWindowSize(100, 30),
		tea.WithoutSignals(), tea.WithContext(t.Context()))
	done := make(chan error, 1)
	go func() {
		_, err := p.Run()
		done <- err
	}()
	p.Send(watch(run()))
	keys := func(s ...string) {
		for _, k := range s {
			switch k {
			case "enter":
				p.Send(tea.KeyPressMsg{Code: tea.KeyEnter})
			case "tab":
				p.Send(tea.KeyPressMsg{Code: tea.KeyTab})
			default:
				for _, r := range k {
					p.Send(tea.KeyPressMsg{Code: r, Text: string(r)})
				}
			}
		}
	}
	keys("tab", "tab", "tab", "tab", "]", "i", "hello", "enter", "P")
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(sent)
		mu.Unlock()
		if n == 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	keys("q")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the program didn't quit")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 2 || sent[0].GetSteer().GetNote() != "hello" || sent[0].GetActor() != "tester" || sent[1].GetPause() == nil {
		t.Errorf("commands sent: %v", sent)
	}
	if !strings.Contains(m.notice, "pause refused") || !strings.Contains(m.notice, "doesn't let a local viewer") {
		t.Errorf("notice %q", m.notice)
	}
	if m.worker != "root" || !strings.Contains(out.String(), "WORKERS") {
		t.Errorf("worker %q; output %d bytes", m.worker, out.Len())
	}
}

// Untrusted text can't drive the terminal from either view.
func TestRenderSanitized(t *testing.T) {
	evil := "\x1b]0;pwned\x07\x1b[2J\x1b]52;c;ZXZpbA==\x07red\x1b[31m\rover"
	evs := []*gatewayv0.ActivityEvent{
		{Epoch: 1, Seq: 1, Worker: "root", Body: &gatewayv0.ActivityEvent_ModelTurn{ModelTurn: &gatewayv0.ModelTurn{Turn: 1, Text: evil}}},
		{Epoch: 1, Seq: 2, Worker: "root", Body: &gatewayv0.ActivityEvent_ToolFinished{ToolFinished: &gatewayv0.ToolFinished{CallId: "x", Tool: evil, Output: evil}}},
		{Epoch: 1, Seq: 3, Worker: "root", Body: &gatewayv0.ActivityEvent_FileChange{FileChange: &gatewayv0.FileChange{CallId: "x", Path: evil, Diff: "+" + evil}}},
	}
	for _, m := range []*Model{NewLive(Connection{}, nil, ""), mustReplay(t, evs)} {
		m.Update(watch(evs))
		for tab := range tabCount {
			m.SetTab(tab)
			screen := m.Render()
			for _, bad := range []string{"\x07", "]0;", "]52;", "\x1b[2J", "\r"} {
				if strings.Contains(screen, bad) {
					t.Errorf("tab %d shows %q", tab, bad)
				}
			}
		}
		m.SetTab(TabChat)
		if !strings.Contains(m.Dump(), "redover") {
			t.Error("the text around the escapes is gone")
		}
	}
}

func mustReplay(t *testing.T, evs []*gatewayv0.ActivityEvent) *Model {
	p, err := OpenPlayer(recording(t, run()[:1]))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return NewReplay(p)
}

func TestPlayer(t *testing.T) {
	evs := run()
	path := recording(t, evs[:5])
	p, err := OpenPlayer(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if p.Len() != 5 || p.Pos() != 0 || p.View().ID != "sess-1234abcd" {
		t.Fatalf("opened: %d events at %d, session %q", p.Len(), p.Pos(), p.View().ID)
	}
	// Playing at 2x shows the events as their time comes, skipping long
	// pauses.
	start := time.Now()
	p.Play(start, 2)
	p.Advance(start.Add(1500 * time.Millisecond))
	if p.Pos() != 4 {
		t.Errorf("after 3s of recording: %d events", p.Pos())
	}
	p.Seek(1)
	if p.Pos() != 1 || p.View().Events != 1 {
		t.Errorf("seek back: %d", p.Pos())
	}
	// A growing recording: following shows new events.
	store, err := sdk.NewRecordingStore(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(context.Background(), "sess-1234abcd", evs[5:]); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	p.SetFollow(true)
	if !p.Advance(time.Now()) || p.Len() != len(evs) || p.Pos() != len(evs) || p.State() != "following" {
		t.Errorf("following: %d of %d", p.Pos(), p.Len())
	}

	// An unfinished recording, cut in the middle of a batch, plays up to
	// the cut.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cut := filepath.Join(t.TempDir(), "cut.actrec")
	if err := os.WriteFile(cut, data[:len(data)-20], 0o600); err != nil {
		t.Fatal(err)
	}
	q, err := OpenPlayer(cut)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if q.Len() != 5 || q.Err() != nil {
		t.Errorf("unfinished: %d events, %v", q.Len(), q.Err())
	}
	if _, err := OpenPlayer(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing file: %v", err)
	}
}

// Clicks pick tabs and tool calls; a second click on a call expands it.
func TestMouse(t *testing.T) {
	m := liveModel()
	m.SetSize(120, 40)
	m.Render()
	tools := m.tabSpans[TabTools]
	m.Update(tea.MouseClickMsg{X: tools[0] + 1, Y: 0, Button: tea.MouseLeft})
	if m.tab != TabTools {
		t.Fatalf("tab %d after clicking Tools", m.tab)
	}
	m.Update(tea.MouseClickMsg{X: m.tabSpans[TabChat][0], Y: 0, Button: tea.MouseLeft})
	m.Render()
	row := -1
	for i, b := range m.rows {
		if strings.HasSuffix(b, "/c1") {
			row = i
			break
		}
	}
	if m.tab != TabChat || row < 0 {
		t.Fatalf("tab %d, no row for the subagent's call in %v", m.tab, m.rows)
	}
	m.Update(tea.MouseClickMsg{X: 5, Y: row + 1, Button: tea.MouseLeft})
	if !strings.HasSuffix(m.selected, "/c1") {
		t.Fatalf("selected %q", m.selected)
	}
	m.Update(tea.MouseClickMsg{X: 5, Y: row + 1, Button: tea.MouseLeft})
	if !m.expanded[m.selected] {
		t.Error("a second click didn't expand the call")
	}
	m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	if m.scroll != 3 || m.selected != "" {
		t.Errorf("wheel: scroll %d, selected %q", m.scroll, m.selected)
	}
}

// The file viewer: a recording shows what it holds, a live box serves the
// file; a click on the sidebar, or o and enter, opens it.
func TestFileViewer(t *testing.T) {
	p, err := OpenPlayer(recording(t, run()))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	p.Seek(p.Len())
	replay := NewReplay(p)
	replay.SetSize(120, 30)
	replay.Open("workspace/report.md")
	if d := replay.Dump(); !strings.Contains(d, "1 Total: 12") || !strings.Contains(d, "from the recording") {
		t.Errorf("replayed file:\n%s", d)
	}
	replay.Open("workspace/never-changed.txt")
	if !strings.Contains(replay.Dump(), "not in the recording") {
		t.Error("a file the recording lacks")
	}

	live := liveModel()
	live.SetSize(120, 40)
	var asked []string
	live.SetFileReader(func(path string) (*attachv0.ReadFileResponse, error) {
		asked = append(asked, path)
		return &attachv0.ReadFileResponse{Path: path, Content: []byte("\x1b]0;evil\x07Total: 12\n"), Size: 10}, nil
	})
	live.Render()
	row := -1
	for y, path := range live.fileRows {
		if path == "workspace/report.md" {
			row = y
		}
	}
	if row < 0 {
		t.Fatalf("no sidebar row for workspace/report.md: %v", live.fileRows)
	}
	_, cmd := live.Update(tea.MouseClickMsg{X: live.sideX + 2, Y: row, Button: tea.MouseLeft})
	if cmd == nil || live.popup == nil {
		t.Fatal("a click on the file didn't open it")
	}
	live.Update(cmd())
	screen := live.Render()
	if len(asked) != 1 || !strings.Contains(ansi.Strip(screen), "1 Total: 12") || strings.Contains(screen, "]0;evil") {
		t.Errorf("asked %v; popup:\n%s", asked, ansi.Strip(screen))
	}
	live.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if live.popup != nil {
		t.Error("esc didn't close the file")
	}
	// The keyboard way.
	live.Update(tea.KeyPressMsg{Code: 'o', Text: "o"})
	_, cmd = live.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil || live.popup == nil || live.popup.path != "workspace/report.md" {
		t.Fatalf("o and enter: %+v", live.popup)
	}
}

func TestSeparators(t *testing.T) {
	m := liveModel()
	m.SetSeparators(true)
	golden(t, "live-chat-separators", m.Dump())
}

func TestTruncateLeft(t *testing.T) {
	for _, tt := range []struct {
		in    string
		width int
		want  string
	}{
		{"output/report.md", 20, "output/report.md"},
		{"workspace/analysis/north.md", 20, "…e/analysis/north.md"},
		{"workspace/analysis/north.md", 1, "w"},
	} {
		if got := truncateLeft(tt.in, tt.width); got != tt.want {
			t.Errorf("truncateLeft(%q, %d) = %q, want %q", tt.in, tt.width, got, tt.want)
		}
	}
}

// goneBox is a box that has stopped serving.
type goneBox struct{ attachv0.AttachClient }

func (goneBox) ReadFile(context.Context, *attachv0.ReadFileRequest, ...grpc.CallOption) (*attachv0.ReadFileResponse, error) {
	return nil, status.Error(codes.Unavailable, "connection refused")
}

// Once the session ends the box stops serving, but a viewer on the same
// machine can still open the files, by the same rules.
func TestFilesAfterTheBoxEnds(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"workspace", "output", ".actlet"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, data := range map[string]string{"output/report.md": "Total: 12\n", ".actlet/run.log": "private\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	read := Files(t.Context(), goneBox{}, dir)
	resp, err := read("output/report.md")
	if err != nil || string(resp.GetContent()) != "Total: 12\n" || resp.GetPath() != "output/report.md" {
		t.Errorf("report: %v, %v", resp, err)
	}
	if _, err := read(".actlet/run.log"); status.Code(err) != codes.PermissionDenied {
		t.Errorf("state file: %v", err)
	}
	if _, err := Files(t.Context(), goneBox{}, "")("output/report.md"); status.Code(err) != codes.Unavailable {
		t.Errorf("without a folder: %v", err)
	}
}
