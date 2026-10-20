package agent

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/actlet/actlet/internal/agent/agentpb"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// compactionScript is six turns, the third of which fills the context
// past the threshold. The last checks the conversation it gets: the task
// with the summary, then the turns kept and those since.
func compactionScript(t *testing.T) []turn {
	glob := [][2]string{{"glob", `{"pattern":"*.none"}`}}
	ts := []turn{
		{text: "one", calls: glob},
		{text: "two", calls: glob},
		{text: "three", calls: glob, context: 600},
		{text: "four", calls: glob},
		{text: "five", calls: glob},
	}
	return append(ts, turn{text: "done", check: func(req *gatewayv0.InferenceRequest) error {
		msgs := req.GetMessages()
		first := messageText(msgs[0])
		if !strings.Contains(first, "count the numbers") || strings.Count(first, summaryHeading) != 1 || !strings.HasSuffix(first, "SUMMARY") {
			return fmt.Errorf("first message %q", first)
		}
		var answers []string
		for _, m := range msgs {
			if m.GetRole() == gatewayv0.Role_ROLE_ASSISTANT {
				answers = append(answers, messageText(m))
			}
		}
		if want := []string{"two", "three", "four", "five"}; !slices.Equal(answers, want) {
			return fmt.Errorf("answers %q, want %q", answers, want)
		}
		return nil
	}})
}

func compactionSandbox(t *testing.T) *sandbox {
	s := newSandbox(t, compactionScript(t)...)
	s.settings.ContextWindows = map[string]uint64{"main": 1000}
	s.settings.Compaction = CompactionPolicy{ThresholdPercent: 50, KeepTurns: 2, Model: "small"}
	return s
}

// checkCompaction checks the one compaction a run made: checkpoints
// around it, unless a crash came before the one after, its summary call,
// and its event.
func checkCompaction(t *testing.T, s *sandbox, after bool) {
	t.Helper()
	s.mu.Lock()
	var kinds []string
	for _, r := range s.records {
		kinds = append(kinds, r.Kind)
	}
	s.mu.Unlock()
	i := slices.Index(kinds, KindCompactionIntent)
	j := slices.Index(kinds, KindCompactionOutcome)
	if i < 1 || j < i || kinds[i-1] != KindCheckpoint || len(kinds) <= j+1 || after != (kinds[j+1] == KindCheckpoint) {
		t.Errorf("records %v", kinds)
	}
	if n := s.count(KindCompactionOutcome); n != 1 {
		t.Errorf("%d compactions", n)
	}
	in := s.decoded(KindCompactionIntent)[0].(*agentpb.CompactionIntent)
	if in.GetSummarizedTurns() != 1 || in.GetKeptTurns() != 2 || in.GetModel() != "small" || in.GetContextTokens() != 602 {
		t.Errorf("intent %v", in)
	}
	for _, req := range s.model.compactions {
		text := messageText(req.GetMessages()[0])
		if req.GetModel() != "small" || req.GetIntentId() != in.GetIntentId() || req.GetPurpose() != "compaction" ||
			!strings.Contains(text, "## Agent\n\none\n[call call_1_1 glob") || strings.Contains(text, "two") {
			t.Errorf("summary call %v", req)
		}
	}
	if n := s.eventsOf(func(ev *gatewayv0.ActivityEvent) bool {
		c := ev.GetCompaction()
		return c != nil && ev.GetWorker() == RootWorker && c.GetSummary() == "SUMMARY" && c.GetUsage().GetInputTokens() == 500
	}); n != 1 {
		t.Errorf("%d compaction events", n)
	}
}

// The context reaches the threshold: the older turns are summarised, and
// the agent carries on with the task, the summary and its recent turns.
func TestCompaction(t *testing.T) {
	s := compactionSandbox(t)
	s.mustFinish()
	checkCompaction(t, s, true)
	if n := len(s.model.compactions); n != 1 {
		t.Errorf("%d summary calls", n)
	}
	// The summary's call counts towards the worker's usage.
	root := s.decoded(KindCheckpoint)
	last := root[len(root)-1].(*agentpb.Checkpoint).GetWorkers()[0]
	if u := last.GetUsage(); u.GetInputTokens() < 500 || last.GetCompactions() != 1 {
		t.Errorf("usage %v, %d compactions", u, last.GetCompactions())
	}
}

