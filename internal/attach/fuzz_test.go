//go:build !windows

// Not on Windows: its file trees hold symbolic links, which need
// privileges there.

package attach

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"google.golang.org/protobuf/proto"

	attachv0 "github.com/actlet/actlet/proto/actlet/attach/v0"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

const secret = "SECRET-7c1e"

// sessionFolder is a session folder whose workspace links to what a
// viewer mustn't read: the state directory, a file outside the folder, and
// a directory outside it.
func sessionFolder(t testing.TB) string {
	t.Helper()
	top := t.TempDir()
	dir := filepath.Join(top, "session")
	for name, content := range map[string]string{
		"inputs/a.txt": "a", "workspace/b.txt": "b", "output/c.txt": "c",
		".actlet/token": secret, "task.json": secret, "../outside/secret": secret,
	} {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ws := filepath.Join(dir, "workspace")
	for link, target := range map[string]string{
		"state": "../.actlet", "task": "../task.json", "out": filepath.Join(top, "outside"),
		"abs": filepath.Join(top, "outside", "secret"), "in": "../inputs/a.txt",
	} {
		if err := os.Symlink(target, filepath.Join(ws, link)); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// FuzzReadFile reads any path a viewer may ask for: it never gets a file
// outside inputs/, workspace/ and output/.
func FuzzReadFile(f *testing.F) {
	for _, p := range []string{"b.txt", "../inputs/a.txt", "in", "state/token", "task", "out/secret", "abs",
		"../.actlet/token", "../task.json", "/etc/passwd", "../../outside/secret", "./../workspace/../output/c.txt", ""} {
		f.Add(p)
	}
	dir := sessionFolder(f)
	f.Fuzz(func(t *testing.T, path string) {
		for _, p := range []string{path, filepath.Join(dir, path)} {
			resp, err := ReadFile(dir, &attachv0.ReadFileRequest{Path: p})
			if err != nil {
				continue
			}
			if strings.Contains(string(resp.GetContent()), secret) {
				t.Fatalf("read %q: %s", p, resp.GetContent())
			}
			if !strings.HasPrefix(resp.GetPath(), "inputs/") && !strings.HasPrefix(resp.GetPath(), "workspace/") && !strings.HasPrefix(resp.GetPath(), "output/") {
				t.Fatalf("read %q as %s", p, resp.GetPath())
			}
		}
	})
}

// A directory the agent swaps for a link while a viewer reads a file in
// it never leads the read out of the readable directories.
func TestReadFileSwappedDirectory(t *testing.T) {
	dir := sessionFolder(t)
	ws := filepath.Join(dir, "workspace")
	d, asDir, asLink := filepath.Join(ws, "d"), filepath.Join(ws, "d.dir"), filepath.Join(ws, "d.link")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "token"), []byte("b"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../.actlet", asLink); err != nil {
		t.Fatal(err)
	}
	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; !stop.Load(); i++ {
			if i%2 == 0 { // the directory out, the link in
				_ = os.Rename(d, asDir)
				_ = os.Rename(asLink, d)
			} else {
				_ = os.Rename(d, asLink)
				_ = os.Rename(asDir, d)
			}
		}
	}()
	defer func() { stop.Store(true); wg.Wait() }()
	for range 20000 {
		resp, err := ReadFile(dir, &attachv0.ReadFileRequest{Path: "d/token"})
		if err == nil && strings.Contains(string(resp.GetContent()), secret) {
			t.Fatal("read the state directory through a swapped directory")
		}
	}
}

// recordingBox records the commands it is given.
type recordingBox struct {
	mu   sync.Mutex
	cmds []*gatewayv0.Command
}

func (b *recordingBox) Info() *attachv0.InfoResponse { return &attachv0.InfoResponse{} }

func (b *recordingBox) Apply(_ context.Context, cmd *gatewayv0.Command) (*gatewayv0.CommandAck, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cmds = append(b.cmds, cmd)
	return &gatewayv0.CommandAck{CommandId: cmd.GetId()}, nil
}

// FuzzCommand sends any command request a viewer might: only the commands
// the profile allows reach the box, as the viewer's, never empty notes.
func FuzzCommand(f *testing.F) {
	for _, req := range []*attachv0.CommandRequest{
		{Action: &attachv0.CommandRequest_Steer{Steer: &gatewayv0.Steer{Note: "hi"}}, Actor: "ann"},
		{Action: &attachv0.CommandRequest_Steer{Steer: &gatewayv0.Steer{Note: "  "}}},
		{Action: &attachv0.CommandRequest_Stop{Stop: &gatewayv0.Stop{Kill: true}}},
		{Action: &attachv0.CommandRequest_Pause{Pause: &gatewayv0.Pause{}}},
		{},
	} {
		b, _ := proto.Marshal(req)
		f.Add(b)
	}
	box := &recordingBox{}
	srv := NewServer("s1", NewHub(HubOptions{}), box, []string{CommandSteer}, slog.New(slog.DiscardHandler))
	f.Fuzz(func(t *testing.T, data []byte) {
		req := new(attachv0.CommandRequest)
		if proto.Unmarshal(data, req) != nil {
			return
		}
		box.mu.Lock()
		before := len(box.cmds)
		box.mu.Unlock()
		_, err := srv.Command(context.Background(), req)
		box.mu.Lock()
		defer box.mu.Unlock()
		if len(box.cmds) == before {
			return
		}
		cmd := box.cmds[len(box.cmds)-1]
		if err != nil || cmd.GetSteer() == nil || strings.TrimSpace(cmd.GetSteer().GetNote()) == "" ||
			cmd.GetOrigin() != gatewayv0.Origin_ORIGIN_LOCAL || !strings.HasPrefix(cmd.GetId(), "local-") {
			t.Fatalf("the box got %v (err %v)", cmd, err)
		}
	})
}
