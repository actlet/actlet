package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
	"github.com/actlet/actlet/sdk"
)

var fixture = filepath.Join("..", "..", "test", "compat", "v0")

func searchLines(t *testing.T, want int, args ...string) []string {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(append([]string{"replay", "search"}, args...), &out, &errOut)
	if code != want {
		t.Fatalf("search %v: exit %d, want %d: %s", args, code, want, errOut.String())
	}
	return strings.FieldsFunc(out.String(), func(r rune) bool { return r == '\n' })
}

// Every filter, on the demo recording, and the same from its export.
func TestReplaySearch(t *testing.T) {
	for _, file := range []string{"recording.actrec", "events.jsonl"} {
		rec := filepath.Join(fixture, file)
		t.Run(file, func(t *testing.T) {
			all := searchLines(t, 0, rec)
			if len(all) != 81 {
				t.Fatalf("%d events", len(all))
			}
			for _, tc := range []struct {
				args  []string
				n     int
				every string
			}{
				{[]string{"--kind", "steer"}, 1, "steer: "},
				{[]string{"--kind", "tool_started,tool_finished", "--tool", "python"}, 4, "python"},
				{[]string{"--worker", "root.2"}, 10, "root.2"},
				{[]string{"--worker", "root", "--kind", "model_turn"}, 9, "model_turn"},
				{[]string{"--regex", "--kind", "file_change", `Revenue: \d+ EUR`}, 3, "file_change analysis/"},
				{[]string{"KEYERROR"}, 2, ""},
				{[]string{"--limit", "3"}, 3, ""},
			} {
				lines := searchLines(t, 0, append([]string{rec}, tc.args...)...)
				if len(lines) != tc.n {
					t.Errorf("%v: %d matches, want %d:\n%s", tc.args, len(lines), tc.n, strings.Join(lines, "\n"))
				}
				for _, l := range lines {
					if !strings.Contains(l, tc.every) {
						t.Errorf("%v: %q lacks %q", tc.args, l, tc.every)
					}
				}
			}
			// A window after the first event, then an absolute time.
			t0, err := time.Parse(time.RFC3339Nano, strings.Fields(all[0])[1])
			if err != nil {
				t.Fatal(err)
			}
			window := searchLines(t, 0, rec, "--since", "2s", "--until", "3s")
			for _, l := range window {
				at, err := time.Parse(time.RFC3339Nano, strings.Fields(l)[1])
				if err != nil || at.Before(t0.Add(2*time.Second)) || at.After(t0.Add(3*time.Second)) {
					t.Errorf("--since 2s --until 3s matched %q", l)
				}
			}
			if len(window) == 0 || len(window) == len(all) {
				t.Errorf("--since 2s --until 3s matched %d of %d", len(window), len(all))
			}
			first := strings.Fields(all[0])[1]
			if n := len(searchLines(t, 0, "--until", first, rec)); n < 1 {
				t.Errorf("--until %s: %d", first, n)
			}
			searchLines(t, 1, rec, "nothing like this")
			// JSON, one event a line.
			for _, l := range searchLines(t, 0, "--json", "--kind", "model_turn", rec) {
				ev := new(gatewayv0.ActivityEvent)
				if err := protojson.Unmarshal([]byte(l), ev); err != nil || ev.GetModelTurn() == nil {
					t.Fatalf("JSON line %q: %v", l, err)
				}
			}
		})
	}
	for _, args := range [][]string{
		{"--regex", "(", filepath.Join(fixture, "events.jsonl")},
		{"--kind", "tool_done", filepath.Join(fixture, "events.jsonl")},
		{"--since", "yesterday", filepath.Join(fixture, "events.jsonl")},
		{},
	} {
		searchLines(t, 2, args...)
	}
}

// Searching a recording of 50,000 events takes little memory: it streams.
func TestReplaySearchBoundedMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("writes a large recording")
	}
	store, err := sdk.NewRecordingStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const total, batch = 50000, 500
	text := strings.Repeat("the agent's output, line after line; ", 28) // about 1 KiB
	start := time.Now()
	for i := 0; i < total; i += batch {
		var evs []*gatewayv0.ActivityEvent
		for j := i; j < i+batch; j++ {
			seq := uint64(j + 1)
			evs = append(evs, &gatewayv0.ActivityEvent{Id: sdk.EventID(1, seq), Epoch: 1, Seq: seq, Worker: "root",
				Time: timestamppb.New(start.Add(time.Duration(j) * time.Millisecond)),
				Body: &gatewayv0.ActivityEvent_Text{Text: &gatewayv0.Text{Text: fmt.Sprintf("%s %d", text, j)}}})
		}
		if err := store.Append(context.Background(), "big", evs); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	path := store.Path("big")
	if fi, err := os.Stat(path); err == nil {
		t.Logf("recording: %d events, %.1f MiB on disk, %.1f MiB of text", total, float64(fi.Size())/(1<<20), float64(total*len(text))/(1<<20))
	}

	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	var peak atomic.Uint64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var m runtime.MemStats
		for {
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
			runtime.ReadMemStats(&m)
			if m.HeapInuse > peak.Load() {
				peak.Store(m.HeapInuse)
			}
		}
	}()
	var out bytes.Buffer
	code := run([]string{"replay", "search", path, " 49999"}, &out, io.Discard)
	close(stop)
	wg.Wait()
	if code != 0 || strings.Count(out.String(), "\n") != 1 {
		t.Fatalf("exit %d:\n%.300s", code, out.String())
	}
	grew := int64(peak.Load()) - int64(base.HeapInuse)
	t.Logf("heap in use grew by at most %.1f MiB", float64(grew)/(1<<20))
	if grew > 24<<20 {
		t.Errorf("the search took %.1f MiB of heap for %d events", float64(grew)/(1<<20), total)
	}
}
