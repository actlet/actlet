//go:build !windows

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/actlet/actlet/internal/attach"
	"github.com/actlet/actlet/internal/journal"
	"github.com/actlet/actlet/internal/sessionfs"
	attachv0 "github.com/actlet/actlet/proto/actlet/attach/v0"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
	"github.com/actlet/actlet/sdk"
)

var (
	buildOnce sync.Once
	binDir    string
	buildErr  error
)

// binaries builds actlet and actlet-devgw once for the package's tests.
func binaries(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		binDir, buildErr = os.MkdirTemp("", "actlet-cli-test-")
		if buildErr != nil {
			return
		}
		goTool, err := exec.LookPath("go")
		if err != nil {
			buildErr = err
			return
		}
		cmd := exec.Command(goTool, "build", "-o", binDir+string(filepath.Separator), "../actlet", "../actlet-devgw")
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("go build: %w\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return binDir
}

func TestMain(m *testing.M) {
	code := m.Run()
	if binDir != "" {
		_ = os.RemoveAll(binDir)
	}
	os.Exit(code)
}

// freeAddr returns a loopback address with a port free a moment ago.
func freeAddr(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lis.Close() }()
	return lis.Addr().String()
}

// attachScript writes a note and runs a subagent, then waits in the shell
// until the test creates workspace/go, so the session holds still while
// viewers look at it.
const attachScript = `
turns:
  - text: Writing a note, and asking a subagent to count.
    tool_calls:
      - name: write_file
        input: {path: notes.txt, content: "remote attach works\n"}
      - name: spawn_subagent
        input: {task: "Count to three."}
  - expect:
      - contains: notes.txt
      - contains: one, two, three
    tool_calls:
      - name: shell
        input: {command: "env; until [ -f go ]; do sleep 0.1; done; echo released"}
  - text: done
subagents:
  - match: Count to three
    turns:
      - text: one, two, three
`

const attachProfile = `
schema: v0
id: attach-test
version: "1"
models:
  aliases:
    - alias: scripted
      context_window: 100000
  roles: {main: scripted}
`

const attachTask = `{"schema": "v0", "id": "remote", "kind": "agent", "input": "Write a note."}`

// remoteSession is devgw and a box running a session with an attach port.
type remoteSession struct {
	gateway, port string
	dir, data     string
	box, devgw    *exec.Cmd
	boxLog        *bytes.Buffer
}

const (
	gatewayToken = "gw-secret-6f1c2a"
	attachToken  = "attach-secret-93be4d"
	otherSecret  = "other-secret-0d77e1"
)

func startRemote(t *testing.T, withPort bool) *remoteSession {
	t.Helper()
	bin := binaries(t)
	work := t.TempDir()
	for name, content := range map[string]string{"model.yaml": attachScript, "profile.yaml": attachProfile, "task.json": attachTask} {
		if err := os.WriteFile(filepath.Join(work, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r := &remoteSession{gateway: freeAddr(t), dir: filepath.Join(work, "session"), data: filepath.Join(work, "gateway"), boxLog: &bytes.Buffer{}}
	r.devgw = exec.Command(filepath.Join(bin, "actlet-devgw"), "-listen", r.gateway, "-token", gatewayToken, "-data", r.data,
		"-task", filepath.Join(work, "task.json"), "-profile", filepath.Join(work, "profile.yaml"), "-model", filepath.Join(work, "model.yaml"))
	r.devgw.Stdout, r.devgw.Stderr = io.Discard, io.Discard
	if err := r.devgw.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = r.devgw.Process.Kill()
		_ = r.devgw.Wait()
	})
	waitListening(t, r.gateway)

	args := []string{"run", "-session", r.dir}
	if withPort {
		r.port = freeAddr(t)
		args = append(args, "-attach-listen", r.port)
	}
	r.box = exec.Command(filepath.Join(bin, "actlet"), args...)
	r.box.Env = append(os.Environ(), "ACTLET_GATEWAY="+r.gateway, "ACTLET_TOKEN="+gatewayToken, "ACTLET_ATTACH_TOKEN="+attachToken,
		"ACTLET_OTHER_SECRET="+otherSecret, "ACTLET_TOOLS_MANIFEST=")
	r.box.Stdout, r.box.Stderr = r.boxLog, r.boxLog
	if err := r.box.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if r.box.ProcessState == nil {
			_ = r.box.Process.Kill()
			_ = r.box.Wait()
		}
	})
	return r
}

