//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

// Unix only: the end-to-end tests stop and crash actlet with signals and
// read its wait status, which Windows doesn't have.

package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"

	"github.com/actlet/actlet/internal/agent"
	"github.com/actlet/actlet/internal/journal"
	"github.com/actlet/actlet/internal/session"
	"github.com/actlet/actlet/internal/sessionfs"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// journalKinds returns the kinds of the session journal's records.
func journalKinds(t *testing.T, dir string) []string {
	t.Helper()
	f, err := os.Open(sessionfs.Folder{Dir: dir}.Journal())
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
	var kinds []string
	for {
		b, err := r.Next()
		if errors.Is(err, io.EOF) || errors.Is(err, journal.ErrTornTail) {
			return kinds
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, rec := range b.Records {
			kinds = append(kinds, rec.Kind)
		}
	}
}

// The second turn fills the context past the threshold; the first is
// summarised, and the third sees the summary once, after the task.
const compactionScript = `
turns:
  - text: Reading it.
    tool_calls:
      - name: shell
        input: {command: "echo once >> side.txt"}
  - text: Counting.
    usage: {input: 900, output: 10}
    tool_calls:
      - name: glob
        input: {pattern: "*.txt"}
  - expect_once: [SUMMARY-7f3a, Counting.]
    text: The sum is 3.
summary:
  text: SUMMARY-7f3a ran the shell command once.
  usage: {input: 400, output: 20}
`

// A worker compacts its context at the threshold and carries on; SIGKILL
// during the compaction, before or after its summary is journaled,
// recovers with one compaction.
func TestCompactionCrash(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	for _, crashAt := range []string{"", "compaction.intent", "compaction.outcome"} {
		name := crashAt
		if name == "" {
			name = "no crash"
		}
		t.Run(name, func(t *testing.T) {
			g := newAgentGateway(t, compactionScript, func(g *agentGateway) {
				g.models = &gatewayv0.ModelCatalog{
					Aliases: []*gatewayv0.ModelAlias{{Alias: "scripted", ContextWindow: 1000}, {Alias: "summariser"}},
					Roles:   map[string]string{"main": "scripted", "compaction": "summariser"},
				}
				g.compaction = &gatewayv0.CompactionPolicy{KeepTurns: 1}
			})
			dir := sessionDir(t, nil)
			if crashAt != "" {
				if code := exitCode(t, g.worker(dir, crashAt)); code != -int(syscall.SIGKILL) {
					t.Fatalf("first run: exit %d, want killed", code)
				}
			}
			if code := exitCode(t, g.worker(dir, "")); code != session.ExitCompleted {
				t.Fatalf("exit %d, want 0", code)
			}
			kinds := journalKinds(t, dir)
			i := slices.Index(kinds, agent.KindCompactionIntent)
			if i < 1 || kinds[i-1] != agent.KindCheckpoint {
				t.Errorf("journal %v", kinds)
			}
			if n := len(slices.DeleteFunc(slices.Clone(kinds), func(k string) bool { return k != agent.KindCompactionOutcome })); n != 1 {
				t.Errorf("%d compactions", n)
			}
			summaries := 0
			for _, c := range g.model.Calls() {
				if c.Script == "summary" {
					summaries++
				}
			}
			if summaries != 1 {
				t.Errorf("%d summary calls", summaries)
			}
			// Activity the worker still held when it was killed is lost,
			// as after a compaction's outcome.
			events := g.eventCount(func(ev *gatewayv0.ActivityEvent) bool {
				c := ev.GetCompaction()
				return c != nil && c.GetModel() == "summariser" && c.GetSummarizedTurns() == 1 && c.GetKeptTurns() == 1 && c.GetContextTokens() == 910
			})
			if events > 1 || events == 0 && crashAt != "compaction.outcome" {
				t.Errorf("%d compaction events", events)
			}
			if got := readFile(t, filepath.Join(dir, "workspace", "side.txt")); got != "once\n" {
				t.Errorf("side.txt = %q", got)
			}
		})
	}
}
