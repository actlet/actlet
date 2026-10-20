package attach

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	attachv0 "github.com/actlet/actlet/proto/actlet/attach/v0"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

func textEvent(seq uint64) *gatewayv0.ActivityEvent {
	return &gatewayv0.ActivityEvent{Epoch: 1, Seq: seq, Id: fmt.Sprintf("1.%d", seq),
		Body: &gatewayv0.ActivityEvent_Text{Text: &gatewayv0.Text{Text: strings.Repeat("x", 100)}}}
}

func stateEvent(seq uint64) *gatewayv0.ActivityEvent {
	return &gatewayv0.ActivityEvent{Epoch: 1, Seq: seq, Body: &gatewayv0.ActivityEvent_State{State: &gatewayv0.StateChange{
		To: gatewayv0.SessionState_SESSION_STATE_RUNNING,
	}}}
}

// drain reads what is queued without waiting.
func drain(s *Subscription) (seqs []uint64, skipped uint64) {
	for {
		select {
		case m, ok := <-s.C():
			if !ok {
				return
			}
			if ev := m.GetEvent(); ev != nil {
				seqs = append(seqs, ev.GetSeq())
			}
			skipped += m.GetSkipped().GetCount()
		default:
			return
		}
	}
}

func TestHubBacklogThenLive(t *testing.T) {
	h := NewHub(HubOptions{Backlog: 5, Pinned: 2, ClientBuf: 100})
	h.Publish(stateEvent(1))
	for seq := uint64(2); seq <= 10; seq++ {
		h.Publish(textEvent(seq))
	}
	// The backlog holds 6-10; the state event 1 is pinned.
	s, err := h.Subscribe(0)
	if err != nil {
		t.Fatal(err)
	}
	h.Publish(textEvent(11))
	seqs, skipped := drain(s)
	if got := fmt.Sprint(seqs); got != "[1 6 7 8 9 10 11]" || skipped != 4 {
		t.Errorf("events %s, skipped %d", got, skipped)
	}
	// From a position: only what follows it.
	s2, _ := h.Subscribe(9)
	if seqs, skipped := drain(s2); fmt.Sprint(seqs) != "[10 11]" || skipped != 0 {
		t.Errorf("after 9: %v, skipped %d", seqs, skipped)
	}
	s.Close()
	s2.Close()
	if _, queued := h.Held(); queued != 0 {
		t.Errorf("%d messages queued after detaching", queued)
	}
}

// A viewer that doesn't read skips events, is told how many, and never
// holds up publishing.
func TestHubSlowViewer(t *testing.T) {
	h := NewHub(HubOptions{Backlog: 10, ClientBuf: 4})
	s, _ := h.Subscribe(0)
	for seq := uint64(1); seq <= 20; seq++ {
		h.Publish(textEvent(seq)) // never blocks
	}
	seqs, _ := drain(s)
	if fmt.Sprint(seqs) != "[1 2 3 4]" {
		t.Fatalf("first events %v", seqs)
	}
	h.Publish(textEvent(21))
	h.Publish(textEvent(22))
	seqs, skipped := drain(s)
	if skipped != 16 || fmt.Sprint(seqs) != "[21 22]" {
		t.Errorf("then %v, skipped %d", seqs, skipped)
	}
}