func waitListening(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = c.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("nothing listens on %s", addr)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitForShell waits, through the local socket, until the box runs the
// shell command that holds the session still.
func (r *remoteSession) waitForShell(t *testing.T) {
	t.Helper()
	state := sessionfs.Folder{Dir: r.dir}.State()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := attach.Endpoint(state); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the box never listened for viewers; log:\n%s", r.boxLog)
		}
		time.Sleep(20 * time.Millisecond)
	}
	client, closeClient, err := attach.Dial(state)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closeClient() }()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	stream, err := client.Watch(ctx, &attachv0.WatchRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for {
		m, err := stream.Recv()
		if err != nil {
			t.Fatalf("waiting for the shell: %v; log:\n%s", err, r.boxLog)
		}
		if ts := m.GetEvent().GetToolStarted(); ts != nil && ts.GetTool() == "shell" {
			// Let the event reach the gateway's store too.
			time.Sleep(300 * time.Millisecond)
			return
		}
	}
}

// release lets the session finish, and waits for the box to exit.
func (r *remoteSession) release(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.dir, "workspace", "go"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- r.box.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("box: %v; log:\n%s", err, r.boxLog)
		}
	case <-time.After(60 * time.Second):
		t.Fatalf("the box didn't finish; log:\n%s", r.boxLog)
	}
}

func cli(t *testing.T, args ...string) (string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return stdout.String() + stderr.String(), code
}

