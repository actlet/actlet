package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/actlet/actlet/internal/journal"
)

// writeJournal creates a journal with a few text records and one binary
// record and returns its path.
func writeJournal(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.actlet")
	w, err := journal.Create(path, journal.DefaultOptions("cli-test"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, rec := range []struct{ kind, data string }{
		{"session.start", `{"profile":"default"}`},
		{"tool.intent", `{"tool":"shell","cmd":"go test ./..."}`},
		{"tool.outcome", `{"exit":0}`},
		{"blob", "\xff\x00\x01"},
	} {
		if _, err := w.Append(ctx, rec.kind, []byte(rec.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestJournalInspect(t *testing.T) {
	path := writeJournal(t)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"journal", "inspect", path}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	t.Logf("inspect output:\n%s", stdout.String())
	for _, want := range []string{"session:  cli-test", "batches:  4", "records:  4 (seq 1-4)", "status:   ok"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("output missing %q", want)
		}
	}
}

func TestJournalInspectTornAndCorrupt(t *testing.T) {
	path := writeJournal(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	torn := filepath.Join(t.TempDir(), "torn.actlet")
	if err := os.WriteFile(torn, data[:len(data)-5], 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"journal", "inspect", "-summary", torn}, &stdout, &stderr); code != 0 {
		t.Fatalf("torn: exit %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "records:  3 (seq 1-3)") || !strings.Contains(stdout.String(), "incomplete final batch") {
		t.Errorf("torn: unexpected output:\n%s", stdout.String())
	}

	corrupt := filepath.Join(t.TempDir(), "corrupt.actlet")
	c := bytes.Clone(data)
	c[len(c)-10] ^= 0x40
	if err := os.WriteFile(corrupt, c, 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if code := run([]string{"journal", "inspect", "-summary", corrupt}, &stdout, &stderr); code != 1 {
		t.Fatalf("corrupt: exit %d, want 1", code)
	}
	if !strings.Contains(stdout.String(), "INVALID") {
		t.Errorf("corrupt: unexpected output:\n%s", stdout.String())
	}
}

func TestJournalExportJSONL(t *testing.T) {
	path := writeJournal(t)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"journal", "export", "--jsonl", path}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	t.Logf("export output:\n%s", stdout.String())

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("got %d lines, want 4", len(lines))
	}
	var recs []exportRecord
	for _, l := range lines {
		var r exportRecord
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatalf("line %q: %v", l, err)
		}
		recs = append(recs, r)
	}
	if recs[1].Seq != 2 || recs[1].Kind != "tool.intent" || recs[1].Data == nil || !strings.Contains(*recs[1].Data, "go test") {
		t.Errorf("record 2 = %+v", recs[1])
	}
	if recs[3].Data != nil || !bytes.Equal(recs[3].DataBase64, []byte("\xff\x00\x01")) {
		t.Errorf("binary record = %+v, want data_base64", recs[3])
	}
	if recs[0].Time.IsZero() {
		t.Error("record time missing")
	}
}

func TestJournalUsageErrors(t *testing.T) {
	path := writeJournal(t)
	for _, args := range [][]string{
		{"journal"},
		{"journal", "bogus"},
		{"journal", "inspect"},
		{"journal", "inspect", path, "extra"},
		{"journal", "export", path}, // no format
	} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 2 {
			t.Errorf("%q: exit %d, want 2", args, code)
		}
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"journal", "inspect", filepath.Join(t.TempDir(), "missing")}, &stdout, &stderr); code != 1 {
		t.Errorf("missing file: exit %d, want 1", code)
	}
}
