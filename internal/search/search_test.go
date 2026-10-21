package search

import (
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

var t0 = time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)

func events() []*gatewayv0.ActivityEvent {
	at := func(s int) *timestamppb.Timestamp { return timestamppb.New(t0.Add(time.Duration(s) * time.Second)) }
	return []*gatewayv0.ActivityEvent{
		{Id: "1.1", Time: at(0), Body: &gatewayv0.ActivityEvent_Started{Started: &gatewayv0.SessionStarted{ProfileId: "coding"}}},
		{Id: "1.2", Time: at(10), Worker: "root", Body: &gatewayv0.ActivityEvent_ModelTurn{ModelTurn: &gatewayv0.ModelTurn{
			Text: "Reading the Sales file.", ToolCalls: []string{"read_file"}}}},
		{Id: "1.3", Time: at(20), Worker: "root", Body: &gatewayv0.ActivityEvent_ToolStarted{ToolStarted: &gatewayv0.ToolStarted{
			Tool: "read_file", Input: `{"path":"sales.csv"}`}}},
		{Id: "1.4", Time: at(30), Worker: "root.1", Body: &gatewayv0.ActivityEvent_ToolFinished{ToolFinished: &gatewayv0.ToolFinished{
			Tool: "shell", Output: "total: 43300 EUR"}}},
		{Id: "1.5", Time: at(40), Body: &gatewayv0.ActivityEvent_Steer{Steer: &gatewayv0.SteerNote{Note: "quote in EUR", Actor: "ann"}}},
		{Id: "1.6", Time: at(50), Body: &gatewayv0.ActivityEvent_SessionInfo{SessionInfo: &gatewayv0.SessionInfo{
			Grants: []*gatewayv0.ToolGrant{{Name: "shell", Settings: map[string]string{"timeout": "5m"}}}}}},
	}
}

func matches(f *Filter) []string {
	var out []string
	for _, ev := range events() {
		if f.Match(ev) {
			out = append(out, ev.GetId())
		}
	}
	return out
}

func TestFilter(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    Filter
		want []string
	}{
		{"everything", Filter{}, []string{"1.1", "1.2", "1.3", "1.4", "1.5", "1.6"}},
		{"text, any case", Filter{Text: "sales"}, []string{"1.2", "1.3"}},
		{"text in nested messages and maps", Filter{Text: "5m"}, []string{"1.6"}},
		{"text in a list", Filter{Text: "read_file"}, []string{"1.2", "1.3"}},
		{"a worker by text", Filter{Text: "root.1"}, []string{"1.4"}},
		{"regex", Filter{Regexp: regexp.MustCompile(`\d+ EUR`)}, []string{"1.4"}},
		{"kinds", Filter{Kinds: []string{"steer", "started"}}, []string{"1.1", "1.5"}},
		{"worker", Filter{Worker: "root"}, []string{"1.2", "1.3"}},
		{"tool", Filter{Tool: "shell"}, []string{"1.4"}},
		{"since", Filter{Since: t0.Add(40 * time.Second)}, []string{"1.5", "1.6"}},
		{"until", Filter{Until: t0.Add(10 * time.Second)}, []string{"1.1", "1.2"}},
		{"all together", Filter{Text: "eur", Kinds: []string{"tool_finished", "steer"}, Since: t0.Add(35 * time.Second)}, []string{"1.5"}},
	} {
		if got := matches(&tc.f); !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestKinds(t *testing.T) {
	k := Kinds()
	if !slices.Contains(k, "model_turn") || !slices.Contains(k, "compaction") || slices.Contains(k, "unknown") {
		t.Errorf("kinds %v", k)
	}
	if got := Kind(&gatewayv0.ActivityEvent{}); got != "unknown" {
		t.Errorf("an empty event's kind: %s", got)
	}
	if text := Text(events()[3]); !strings.Contains(text, "root.1") || !strings.Contains(text, "43300") {
		t.Errorf("text %q", text)
	}
}
