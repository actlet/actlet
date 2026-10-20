package main

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/types/known/timestamppb"

	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
	"github.com/actlet/actlet/sdk"
)

func event(epoch, seq uint64, body func(*gatewayv0.ActivityEvent)) *gatewayv0.ActivityEvent {
	ev := &gatewayv0.ActivityEvent{Id: sdk.EventID(epoch, seq), Epoch: epoch, Seq: seq, Time: timestamppb.Now(),
		Body: &gatewayv0.ActivityEvent_Text{Text: &gatewayv0.Text{Text: "working"}}}
	if body != nil {
		body(ev)
	}
	return ev
}

// writeRecording writes a gateway recording with two epochs and a gap, and
// returns its path.
func writeRecording(t *testing.T) string {
	t.Helper()
	store, err := sdk.NewRecordingStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	started := func(ev *gatewayv0.ActivityEvent) {
		ev.Body = &gatewayv0.ActivityEvent_Started{Started: &gatewayv0.SessionStarted{Epoch: ev.GetEpoch()}}
	}
	gap := func(ev *gatewayv0.ActivityEvent) {
		ev.Body = &gatewayv0.ActivityEvent_Gap{Gap: &gatewayv0.Gap{Dropped: 7}}
	}
	batches := [][]*gatewayv0.ActivityEvent{
		{event(1, 1, started), event(1, 2, nil), event(1, 3, nil)},
		{event(2, 1, started), event(2, 2, gap), event(2, 3, nil)},
	}
	for _, b := range batches {
		if err := store.Append(ctx, "sess-1", b); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return store.Path("sess-1")
}

func TestReplayInspect(t *testing.T) {
	path := writeRecording(t)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"replay", "inspect", path}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	t.Logf("replay inspect:\n%s", stdout.String())
	for _, want := range []string{
		"session:  sess-1", "events:   6 in 2 epochs", "epoch 1: seq 1-3 (3 events)", "epoch 2: seq 1-3",
		"kinds:    gap 1, started 2, text 3", "dropped:  7 display-only events, reported by 1 gap events", "status:   ok",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("output missing %q", want)
		}
	}
}

func TestReplayInspectReportsHoles(t *testing.T) {
	store, err := sdk.NewRecordingStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// A worker's spill file can start past 1 and, unlike a gateway, the
	// store below doesn't check order: write a hole on purpose.
	if err := store.Append(context.Background(), "s", []*gatewayv0.ActivityEvent{event(1, 1, nil), event(1, 4, nil)}); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	var stdout, stderr bytes.Buffer
	run([]string{"replay", "inspect", store.Path("s")}, &stdout, &stderr)
	if !strings.Contains(stdout.String(), "2 missing") {
		t.Errorf("hole not reported:\n%s", stdout.String())
	}
}

func TestReplayExportJSONL(t *testing.T) {
	path := writeRecording(t)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"replay", "export", "--jsonl", path}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	out := stdout.String()
	var events []*gatewayv0.ActivityEvent
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		ev := new(gatewayv0.ActivityEvent)
		if err := protojson.Unmarshal(sc.Bytes(), ev); err != nil {
			t.Fatalf("line %q: %v", sc.Text(), err)
		}
		events = append(events, ev)
	}
	if len(events) != 6 || events[4].GetGap().GetDropped() != 7 || events[3].GetId() != "2.1" {
		t.Fatalf("exported %d events: %v", len(events), events)
	}
	if !strings.Contains(out, `"epoch"`) {
		t.Error("export doesn't use proto field names")
	}
}

