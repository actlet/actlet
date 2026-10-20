package compat

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/actlet/actlet/internal/agent"
	"github.com/actlet/actlet/internal/journal"
	"github.com/actlet/actlet/internal/session"
	"github.com/actlet/actlet/internal/sessionfs"
	"github.com/actlet/actlet/internal/view"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
	"github.com/actlet/actlet/sdk"
)

// versions are the format versions with fixtures.
var versions = []string{"v0"}

func copyTo(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from)
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

// The session journal recovers to the completed session it recorded, and
// opens for appending without truncating anything.
func TestJournal(t *testing.T) {
	for _, v := range versions {
		t.Run(v, func(t *testing.T) {
			folder := sessionfs.Folder{Dir: t.TempDir()}
			copyTo(t, filepath.Join(v, "session.actlet"), folder.Journal())
			rec, err := session.Recover(folder)
			if err != nil {
				t.Fatal(err)
			}
			if rec.SessionID == "" || rec.Epochs != 1 || rec.EndedOutcome().State != gatewayv0.SessionState_SESSION_STATE_COMPLETED {
				t.Errorf("recovered %s, %d runs, %v", rec.SessionID, rec.Epochs, rec.EndedOutcome())
			}
			if len(rec.Agent) == 0 || rec.Agent[0].Kind != agent.KindCheckpoint {
				t.Errorf("agent records from %v", rec.Agent)
			}
			w, err := journal.Open(folder.Journal(), journal.DefaultOptions(rec.SessionID))
			if err != nil {
				t.Fatal(err)
			}
			if r := w.Recovery(); r.Truncated() != 0 {
				t.Errorf("opening truncated %d bytes", r.Truncated())
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// events reads a recording's activity events.
func events(t *testing.T, path string) []*gatewayv0.ActivityEvent {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	r, err := journal.NewReader(f, st.Size(), journal.ReaderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var out []*gatewayv0.ActivityEvent
	for {
		b, err := r.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, rec := range b.Records {
			ev := new(gatewayv0.ActivityEvent)
			if err := proto.Unmarshal(rec.Data, ev); err != nil {
				t.Fatal(err)
			}
			out = append(out, ev)
		}
	}
}

// The recording holds the events of its JSON Lines export, and folds
// into the same view as when it was written.
func TestRecording(t *testing.T) {
	for _, v := range versions {
		t.Run(v, func(t *testing.T) {
			evs := events(t, filepath.Join(v, "recording.actrec"))
			f, err := os.Open(filepath.Join(v, "events.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = f.Close() }()
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 64<<10), 16<<20)
			n := 0
			for ; sc.Scan(); n++ {
				ev := new(gatewayv0.ActivityEvent)
				if err := protojson.Unmarshal(sc.Bytes(), ev); err != nil {
					t.Fatalf("line %d: %v", n+1, err)
				}
				if n >= len(evs) || !proto.Equal(ev, evs[n]) {
					t.Fatalf("line %d differs from the recording", n+1)
				}
			}
			if n != len(evs) || n == 0 {
				t.Fatalf("%d lines, %d events", n, len(evs))
			}

			s := view.New()
			for _, ev := range evs {
				s.Apply(ev)
			}
			var got bytes.Buffer
			enc := json.NewEncoder(&got)
			enc.SetIndent("", "  ")
			enc.SetEscapeHTML(false)
			if err := enc.Encode(s.ViewState()); err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(filepath.Join(v, "view.json"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Bytes(), want) {
				t.Errorf("the view differs from %s/view.json; if the change is deliberate, it breaks viewers of the format", v)
			}
		})
	}
}

// The profile reads the same from YAML and JSON, and task.json reads.
func TestProfileAndTask(t *testing.T) {
	for _, v := range versions {
		t.Run(v, func(t *testing.T) {
			var wire []*gatewayv0.Profile
			for _, name := range []string{"profile.yaml", "profile.json"} {
				p, err := sdk.LoadProfile(filepath.Join(v, name))
				if err != nil {
					t.Fatal(err)
				}
				pb, err := p.Proto()
				if err != nil {
					t.Fatal(err)
				}
				wire = append(wire, pb)
			}
			if !proto.Equal(wire[0], wire[1]) || wire[0].GetId() == "" || len(wire[0].GetBudgets()) == 0 {
				t.Errorf("profiles:\n%v\n%v", wire[0], wire[1])
			}
			folder := sessionfs.Folder{Dir: t.TempDir()}
			copyTo(t, filepath.Join(v, "task.json"), folder.TaskFile())
			task, err := folder.ReadTask()
			if err != nil || task.ID == "" || task.Kind != agent.Kind {
				t.Errorf("task %+v, %v", task, err)
			}
		})
	}
}
