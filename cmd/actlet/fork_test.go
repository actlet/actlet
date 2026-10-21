//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

// Unix only: the end-to-end tests stop and crash actlet with signals and
// read its wait status, which Windows doesn't have.

package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/actlet/actlet/internal/session"
	"github.com/actlet/actlet/internal/sessionfs"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
	"github.com/actlet/actlet/sdk"
)

// forkScript writes a file a turn, runs a command in the third, and ends.
const forkScript = `
turns:
  - text: one
    tool_calls:
      - name: write_file
        input: {path: a.txt, content: "a"}
  - text: two
    tool_calls:
      - name: write_file
        input: {path: b.txt, content: "b"}
  - text: three
    tool_calls:
      - name: shell
        input: {command: "echo c > c.txt"}
  - expect:
      - contains: exit code 0
    text: done
`

func fileSum(t *testing.T, path string) [32]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(data)
}

// forkAndRun forks the session in parent, runs the fork to completion
// against the gateway, and checks its lineage and that the parent is
// untouched.
func forkAndRun(t *testing.T, g *agentGateway, parent string, opts sdk.ForkOptions, wantTurn uint64) (string, *sdk.Fork) {
	t.Helper()
	parentID := mustRecover(t, parent).SessionID
	before := fileSum(t, sessionfs.Folder{Dir: parent}.Journal())
	child := filepath.Join(t.TempDir(), "fork")
	f, err := sdk.ForkSession(parent, child, opts)
	if err != nil {
		t.Fatal(err)
	}
	if f.SessionID == parentID || f.Lineage.GetParentSessionId() != parentID || f.Lineage.GetTurn() != wantTurn {
		t.Fatalf("fork %s of %s: lineage %v", f.SessionID, parentID, f.Lineage)
	}
	if code := exitCode(t, g.worker(child, "")); code != session.ExitCompleted {
		t.Fatalf("the fork: exit %d, want 0", code)
	}
	if fileSum(t, sessionfs.Folder{Dir: parent}.Journal()) != before {
		t.Error("the parent's journal changed")
	}
	rec := mustRecover(t, child)
	if rec.SessionID != f.SessionID || rec.Lineage.GetCheckpointSeq() != f.Lineage.GetCheckpointSeq() ||
		rec.EndedOutcome().State != gatewayv0.SessionState_SESSION_STATE_COMPLETED {
		t.Errorf("the fork's journal: %s, lineage %v, ended %v", rec.SessionID, rec.Lineage, rec.Ended)
	}
	if l := g.srv.Session(f.SessionID).Lineage(); l.GetParentSessionId() != parentID || l.GetTurn() != wantTurn {
		t.Errorf("the gateway's lineage for the fork: %v", l)
	}
	if n := g.eventCount(func(ev *gatewayv0.ActivityEvent) bool {
		return ev.GetSessionInfo().GetLineage().GetParentSessionId() == parentID
	}); n != 1 {
		t.Errorf("%d session_info events with the lineage", n)
	}
	return child, f
}

func mustRecover(t *testing.T, dir string) *session.Recovered {
	t.Helper()
	rec, err := session.Recover(sessionfs.Folder{Dir: dir})
	if err != nil || rec == nil {
		t.Fatalf("recovering %s: %v", dir, err)
	}
	return rec
}

// A finished session forked at turn 2 carries on from there as a session
// of its own: it runs the second turn's call and the rest of the task.
func TestForkAtTurn(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	g := newAgentGateway(t, forkScript)
	parent := sessionDir(t, nil)
	if code := exitCode(t, g.worker(parent, "")); code != session.ExitCompleted {
		t.Fatalf("the parent: exit %d", code)
	}
	child, _ := forkAndRun(t, g, parent, sdk.ForkOptions{Turn: 2}, 2)
	// The fork wrote b.txt and c.txt itself; a.txt was the parent's.
	ws := filepath.Join(child, "workspace")
	for name, want := range map[string]string{"a.txt": "", "b.txt": "b", "c.txt": "c\n"} {
		if got := readFile(t, filepath.Join(ws, name)); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

// A session killed mid-command forks from its latest checkpoint.
func TestForkCrashedSession(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	g := newAgentGateway(t, forkScript)
	parent := sessionDir(t, nil)
	if code := exitCode(t, g.worker(parent, "tool.done:shell")); code != -int(syscall.SIGKILL) {
		t.Fatalf("the parent: exit %d, want killed", code)
	}
	forkAndRun(t, g, parent, sdk.ForkOptions{CopyWorkspace: true}, 3)
}

// A checkpoint too large for one record, packed in parts, forks too.
func TestForkPackedCheckpoint(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	// Three answers of 1.5 MiB make a checkpoint over the 4 MiB record
	// limit; the text is in words, as the model streams it.
	r := rand.New(rand.NewPCG(1, 2))
	big := func() string {
		var sb strings.Builder
		for sb.Len() < 1536<<10 {
			for range 1000 {
				sb.WriteByte(byte('a' + r.IntN(26)))
			}
			sb.WriteByte(' ')
		}
		return sb.String()
	}
	var script bytes.Buffer
	script.WriteString("turns:\n")
	for i := range 3 {
		fmt.Fprintf(&script, "  - text: %q\n    tool_calls:\n      - name: write_file\n        input: {path: f%d.txt, content: x}\n", big(), i)
	}
	script.WriteString("  - text: done\n")
	g := newAgentGateway(t, script.String())
	parent := sessionDir(t, nil)
	if code := exitCode(t, g.worker(parent, "")); code != session.ExitCompleted {
		t.Fatalf("the parent: exit %d", code)
	}
	cps, err := sdk.SessionCheckpoints(parent)
	if err != nil {
		t.Fatal(err)
	}
	var packed *sdk.Checkpoint
	for i := range cps {
		if cps[i].Packed && cps[i].Turn == 3 {
			packed = &cps[i]
		}
	}
	if packed == nil {
		t.Fatalf("no packed checkpoint at turn 3 in %+v", cps)
	}
	forkAndRun(t, g, parent, sdk.ForkOptions{Seq: packed.Seq}, 3)
}