func TestHubLimits(t *testing.T) {
	h := NewHub(HubOptions{MaxClients: 1})
	s, err := h.Subscribe(0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Subscribe(0); err == nil {
		t.Error("a second viewer was let in")
	}
	h.Close()
	if _, ok := <-s.C(); ok {
		t.Error("the feed stayed open after the hub closed")
	}
	h.Publish(textEvent(1)) // after closing: ignored
}

// fakeBox is a session that records the commands it gets.
type fakeBox struct {
	mu       sync.Mutex
	commands []*gatewayv0.Command
}

func (b *fakeBox) Info() *attachv0.InfoResponse {
	return &attachv0.InfoResponse{SessionId: "s1", Epoch: 2, State: gatewayv0.SessionState_SESSION_STATE_RUNNING}
}

func (b *fakeBox) Apply(_ context.Context, cmd *gatewayv0.Command) (*gatewayv0.CommandAck, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.commands = append(b.commands, cmd)
	return &gatewayv0.CommandAck{CommandId: cmd.GetId(), Result: gatewayv0.CommandResult_COMMAND_RESULT_ACCEPTED}, nil
}

// serve runs a server on a socket in a fresh state directory.
func serve(t *testing.T, hub *Hub, box Box, commands []string) (attachv0.AttachClient, string) {
	t.Helper()
	dir := t.TempDir()
	lis, err := Listen(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv := Start(lis, NewServer("s1", hub, box, commands, nil))
	t.Cleanup(srv.Stop)
	client, closeClient, err := Dial(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeClient() })
	return client, dir
}

func TestServer(t *testing.T) {
	hub := NewHub(HubOptions{})
	box := &fakeBox{}
	client, dir := serve(t, hub, box, AllowedCommands(nil))
	ctx := t.Context()

	info, err := client.Info(ctx, &attachv0.InfoRequest{})
	if err != nil || info.GetSessionId() != "s1" || fmt.Sprint(info.GetCommands()) != "[steer]" {
		t.Fatalf("info %v, %v", info, err)
	}

	hub.Publish(textEvent(1))
	stream, err := client.Watch(ctx, &attachv0.WatchRequest{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := stream.Recv()
	if err != nil || first.GetEvent().GetSeq() != 1 {
		t.Fatalf("backlog %v, %v", first, err)
	}
	hub.Publish(textEvent(2))
	if live, err := stream.Recv(); err != nil || live.GetEvent().GetSeq() != 2 {
		t.Fatalf("live %v, %v", live, err)
	}

	// A steer note is passed on as a local command; lifecycle commands
	// are refused without a grant.
	ack, err := client.Command(ctx, &attachv0.CommandRequest{Actor: "alice", Action: &attachv0.CommandRequest_Steer{Steer: &gatewayv0.Steer{Note: "hi"}}})
	if err != nil || ack.GetResult() != gatewayv0.CommandResult_COMMAND_RESULT_ACCEPTED {
		t.Fatalf("steer: %v, %v", ack, err)
	}
	for _, req := range []*attachv0.CommandRequest{
		{Action: &attachv0.CommandRequest_Pause{Pause: &gatewayv0.Pause{Mode: gatewayv0.PauseMode_PAUSE_MODE_DRAIN}}},
		{Action: &attachv0.CommandRequest_Stop{Stop: &gatewayv0.Stop{}}},
	} {
		if _, err := client.Command(ctx, req); status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "doesn't let a local viewer") {
			t.Errorf("%v: %v", req, err)
		}
	}
	if _, err := client.Command(ctx, &attachv0.CommandRequest{Action: &attachv0.CommandRequest_Steer{Steer: &gatewayv0.Steer{}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("empty note: %v", err)
	}
	box.mu.Lock()
	cmds := box.commands
	box.mu.Unlock()
	if len(cmds) != 1 || cmds[0].GetOrigin() != gatewayv0.Origin_ORIGIN_LOCAL || cmds[0].GetActor() != "alice" || cmds[0].GetSteer().GetNote() != "hi" {
		t.Errorf("commands %v", cmds)
	}

	// A second box can't listen on the same folder.
	if _, err := Listen(dir); err == nil || !strings.Contains(err.Error(), "already listening") {
		t.Errorf("second listener: %v", err)
	}
}

func TestAllowedCommands(t *testing.T) {
	for _, tt := range []struct {
		la   *gatewayv0.LocalAttach
		want string
	}{
		{nil, "[steer]"},
		{&gatewayv0.LocalAttach{Commands: []string{"none"}}, "[]"},
		{&gatewayv0.LocalAttach{Commands: []string{"steer", "pause", "bogus", "pause"}}, "[steer pause]"},
		{&gatewayv0.LocalAttach{Disabled: true, Commands: []string{"stop"}}, "[]"},
	} {
		if got := fmt.Sprint(AllowedCommands(tt.la)); got != tt.want {
			t.Errorf("%v: %s, want %s", tt.la, got, tt.want)
		}
	}
}

// The socket goes elsewhere when the state directory's path is too long,
// and a stale socket from a crashed run is replaced.
func TestListenPaths(t *testing.T) {
	long := filepath.Join(t.TempDir(), strings.Repeat("d", 120))
	if err := os.MkdirAll(long, 0o755); err != nil {
		t.Fatal(err)
	}
	lis, err := Listen(long)
	if err != nil {
		t.Fatal(err)
	}
	if ep, err := Endpoint(long); err != nil || ep != lis.Path || len(ep) > maxSocketPath {
		t.Errorf("endpoint %q, %v", ep, err)
	}
	_ = lis.Close()
	if _, err := Endpoint(long); err == nil {
		t.Error("an endpoint after closing")
	}

	if runtime.GOOS == "windows" {
		return
	}
	dir := t.TempDir()
	stale, err := Listen(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = stale.Listener.Close() // crashed: the files stay
	again, err := Listen(dir)
	if err != nil {
		t.Fatalf("over a stale socket: %v", err)
	}
	_ = again.Close()
}

// A viewer that stops reading doesn't grow the box's memory: the hub's
// buffers are bounded and gRPC's flow control stops the rest.
func TestStalledViewerIsBounded(t *testing.T) {
	hub := NewHub(HubOptions{Backlog: 100, ClientBuf: 16})
	client, _ := serve(t, hub, &fakeBox{}, nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if _, err := client.Watch(ctx, &attachv0.WatchRequest{}); err != nil { // never read
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)

	measure := func() uint64 {
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.HeapAlloc
	}
	for seq := uint64(1); seq <= 20000; seq++ {
		hub.Publish(textEvent(seq))
	}
	before := measure()
	start := time.Now()
	for seq := uint64(20001); seq <= 220000; seq++ {
		hub.Publish(textEvent(seq))
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("publishing 200000 events took %v: a stalled viewer held it up", took)
	}
	after := measure()
	events, queued := hub.Held()
	if events > 100+DefaultPinned || queued > 16+100+1 {
		t.Errorf("the hub holds %d events and %d queued messages", events, queued)
	}
	if after > before+8<<20 {
		t.Errorf("heap grew from %d to %d bytes while a viewer was stalled", before, after)
	}
}

// Viewers read files of inputs/, workspace/ and output/, and nothing
// else, also not through symbolic links.
func TestReadFile(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"inputs", "workspace", "output", ".actlet"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(rel, content string) {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("workspace/notes.md", "# Notes\n")
	write("output/report.md", "Total: 12\n")
	write("inputs/data.csv", "a,b\n")
	write("task.json", "{}")
	write(".actlet/secret", "token")
	write("workspace/big.txt", strings.Repeat("x", MaxReadFile+10))
	write("workspace/blob.bin", "a\x00b")
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("private"), 0o644); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		_ = os.Symlink(outside, filepath.Join(dir, "workspace", "escape"))
		_ = os.Symlink("../.actlet/secret", filepath.Join(dir, "workspace", "secret"))
	}
	srv := NewServer("s1", NewHub(HubOptions{}), &fakeBox{}, nil, nil)
	srv.ServeFiles(dir)
	tests := []struct {
		path    string
		code    codes.Code
		content string
		rel     string
	}{
		{"notes.md", codes.OK, "# Notes\n", "workspace/notes.md"},
		{"../output/report.md", codes.OK, "Total: 12\n", "output/report.md"},
		{"../inputs/data.csv", codes.OK, "a,b\n", "inputs/data.csv"},
		{"../task.json", codes.PermissionDenied, "", ""},
		{"../.actlet/secret", codes.PermissionDenied, "", ""},
		{outside, codes.PermissionDenied, "", ""},
		{"missing.txt", codes.NotFound, "", ""},
		{".", codes.InvalidArgument, "", ""},
	}
	if runtime.GOOS != "windows" {
		tests = append(tests, struct {
			path    string
			code    codes.Code
			content string
			rel     string
		}{"escape", codes.PermissionDenied, "", ""}, struct {
			path    string
			code    codes.Code
			content string
			rel     string
		}{"secret", codes.PermissionDenied, "", ""})
	}
	for _, tt := range tests {
		resp, err := srv.ReadFile(t.Context(), &attachv0.ReadFileRequest{Path: tt.path})
		if status.Code(err) != tt.code {
			t.Errorf("%s: %v, want %v", tt.path, err, tt.code)
			continue
		}
		if err == nil && (string(resp.GetContent()) != tt.content || resp.GetPath() != tt.rel) {
			t.Errorf("%s: %q at %q", tt.path, resp.GetContent(), resp.GetPath())
		}
	}
	big, err := srv.ReadFile(t.Context(), &attachv0.ReadFileRequest{Path: "big.txt"})
	if err != nil || !big.GetTruncated() || len(big.GetContent()) != MaxReadFile || big.GetSize() != MaxReadFile+10 {
		t.Errorf("big file: %v", err)
	}
	if bin, err := srv.ReadFile(t.Context(), &attachv0.ReadFileRequest{Path: "blob.bin"}); err != nil || !bin.GetBinary() || len(bin.GetContent()) != 0 {
		t.Errorf("binary file: %v %v", bin, err)
	}
}

// An orchestrator's hub holds several runs, each numbered from 1.
func TestHubRuns(t *testing.T) {
	h := NewHub(HubOptions{Backlog: 4})
	ev := func(epoch, seq uint64) *gatewayv0.ActivityEvent {
		e := textEvent(seq)
		e.Epoch = epoch
		return e
	}
	for _, e := range []*gatewayv0.ActivityEvent{ev(1, 1), ev(1, 2), ev(2, 1), ev(2, 2)} {
		h.Publish(e)
	}
	drain := func(s *Subscription) (got []string) {
		for {
			select {
			case m := <-s.C():
				if sk := m.GetSkipped(); sk != nil {
					got = append(got, fmt.Sprintf("skipped %d", sk.GetCount()))
				} else {
					got = append(got, fmt.Sprintf("%d.%d", m.GetEvent().GetEpoch(), m.GetEvent().GetSeq()))
				}
			default:
				s.Close()
				return got
			}
		}
	}
	all, _ := h.Subscribe(0)
	if got := fmt.Sprint(drain(all)); got != "[1.1 1.2 2.1 2.2]" {
		t.Errorf("everything: %s", got)
	}
	after, _ := h.Subscribe(1)
	if got := fmt.Sprint(drain(after)); got != "[2.2]" {
		t.Errorf("after 1 of the latest run: %s", got)
	}
	// The ring keeps 4: the first run's start is gone, and counted.
	h.Publish(ev(2, 3))
	late, _ := h.Subscribe(0)
	if got := fmt.Sprint(drain(late)); got != "[skipped 1 1.2 2.1 2.2 2.3]" {
		t.Errorf("late: %s", got)
	}
}

// A box's attach port: only viewers with the token get in, only for the
// box's session, and their commands are remote ones.
func TestPort(t *testing.T) {
	if _, err := ListenPort("0.0.0.0:0", false); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Errorf("a non-loopback address: %v", err)
	}
	lis, err := ListenPort("127.0.0.1:0", false)
	if err != nil {
		t.Fatal(err)
	}
	hub := NewHub(HubOptions{})
	box := &fakeBox{}
	srv := StartPort(lis, NewServer("s1", hub, box, []string{CommandSteer}, nil).Remote(), "sesame")
	t.Cleanup(srv.Stop)
	addr := lis.Addr().String()
	dial := func(o RemoteOptions) attachv0.AttachClient {
		c, closeClient, err := DialRemote(addr, o)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = closeClient() })
		return c
	}
	ctx := t.Context()
	for _, token := range []string{"", "wrong"} {
		c := dial(RemoteOptions{Token: token})
		if _, err := c.Info(ctx, &attachv0.InfoRequest{}); status.Code(err) != codes.Unauthenticated {
			t.Errorf("token %q: info %v", token, err)
		}
		stream, err := c.Watch(ctx, &attachv0.WatchRequest{})
		if err == nil {
			_, err = stream.Recv()
		}
		if status.Code(err) != codes.Unauthenticated {
			t.Errorf("token %q: watch %v", token, err)
		}
	}
	if _, err := dial(RemoteOptions{Token: "sesame", Session: "other"}).Info(ctx, &attachv0.InfoRequest{}); status.Code(err) != codes.NotFound {
		t.Errorf("another session: %v", err)
	}
	c := dial(RemoteOptions{Token: "sesame", Session: "s1"})
	if list, err := c.Sessions(ctx, &attachv0.SessionsRequest{}); err != nil || len(list.GetSessions()) != 1 || list.GetSessions()[0].GetSessionId() != "s1" {
		t.Errorf("sessions %v, %v", list, err)
	}
	if _, err := c.Command(ctx, &attachv0.CommandRequest{Action: &attachv0.CommandRequest_Steer{Steer: &gatewayv0.Steer{Note: "hi"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Command(ctx, &attachv0.CommandRequest{Action: &attachv0.CommandRequest_Stop{Stop: &gatewayv0.Stop{}}}); status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "remote viewer") {
		t.Errorf("stop: %v", err)
	}
	box.mu.Lock()
	defer box.mu.Unlock()
	if len(box.commands) != 1 || box.commands[0].GetOrigin() != gatewayv0.Origin_ORIGIN_REMOTE || box.commands[0].GetActor() != "remote viewer" {
		t.Errorf("commands %v", box.commands)
	}
}