// localDump runs `actlet attach -dump` on the session folder.
func (r *remoteSession) localDump(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command(filepath.Join(binaries(t), "actlet"), append([]string{"attach", "-session", r.dir, "-dump"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("actlet attach: %v\n%s", err, out)
	}
	return string(out)
}

var spinner = regexp.MustCompile(`[⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏]`)

// screen is a dump without its status line, which names the connection
// and the time, and with the spinner stopped.
func screen(dump string) string {
	lines := strings.Split(strings.TrimRight(dump, "\n"), "\n")
	if len(lines) > 0 {
		lines = lines[:len(lines)-1]
	}
	return spinner.ReplaceAllString(strings.Join(lines, "\n"), "*")
}

func statusLine(dump string) string {
	lines := strings.Split(strings.TrimRight(dump, "\n"), "\n")
	return lines[len(lines)-1]
}

// The same session seen locally, through the orchestrator and directly
// shows the same; viewers elsewhere may steer as the profile allows and
// open files; only those with the token get in.
func TestAttachRemote(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs binaries")
	}
	r := startRemote(t, true)
	r.waitForShell(t)

	viaGateway := []string{"attach", "-gateway", r.gateway, "-token", gatewayToken, "-dump"}
	direct := []string{"attach", "-direct", r.port, "-token", attachToken, "-dump"}
	for _, tab := range []string{"chat", "events"} {
		local := r.localDump(t, "-tab", tab)
		gw, code := cli(t, append(viaGateway, "-tab", tab)...)
		if code != 0 {
			t.Fatalf("through the gateway: exit %d\n%s", code, gw)
		}
		box, code := cli(t, append(direct, "-tab", tab)...)
		if code != 0 {
			t.Fatalf("direct: exit %d\n%s", code, box)
		}
		if screen(gw) != screen(local) || screen(box) != screen(local) {
			t.Errorf("%s tab differs:\nlocal:\n%s\norchestrator:\n%s\nbox:\n%s", tab, local, gw, box)
		}
		if tab == "chat" {
			if !strings.Contains(local, "Subagent root.1") || !strings.Contains(local, "Write notes.txt") {
				t.Errorf("the chat lacks the subagent or the write:\n%s", local)
			}
			// What each viewer says it is connected to.
			for dump, want := range map[string]string{
				local: "local box",
				gw:    "orchestrator " + r.gateway + " · session ",
				box:   "box " + r.port,
			} {
				if !strings.Contains(statusLine(dump), want) {
					t.Errorf("status line %q, want %q", statusLine(dump), want)
				}
			}
		}
	}

	// Files, read through the orchestrator and from the box.
	for _, args := range [][]string{viaGateway, direct} {
		out, code := cli(t, append(args, "-file", "workspace/notes.txt")...)
		if code != 0 || !strings.Contains(out, "remote attach works") {
			t.Errorf("%v: file viewer, exit %d:\n%s", args[:2], code, out)
		}
	}

	// Without the token, or with the other one, nobody gets in.
	for _, args := range [][]string{
		{"attach", "-gateway", r.gateway, "-dump"},
		{"attach", "-gateway", r.gateway, "-token", attachToken, "-dump"},
		{"attach", "-direct", r.port, "-dump"},
		{"attach", "-direct", r.port, "-token", gatewayToken, "-dump"},
	} {
		if out, code := cli(t, args...); code != 1 || !strings.Contains(out, "Unauthenticated") {
			t.Errorf("%v: exit %d:\n%s", args, code, out)
		}
	}

	// A note through the orchestrator reaches the agent; the profile lets
	// viewers do nothing more.
	list, err := remoteClient(t, r.gateway, gatewayToken, "").Sessions(t.Context(), &attachv0.SessionsRequest{})
	if err != nil || len(list.GetSessions()) != 1 {
		t.Fatalf("sessions %v, %v", list, err)
	}
	viewer := remoteClient(t, r.gateway, gatewayToken, list.GetSessions()[0].GetSessionId())
	ack, err := viewer.Command(t.Context(), &attachv0.CommandRequest{Actor: "carol", Action: &attachv0.CommandRequest_Steer{Steer: &gatewayv0.Steer{Note: "keep it short"}}})
	if err != nil || ack.GetResult() != gatewayv0.CommandResult_COMMAND_RESULT_ACCEPTED {
		t.Fatalf("steer: %v, %v", ack, err)
	}
	if _, err := viewer.Command(t.Context(), &attachv0.CommandRequest{Action: &attachv0.CommandRequest_Stop{Stop: &gatewayv0.Stop{}}}); err == nil || !strings.Contains(err.Error(), "PermissionDenied") {
		t.Errorf("stop: %v", err)
	}

	// A viewer that crashes doesn't affect the run.
	crashed := exec.Command(os.Args[0], "-test.run=^TestViewerHelper$")
	crashed.Env = append(os.Environ(), viewerHelperEnv+"="+r.gateway)
	if err := crashed.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	_ = crashed.Process.Signal(syscall.SIGKILL)
	_ = crashed.Wait()

	r.release(t)
	events, _ := cli(t, "replay", "export", "--jsonl", recording(t, r.data))
	if !strings.Contains(events, `"note":"keep it short"`) || !strings.Contains(events, `"origin":"ORIGIN_REMOTE"`) || !strings.Contains(events, `"actor":"carol"`) {
		t.Errorf("the note isn't in the recording as a remote one:\n%s", events)
	}
	if !strings.Contains(events, "SESSION_STATE_COMPLETED") {
		t.Errorf("the session didn't complete:\n%s", r.boxLog)
	}
}

const viewerHelperEnv = "ACTLET_CLI_VIEWER_HELPER"

// TestViewerHelper is a viewer in a separate process: it attaches through
// the gateway and reads, until killed.
func TestViewerHelper(t *testing.T) {
	addr := os.Getenv(viewerHelperEnv)
	if addr == "" {
		t.Skip("helper process only")
	}
	_, _ = cli(t, "attach", "-gateway", addr, "-token", gatewayToken, "-dump", "-tab", "events")
	time.Sleep(time.Hour)
}

func remoteClient(t *testing.T, addr, token, session string) attachv0.AttachClient {
	t.Helper()
	c, closeClient, err := attach.DialRemote(addr, attach.RemoteOptions{Token: token, Session: session})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeClient() })
	return c
}

func recording(t *testing.T, dir string) string {
	t.Helper()
	recs, _ := filepath.Glob(filepath.Join(dir, "*.actrec"))
	if len(recs) != 1 {
		t.Fatalf("recordings %v", recs)
	}
	return recs[0]
}

// Without -attach-listen, the box opens no TCP port.
func TestAttachPortOffByDefault(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs binaries")
	}
	lsof, err := exec.LookPath("lsof")
	if err != nil {
		t.Skip("needs lsof")
	}
	r := startRemote(t, false)
	r.waitForShell(t)
	out, err := exec.Command(lsof, "-nP", "-a", "-p", fmt.Sprint(r.box.Process.Pid), "-iTCP", "-sTCP:LISTEN").Output()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) { // lsof exits 1 when it finds nothing
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Errorf("the box listens:\n%s", out)
	}
	r.release(t)
}

// records returns the payloads of every record of a journal file: the
// session journal or an activity recording.
func records(t *testing.T, path string) [][]byte {
	t.Helper()
	r, _, closeJournal, err := openJournal(path, journal.ReaderOptions{})
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	defer closeJournal()
	var out [][]byte
	for {
		b, err := r.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		for _, rec := range b.Records {
			out = append(out, rec.Data)
		}
	}
}