// A crash during a compaction makes its call again with the same intent;
// one after it doesn't.
func TestCompactionCrash(t *testing.T) {
	for _, tc := range []struct {
		name  string
		crash func(s *sandbox)
		calls int
		after bool
	}{
		{"after the intent", func(s *sandbox) { s.crashAt = "compaction.intent" }, 1, true},
		{"before the outcome", func(s *sandbox) {
			s.crashBefore = func(kind string, _ proto.Message) bool { return kind == KindCompactionOutcome }
		}, 2, true},
		{"after the outcome", func(s *sandbox) { s.crashAt = "compaction.outcome" }, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := compactionSandbox(t)
			tc.crash(s)
			s.mustCrash()
			s.mustFinish()
			checkCompaction(t, s, tc.after)
			if n := len(s.model.compactions); n != tc.calls {
				t.Errorf("%d summary calls", n)
			}
		})
	}
}

// A context window that isn't known, or compaction turned off, never
// compacts.
func TestNoCompaction(t *testing.T) {
	glob := [][2]string{{"glob", `{"pattern":"*.none"}`}}
	for _, setup := range []func(s *sandbox){
		func(s *sandbox) { s.settings.ContextWindows = map[string]uint64{"other": 1000} },
		func(s *sandbox) {
			s.settings.ContextWindows = map[string]uint64{"main": 1000}
			s.settings.Compaction.Off = true
		},
	} {
		s := newSandbox(t, turn{calls: glob, context: 990}, turn{calls: glob, context: 990}, turn{text: "done"})
		setup(s)
		s.mustFinish()
		if n := s.count(KindCompactionIntent); n != 0 {
			t.Errorf("%d compactions", n)
		}
	}
}

func TestCompactionCut(t *testing.T) {
	u := func(s string) *gatewayv0.Message { return userText(s) }
	a := func(s string) *gatewayv0.Message {
		return &gatewayv0.Message{Role: gatewayv0.Role_ROLE_ASSISTANT, Parts: []*gatewayv0.Part{{Part: &gatewayv0.Part_Text{Text: s}}}}
	}
	msgs := []*gatewayv0.Message{u("task"), a("1"), u("r1"), a("2"), u("r2"), a("3"), u("r3")}
	for _, tc := range []struct {
		keep, at, summarized, kept int
	}{
		{1, 5, 2, 1}, {2, 3, 1, 2}, {4, 3, 1, 2},
	} {
		at, summarized, kept := compactionCut(msgs, tc.keep)
		if at != tc.at || summarized != tc.summarized || kept != tc.kept {
			t.Errorf("keep %d: cut at %d, %d summarised, %d kept", tc.keep, at, summarized, kept)
		}
	}
	if at, _, _ := compactionCut(msgs[:3], 4); at != 0 {
		t.Errorf("one turn: cut at %d", at)
	}

	// A second compaction replaces the first's summary.
	w := &agentpb.WorkerState{Messages: msgs, Usage: &gatewayv0.Usage{}}
	for i, at := range []uint32{3, 3} {
		id := fmt.Sprint(i)
		w.Compacting = &agentpb.CompactionIntent{IntentId: id, Cut: at}
		if err := compacted(w, &agentpb.CompactionOutcome{IntentId: id, Summary: "S" + id}); err != nil {
			t.Fatal(err)
		}
	}
	if first := messageText(w.Messages[0]); first != "task\n"+summaryHeading+"S1" || len(w.Messages) != 3 || w.GetCompactions() != 2 {
		t.Errorf("after two compactions: %q, %d messages", first, len(w.Messages))
	}
	if err := compacted(w, &agentpb.CompactionOutcome{IntentId: "x"}); err == nil {
		t.Error("an outcome without its intent")
	}
}

// The transcript cuts long tool results short and leaves out the oldest
// messages past its limit, keeping the first.
func TestTranscript(t *testing.T) {
	big := strings.Repeat("x", 3*maxTranscriptPart)
	msgs := []*gatewayv0.Message{userText("the task")}
	for i := range 100 {
		msgs = append(msgs,
			&gatewayv0.Message{Role: gatewayv0.Role_ROLE_ASSISTANT, Parts: []*gatewayv0.Part{{Part: &gatewayv0.Part_ToolCall{ToolCall: &gatewayv0.ToolCall{Id: fmt.Sprint(i), Name: "read_file", Input: "{}"}}}}},
			&gatewayv0.Message{Role: gatewayv0.Role_ROLE_USER, Parts: []*gatewayv0.Part{{Part: &gatewayv0.Part_ToolResult{ToolResult: &gatewayv0.ToolResult{CallId: fmt.Sprint(i), Content: big}}}}})
	}
	tr := transcript(msgs)
	if len(tr) > maxTranscript || !strings.HasPrefix(tr, "## User\n\nthe task\n") || !strings.Contains(tr, "earlier messages left out]") ||
		!strings.Contains(tr, "[result of 99: x") || strings.Contains(tr, "[result of 0:") {
		t.Errorf("transcript of %d bytes:\n%.300s", len(tr), tr)
	}
}
