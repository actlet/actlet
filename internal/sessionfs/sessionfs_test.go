package sessionfs

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLayout(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "s")
	f, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"inputs", "workspace", "output", ".actlet"} {
		if st, err := os.Stat(filepath.Join(dir, d)); err != nil || !st.IsDir() {
			t.Errorf("%s missing: %v", d, err)
		}
	}
	if f.Journal() != filepath.Join(dir, ".actlet", "session.actlet") {
		t.Errorf("journal path %s", f.Journal())
	}
}

func TestTaskRoundTrip(t *testing.T) {
	f, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.ReadTask(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadTask without task.json: %v", err)
	}
	task := &Task{Schema: TaskSchema, ID: "t1", Kind: "demo",
		Definition: json.RawMessage(`{"steps":2}`), Input: json.RawMessage(`{"message":"hi"}`)}
	if err := f.WriteTask(task); err != nil {
		t.Fatal(err)
	}
	got, err := f.ReadTask()
	if err != nil {
		t.Fatal(err)
	}
	var in bytes.Buffer
	if err := json.Compact(&in, got.Input); err != nil || got.ID != "t1" || in.String() != `{"message":"hi"}` {
		t.Errorf("read back %+v", got)
	}
}

func TestTaskValidation(t *testing.T) {
	f, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{
		`{"schema":"v9","id":"t","kind":"demo"}`,
		`{"schema":"v0","kind":"demo"}`,
		`{"schema":"v0","id":"t"}`,
		`{"schema":"v0","id":"t","kind":"demo","extra":1}`,
		`not json`,
	} {
		if err := os.WriteFile(f.TaskFile(), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := f.ReadTask(); err == nil {
			t.Errorf("accepted task.json %s", content)
		}
	}
	if err := f.WriteTask(&Task{Schema: TaskSchema, ID: "t", Kind: "k", Definition: json.RawMessage(`{bad`)}); err == nil {
		t.Error("wrote a task with invalid JSON")
	}
}

func TestStatus(t *testing.T) {
	f, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if s, err := f.ReadStatus(); s != "" || err != nil {
		t.Fatalf("ReadStatus = %q, %v", s, err)
	}
	if err := f.WriteStatus(StatusPaused); err != nil {
		t.Fatal(err)
	}
	if s, _ := f.ReadStatus(); s != StatusPaused {
		t.Errorf("status %q", s)
	}
}