// After a run whose box held a gateway token, an attach token and another
// ACTLET_ setting, and whose agent printed its environment: none of them
// is in the activity, its export, the session journal or the logs. And a
// recording needs nothing beside it to be played: there is no index.
func TestFinishedRun(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs binaries")
	}
	r := startRemote(t, true)
	r.waitForShell(t)
	r.release(t)
	rec := recording(t, r.data)

	t.Run("no credentials", func(t *testing.T) {
		export, code := cli(t, "replay", "export", "--jsonl", rec)
		// The agent's environment is in the activity, without ACTLET_
		// settings.
		if code != 0 || !strings.Contains(export, "released") || !strings.Contains(export, "PATH=") {
			t.Fatalf("export, exit %d, lacks the shell's output:\n%s", code, export)
		}
		places := map[string][]byte{"the export": []byte(export), "the box's log": r.boxLog.Bytes()}
		for i, data := range records(t, rec) {
			places[fmt.Sprintf("recorded event %d", i)] = data
		}
		for i, data := range records(t, sessionfs.Folder{Dir: r.dir}.Journal()) {
			places[fmt.Sprintf("session journal record %d", i)] = data
		}
		for where, data := range places {
			for _, secret := range []string{gatewayToken, attachToken, otherSecret} {
				if bytes.Contains(data, []byte(secret)) {
					t.Errorf("%s holds %s", where, secret)
				}
			}
		}
	})

	t.Run("no index", func(t *testing.T) {
		alone := filepath.Join(t.TempDir(), "copy.actrec")
		data, err := os.ReadFile(rec)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(alone, data, 0o644); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{
			{"replay", "play", "-dump", "-tab", "events"},
			{"replay", "inspect"},
			{"replay", "export", "--jsonl"},
		} {
			want, code1 := cli(t, append(args, rec)...)
			got, code2 := cli(t, append(args, alone)...)
			if code1 != 0 || code2 != 0 || strings.ReplaceAll(got, alone, rec) != want {
				t.Errorf("%v: the recording alone gives exit %d and\n%s\nwant exit %d and\n%s", args, code2, got, code1, want)
			}
		}
	})
}

// Without -session, the only session is chosen, or the user picks one;
// -dump can't ask, and lists them.
func TestPickSession(t *testing.T) {
	srv, err := sdk.NewServer(sdk.Options{
		Authenticate: sdk.StaticToken("tok"),
		Logger:       slog.New(slog.DiscardHandler),
		Assign: func(_ context.Context, req *gatewayv0.RegisterRequest) (*gatewayv0.RegisterResponse, error) {
			return &gatewayv0.RegisterResponse{Profile: &gatewayv0.Profile{Id: "p"}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer()
	srv.RegisterService(gs)
	srv.RegisterAttach(gs)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	addr := lis.Addr().String()
	register := func(id string) {
		cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = cc.Close() }()
		ctx := metadata.AppendToOutgoingContext(t.Context(), "authorization", "Bearer tok")
		if _, err := gatewayv0.NewGatewayClient(cc).Register(ctx, &gatewayv0.RegisterRequest{ProtocolVersion: "v0", SessionId: id, WorkerId: "w-" + id, Epoch: 1}); err != nil {
			t.Fatal(err)
		}
	}
	opts := attach.RemoteOptions{Token: "tok"}
	var stderr bytes.Buffer
	if _, err := pickSession(addr, opts, false, &stderr); err == nil || !strings.Contains(err.Error(), "no sessions") {
		t.Errorf("no sessions: %v", err)
	}
	register("first")
	if id, err := pickSession(addr, opts, true, &stderr); id != "first" || err != nil {
		t.Errorf("the only session: %q, %v", id, err)
	}
	register("second")
	if _, err := pickSession(addr, opts, true, &stderr); err == nil || !strings.Contains(err.Error(), "-session") || !strings.Contains(err.Error(), "worker w-second") {
		t.Errorf("without a prompt: %v", err)
	}
	stdin = strings.NewReader("2\n")
	t.Cleanup(func() { stdin = os.Stdin })
	if id, err := pickSession(addr, opts, false, &stderr); id != "second" || err != nil || !strings.Contains(stderr.String(), "Attach to which?") {
		t.Errorf("picked %q, %v; asked %q", id, err, stderr.String())
	}
}
