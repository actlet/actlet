package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/actlet/actlet/internal/session"
	"github.com/actlet/actlet/internal/sessionfs"
)

// parentSession is the compatibility fixture's demo session, in a folder
// of its own.
func parentSession(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	f := sessionfs.Folder{Dir: dir}
	for from, to := range map[string]string{"session.actlet": f.Journal(), "task.json": f.TaskFile()} {
		data, err := os.ReadFile(filepath.Join("..", "..", "test", "compat", "v0", from))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(to, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestFork(t *testing.T) {
	parent := parentSession(t)
	var out, errOut bytes.Buffer
	if code := run([]string{"fork", "--list", parent}, &out, &errOut); code != 0 {
		t.Fatalf("--list: exit %d: %s", code, errOut.String())
	}
	if !strings.HasPrefix(out.String(), "SEQ") || strings.Count(out.String(), "\n") < 3 {
		t.Fatalf("--list:\n%s", out.String())
	}

	// Flags may follow the folder.
	child := filepath.Join(t.TempDir(), "child")
	out.Reset()
	if code := run([]string{"fork", parent, "--checkpoint", "turn 2", "--to", child, "--session-id", "forked-1"}, &out, &errOut); code != 0 {
		t.Fatalf("fork: exit %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "(turn 2)") || !strings.Contains(out.String(), "as session forked-1") {
		t.Errorf("fork said:\n%s", out.String())
	}
	rec, err := session.Recover(sessionfs.Folder{Dir: child})
	if err != nil || rec.SessionID != "forked-1" || rec.Lineage.GetTurn() != 2 || !rec.Resumable() || len(rec.Agent) == 0 {
		t.Fatalf("the fork's journal: %+v, %v", rec, err)
	}
	if _, err := os.Stat(filepath.Join(child, "task.json")); err != nil {
		t.Error(err)
	}

	for name, args := range map[string][]string{
		"no --to":        {"fork", parent},
		"bad checkpoint": {"fork", "--checkpoint", "soon", "--to", t.TempDir() + "/x", parent},
		"turn 0":         {"fork", "--checkpoint", "turn 0", "--to", t.TempDir() + "/x", parent},
		"two folders":    {"fork", parent, parent, "--to", t.TempDir() + "/x"},
	} {
		if code := run(args, &out, &errOut); code != 2 {
			t.Errorf("%s: exit %d", name, code)
		}
	}
	for name, args := range map[string][]string{
		"no such turn":  {"fork", "--checkpoint", "turn 99", "--to", t.TempDir() + "/x", parent},
		"no such seq":   {"fork", "--checkpoint", "999999", "--to", t.TempDir() + "/x", parent},
		"not empty":     {"fork", "--to", child, parent},
		"not a session": {"fork", "--to", t.TempDir() + "/x", t.TempDir()},
	} {
		errOut.Reset()
		if code := run(args, &out, &errOut); code != 1 || errOut.Len() == 0 {
			t.Errorf("%s: exit %d: %s", name, code, errOut.String())
		}
	}
}