func TestReplayCorruptAndUsage(t *testing.T) {
	path := writeRecording(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Damage the first batch's payload: everything after it is unreadable.
	data[len(encodedHeader(t, path))+40] ^= 0xff
	bad := filepath.Join(t.TempDir(), "bad.actrec")
	if err := os.WriteFile(bad, data, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"replay", "inspect", bad}, &stdout, &stderr); code != 1 || !strings.Contains(stdout.String(), "INVALID") {
		t.Errorf("corrupt inspect: exit %d\n%s", code, stdout.String())
	}
	if code := run([]string{"replay", "export", "--jsonl", bad}, &stdout, &stderr); code != 1 {
		t.Errorf("corrupt export: exit %d", code)
	}
	for _, args := range [][]string{{"replay", "inspect"}, {"replay", "export", path}, {"replay", "inspect", path, "x"}} {
		if code := run(args, &stdout, &stderr); code != 2 {
			t.Errorf("%q: exit %d, want 2", args, code)
		}
	}
	if code := run([]string{"replay", "inspect", filepath.Join(t.TempDir(), "missing")}, &stdout, &stderr); code != 1 {
		t.Errorf("missing file: exit %d, want 1", code)
	}
}

// encodedHeader returns the file header bytes of a journal: magic, version,
// flags, session ID length, session ID and CRC.
func encodedHeader(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	n := 14 + int(data[12]) + int(data[13])<<8 + 4
	return data[:n]
}

// The agent's events are named by kind and counted by worker, and an
// event from a newer worker is "unknown".
func TestReplayAgentEvents(t *testing.T) {
	store, err := sdk.NewRecordingStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bodies := []func(*gatewayv0.ActivityEvent){
		func(ev *gatewayv0.ActivityEvent) {
			ev.Body = &gatewayv0.ActivityEvent_ModelTurn{ModelTurn: &gatewayv0.ModelTurn{Turn: 1, Text: "hi"}}
		},
		func(ev *gatewayv0.ActivityEvent) {
			ev.Body = &gatewayv0.ActivityEvent_ToolStarted{ToolStarted: &gatewayv0.ToolStarted{Tool: "shell"}}
		},
		func(ev *gatewayv0.ActivityEvent) {
			ev.Body = &gatewayv0.ActivityEvent_ToolFinished{ToolFinished: &gatewayv0.ToolFinished{Tool: "shell"}}
		},
		func(ev *gatewayv0.ActivityEvent) {
			ev.Body = &gatewayv0.ActivityEvent_FileChange{FileChange: &gatewayv0.FileChange{Path: "a", Diff: "+x"}}
		},
		func(ev *gatewayv0.ActivityEvent) {
			ev.Worker = "root.1"
			ev.Body = &gatewayv0.ActivityEvent_SubagentSpawned{SubagentSpawned: &gatewayv0.SubagentSpawned{Parent: "root"}}
		},
		func(ev *gatewayv0.ActivityEvent) {
			ev.Worker = "root.1"
			ev.Body = &gatewayv0.ActivityEvent_SubagentFinished{SubagentFinished: &gatewayv0.SubagentFinished{Result: "ok"}}
		},
		func(ev *gatewayv0.ActivityEvent) {
			ev.Body = nil // a body this build doesn't know
			ev.ProtoReflect().SetUnknown(protowire.AppendString(protowire.AppendTag(nil, 99, protowire.BytesType), "new"))
		},
	}
	var evs []*gatewayv0.ActivityEvent
	for i, body := range bodies {
		evs = append(evs, event(1, uint64(i+1), func(ev *gatewayv0.ActivityEvent) {
			ev.Worker = "root"
			body(ev)
		}))
	}
	if err := store.Append(context.Background(), "s", evs); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()

	var stdout, stderr bytes.Buffer
	if code := run([]string{"replay", "inspect", store.Path("s")}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	for _, want := range []string{
		"kinds:    file_change 1, model_turn 1, subagent_finished 1, subagent_spawned 1, tool_finished 1, tool_started 1, unknown 1",
		"workers:  root 5, root.1 2",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("inspect output lacks %q:\n%s", want, stdout.String())
		}
	}
	stdout.Reset()
	if code := run([]string{"replay", "export", "--jsonl", store.Path("s")}, &stdout, &stderr); code != 0 {
		t.Fatalf("export: exit %d: %s", code, stderr.String())
	}
	for _, want := range []string{`"model_turn":{"turn":"1"`, `"tool_started":{"tool":"shell"}`, `"file_change":{"path":"a"`,
		`"subagent_spawned":{"parent":"root"}`, `"worker":"root.1"`} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("export lacks %s", want)
		}
	}
}
